package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const httpUserAgent = "CrazyGet/0.2"

// ErrPaused 表示任务因暂停/退出而中止（非真错误）。
var ErrPaused = errors.New("paused")

// ErrNoResume 表示服务器不支持断点续传。
var ErrNoResume = errors.New("server does not support resume")

type probeResult struct {
	name         string
	size         int64 // -1 未知
	acceptRanges bool
	etag         string
}

var (
	reFilenameStar = regexp.MustCompile(`filename\*=UTF-8''([^;]+)`)
	reFilename     = regexp.MustCompile(`filename="?([^";]+)"?`)
)

// sanitizeName 清理文件名中的非法字符。
func sanitizeName(n string) string {
	n = strings.TrimSpace(n)
	n = strings.Map(func(r rune) rune {
		switch r {
		case '\\', '/', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		return r
	}, n)
	if n == "" {
		n = "download"
	}
	return n
}

// filenameFromHeaders 优先取 Content-Disposition，其次 URL 路径。
func filenameFromHeaders(h http.Header, rawurl string) string {
	if cd := h.Get("Content-Disposition"); cd != "" {
		if m := reFilenameStar.FindStringSubmatch(cd); m != nil {
			if unq, err := url.QueryUnescape(m[1]); err == nil && unq != "" {
				return sanitizeName(unq)
			}
		}
		if m := reFilename.FindStringSubmatch(cd); m != nil {
			return sanitizeName(m[1])
		}
	}
	if u, err := url.Parse(rawurl); err == nil {
		p := path.Base(u.Path)
		if p != "" && p != "." && p != "/" {
			if un, err := url.PathUnescape(p); err == nil && un != "" {
				return sanitizeName(un)
			}
			return sanitizeName(p)
		}
	}
	return "download_" + time.Now().Format("20060102150405")
}

// newHTTPClient 带连接/响应头超时的客户端（正文不设超时，适合大文件）。
func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConnsPerHost:   8,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
	}
}

// probeHTTP 探测 URL：文件名、大小、Range 支持、ETag。
func probeHTTP(rawurl string) (probeResult, error) {
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	req, err := http.NewRequest("GET", rawurl, nil)
	if err != nil {
		return probeResult{}, err
	}
	req.Header.Set("User-Agent", httpUserAgent)
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		return probeResult{}, err
	}
	defer resp.Body.Close()

	res := probeResult{name: filenameFromHeaders(resp.Header, rawurl), size: -1}
	switch resp.StatusCode {
	case http.StatusPartialContent:
		cr := resp.Header.Get("Content-Range") // 形如 bytes 0-0/12345
		if i := strings.LastIndex(cr, "/"); i >= 0 {
			if total, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
				res.size = total
				res.acceptRanges = true
			}
		}
	case http.StatusOK:
		if cl := resp.Header.Get("Content-Length"); cl != "" {
			if n, err := strconv.ParseInt(cl, 10, 64); err == nil && n > 0 {
				res.size = n
			}
		}
	default:
		return res, fmt.Errorf("服务器响应 %d", resp.StatusCode)
	}
	res.etag = resp.Header.Get("ETag")
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1))
	return res, nil
}

// splitSegments 把文件均匀切分为不超过 threads 段（每段至少 1MB）。
func splitSegments(size int64, threads int) []Segment {
	if threads < 1 {
		threads = 1
	}
	if threads > 32 {
		threads = 32
	}
	const minSeg = int64(1 << 20)
	n := threads
	if size/minSeg < int64(n) {
		n = int(size / minSeg)
		if n < 1 {
			n = 1
		}
	}
	segs := make([]Segment, 0, n)
	chunk := size / int64(n)
	var start int64
	for i := 0; i < n; i++ {
		end := start + chunk - 1
		if i == n-1 {
			end = size - 1
		}
		segs = append(segs, Segment{Start: start, End: end})
		start = end + 1
	}
	return segs
}

func sleepBackoff(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// runHTTP 执行 HTTP(S) 下载：探测 → 分段多线程（支持续传）或单流降级。
func (m *Manager) runHTTP(ctx context.Context, t *Task) error {
	pr, err := probeHTTP(t.URL)
	if err != nil {
		return err
	}
	m.mu.Lock()
	// 续传一致性：服务器内容变化则重置
	if len(t.Segments) > 0 && t.ETag != "" && pr.etag != "" && pr.etag != t.ETag {
		t.Segments = nil
		t.AcceptRanges = false
	}
	if len(t.Segments) == 0 {
		t.Size = pr.size
		t.AcceptRanges = pr.acceptRanges
		t.ETag = pr.etag
	}
	if t.FilePath == "" {
		t.Name = uniquePath(pr.name, t.SaveDir)
		t.FilePath = joinPath(t.SaveDir, t.Name)
	}
	m.mu.Unlock()
	atomic.StoreInt64(&t.Completed, sumDone(t.Segments))

	if t.Size == 0 { // 空文件
		f, err := os.Create(t.FilePath)
		if err != nil {
			return err
		}
		_ = f.Close()
		return nil
	}
	if t.AcceptRanges && t.Size > 0 {
		return m.httpSegmented(ctx, t)
	}
	return m.httpSingle(ctx, t)
}

func sumDone(segs []Segment) int64 {
	var n int64
	for _, s := range segs {
		n += s.Done
	}
	return n
}

// httpSegmented 多线程分段下载。
func (m *Manager) httpSegmented(ctx context.Context, t *Task) error {
	m.mu.Lock()
	if len(t.Segments) == 0 {
		t.Segments = splitSegments(t.Size, t.Threads)
	}
	segs := make([]*Segment, 0, len(t.Segments))
	for i := range t.Segments {
		segs = append(segs, &t.Segments[i])
	}
	saveDir := t.SaveDir
	m.mu.Unlock()
	_ = os.MkdirAll(saveDir, 0o755)

	f, err := os.OpenFile(t.FilePath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() != t.Size {
		if err := f.Truncate(t.Size); err != nil {
			return err
		}
	}

	var (
		wg    sync.WaitGroup
		once  sync.Once
		first atomic.Value // error
	)
	for _, seg := range segs {
		if seg.Done >= seg.Len() {
			continue // 该段已完成
		}
		wg.Add(1)
		go func(seg *Segment) {
			defer wg.Done()
			if err := httpWorker(ctx, t, seg, f); err != nil {
				once.Do(func() { first.Store(err) })
			}
		}(seg)
	}
	wg.Wait()

	if v := first.Load(); v != nil {
		if ctx.Err() != nil {
			return ErrPaused
		}
		if errors.Is(v.(error), ErrPaused) {
			return ErrPaused
		}
		return v.(error)
	}
	if ctx.Err() != nil {
		return ErrPaused
	}
	if done := sumDone(t.Segments); t.Size > 0 && done < t.Size {
		return fmt.Errorf("下载不完整 %d/%d 字节（服务器多次中断）", done, t.Size)
	}
	return nil
}

// httpWorker 单个分段 worker：Range 请求循环读取，断线自动重试续传。
func httpWorker(ctx context.Context, t *Task, seg *Segment, f *os.File) error {
	client := newHTTPClient()
	buf := make([]byte, 128*1024)
	backoff := 1 * time.Second
	const maxRetry = 6

	for attempt := 0; attempt < maxRetry; attempt++ {
		if ctx.Err() != nil {
			return ErrPaused
		}
		remain := seg.Len() - seg.Done
		if remain <= 0 {
			return nil
		}
		req, err := http.NewRequestWithContext(ctx, "GET", t.URL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", httpUserAgent)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", seg.Start+seg.Done, seg.End))

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ErrPaused
			}
			sleepBackoff(ctx, backoff)
			backoff *= 2
			continue
		}

		switch resp.StatusCode {
		case http.StatusPartialContent: // 正常续传
		case http.StatusOK:
			// 服务器忽略 Range 返回全量：仅当从段首(0偏移)开始才可用，且需截断读取
			if seg.Start+seg.Done != 0 {
				resp.Body.Close()
				return ErrNoResume
			}
		default:
			e := fmt.Errorf("服务器响应 %d", resp.StatusCode)
			resp.Body.Close()
			return e
		}

		var body io.Reader = resp.Body
		if resp.StatusCode == http.StatusOK {
			body = io.LimitReader(resp.Body, remain)
		}

		offset := seg.Start + seg.Done
		segLen := seg.Len()
		var rerr error
		for {
			if ctx.Err() != nil {
				resp.Body.Close()
				return ErrPaused
			}
			n, err := body.Read(buf)
			if n > 0 {
				// 防御 200 全量响应超出段范围
				room := segLen - seg.Done
				if room < int64(n) {
					n = int(room)
				}
				wn, werr := f.WriteAt(buf[:n], offset)
				if werr != nil {
					resp.Body.Close()
					return werr
				}
				offset += int64(wn)
				seg.Done += int64(wn)
				atomic.AddInt64(&t.Completed, int64(wn))
			}
			if err == io.EOF || (resp.StatusCode == http.StatusOK && seg.Done >= segLen) {
				resp.Body.Close()
				return nil
			}
			if err != nil {
				rerr = err
				break
			}
		}
		resp.Body.Close()
		if ctx.Err() != nil {
			return ErrPaused
		}
		_ = rerr // 网络中断，退避重试
		sleepBackoff(ctx, backoff)
		backoff *= 2
	}
	return fmt.Errorf("分段重试 %d 次仍失败", maxRetry)
}

// httpSingle 单流下载（服务器不支持 Range 时的降级路径，无法续传）。
func (m *Manager) httpSingle(ctx context.Context, t *Task) error {
	_ = os.MkdirAll(t.SaveDir, 0o755)
	f, err := os.Create(t.FilePath)
	if err != nil {
		return err
	}
	defer f.Close()

	client := newHTTPClient()
	req, err := http.NewRequestWithContext(ctx, "GET", t.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", httpUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ErrPaused
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("服务器响应 %d", resp.StatusCode)
	}

	buf := make([]byte, 128*1024)
	for {
		if ctx.Err() != nil {
			return ErrPaused
		}
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			wn, werr := f.Write(buf[:n])
			if werr != nil {
				return werr
			}
			atomic.AddInt64(&t.Completed, int64(wn))
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return ErrPaused
			}
			return rerr
		}
	}
}
