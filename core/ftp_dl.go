package core

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ftp "github.com/jlaffaye/ftp"
)

// ftpConnect 建立一条 FTP 连接（每个下载线程独立连接）。
func ftpConnect(rawurl string) (*ftp.ServerConn, string, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, "", err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":21"
	}
	var opts []ftp.DialOption
	if u.Scheme == "ftps" {
		opts = append(opts, ftp.DialWithTLS(&tls.Config{ServerName: u.Hostname()}))
	}
	opts = append(opts, ftp.DialWithTimeout(20*time.Second))
	c, err := ftp.Dial(host, opts...)
	if err != nil {
		return nil, "", fmt.Errorf("FTP 连接失败: %w", err)
	}
	user, pass := "anonymous", "anonymous@"
	if u.User != nil {
		user = u.User.Username()
		if p, ok := u.User.Password(); ok && p != "" {
			pass = p
		}
	}
	if err := c.Login(user, pass); err != nil {
		_ = c.Quit()
		return nil, "", fmt.Errorf("FTP 登录失败: %w", err)
	}
	ftpPath := strings.TrimPrefix(u.Path, "/")
	return c, ftpPath, nil
}

// probeFTP 探测远端文件大小与路径。
func probeFTP(rawurl string) (probeResult, error) {
	c, p, err := ftpConnect(rawurl)
	if err != nil {
		return probeResult{}, err
	}
	defer c.Quit()
	if p == "" || strings.HasSuffix(p, "/") {
		return probeResult{}, fmt.Errorf("FTP 路径是目录，请指定具体文件")
	}
	res := probeResult{name: sanitizeName(path.Base(p)), size: -1}
	if entry, err := c.GetEntry(p); err == nil && entry != nil {
		res.size = int64(entry.Size)
	}
	return res, nil
}

// runFTP 执行 FTP 下载：先试多连接分段（REST 续传），失败自动降级单流。
func (m *Manager) runFTP(ctx context.Context, t *Task) error {
	pr, err := probeFTP(t.URL)
	if err != nil {
		return err
	}
	m.mu.Lock()
	if t.FilePath == "" {
		t.Name = uniquePath(pr.name, t.SaveDir)
		t.FilePath = joinPath(t.SaveDir, t.Name)
	}
	if len(t.Segments) == 0 {
		t.Size = pr.size
	}
	m.mu.Unlock()

	if t.Size == 0 {
		f, err := osCreate(t.FilePath)
		if err != nil {
			return err
		}
		_ = f.Close()
		return nil
	}

	useSegments := t.AcceptRanges && t.Size > 0 && t.Threads > 1
	if useSegments {
		err := m.ftpSegmented(ctx, t, pr)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || err == ErrPaused {
			return ErrPaused
		}
		if err == ErrNoResume {
			// 服务器不支持 REST，降级单流重来
			m.mu.Lock()
			t.Segments = nil
			t.AcceptRanges = false
			m.mu.Unlock()
			atomic.StoreInt64(&t.Completed, 0)
		} else {
			return err
		}
	}
	return m.ftpSingle(ctx, t)
}

// ftpSegmented 多连接分段下载，每段独立连接 + REST 偏移。
func (m *Manager) ftpSegmented(ctx context.Context, t *Task, pr probeResult) error {
	m.mu.Lock()
	if len(t.Segments) == 0 {
		threads := t.Threads
		if threads < 1 {
			threads = 1
		}
		if threads > 8 {
			threads = 8 // FTP 并发连接保守一点
		}
		t.Segments = splitSegments(t.Size, threads)
		t.ETag = "ftp-resume-ok"
	}
	m.mu.Unlock()
	_ = makeDirs(t.SaveDir)

	f, err := openFileRW(t.FilePath)
	if err != nil {
		return err
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() != t.Size {
		if err := f.Truncate(t.Size); err != nil {
			return err
		}
	}

	// 第一段同步执行：探测 REST 是否受支持
	first := &t.Segments[0]
	if first.Done < first.Len() {
		if err := ftpWorker(ctx, t, first, f); err != nil {
			return err // ErrNoResume → 降级；其他错误也直接交给降级逻辑
		}
	}

	var (
		wg   sync.WaitGroup
		once sync.Once
		ferr atomic.Value
	)
	for i := 1; i < len(t.Segments); i++ {
		seg := &t.Segments[i]
		if seg.Done >= seg.Len() {
			continue
		}
		wg.Add(1)
		go func(seg *Segment) {
			defer wg.Done()
			if err := ftpWorker(ctx, t, seg, f); err != nil {
				once.Do(func() { ferr.Store(err) })
			}
		}(seg)
	}
	wg.Wait()

	if v := ferr.Load(); v != nil {
		if ctx.Err() != nil || v.(error) == ErrPaused {
			return ErrPaused
		}
		if v.(error) == ErrNoResume {
			return ErrNoResume
		}
		return v.(error)
	}
	if ctx.Err() != nil {
		return ErrPaused
	}
	if done := sumDone(t.Segments); done < t.Size {
		return fmt.Errorf("下载不完整 %d/%d 字节", done, t.Size)
	}
	return nil
}

// ftpWorker 单个分段：独立 FTP 连接 + RETR with offset。
func ftpWorker(ctx context.Context, t *Task, seg *Segment, f writerAt) error {
	backoff := 1 * time.Second
	const maxRetry = 4
	for attempt := 0; attempt < maxRetry; attempt++ {
		if ctx.Err() != nil {
			return ErrPaused
		}
		remain := seg.Len() - seg.Done
		if remain <= 0 {
			return nil
		}
		c, p, err := ftpConnect(t.URL)
		if err != nil {
			sleepBackoff(ctx, backoff)
			backoff *= 2
			continue
		}
		rd, err := c.RetrFrom(p, uint64(seg.Start+seg.Done))
		if err != nil {
			_ = c.Quit()
			if attempt == 0 {
				return ErrNoResume // 大概率不支持 REST
			}
			sleepBackoff(ctx, backoff)
			backoff *= 2
			continue
		}

		buf := make([]byte, 128*1024)
		offset := seg.Start + seg.Done
		done := true
		for {
			if ctx.Err() != nil {
				_ = rd.Close()
				_ = c.Quit()
				return ErrPaused
			}
			n, rerr := rd.Read(buf)
			if n > 0 {
				wn, werr := f.WriteAt(buf[:n], offset)
				if werr != nil {
					_ = rd.Close()
					_ = c.Quit()
					return werr
				}
				offset += int64(wn)
				seg.Done += int64(wn)
				atomic.AddInt64(&t.Completed, int64(wn))
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				done = false
				break
			}
		}
		_ = rd.Close()
		_ = c.Quit()
		if done {
			return nil
		}
		if ctx.Err() != nil {
			return ErrPaused
		}
		sleepBackoff(ctx, backoff)
		backoff *= 2
	}
	return fmt.Errorf("FTP 分段重试 %d 次仍失败", maxRetry)
}

// ftpSingle 单流下载（不支持 REST 的服务器降级路径）。
func (m *Manager) ftpSingle(ctx context.Context, t *Task) error {
	_ = makeDirs(t.SaveDir)
	f, err := osCreate(t.FilePath)
	if err != nil {
		return err
	}
	defer f.Close()

	c, p, err := ftpConnect(t.URL)
	if err != nil {
		return err
	}
	defer c.Quit()

	rd, err := c.Retr(p)
	if err != nil {
		return fmt.Errorf("FTP RETR 失败: %w", err)
	}
	defer rd.Close()

	buf := make([]byte, 128*1024)
	for {
		if ctx.Err() != nil {
			return ErrPaused
		}
		n, rerr := rd.Read(buf)
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

// 并发安全的 WaitGroup 封装（预留）。
var _ = sync.WaitGroup{}
