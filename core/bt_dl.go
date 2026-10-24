package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"
)

// BTClient 全局共享的 BitTorrent 客户端（DHT / uTP / 多任务）。
type BTClient struct {
	cli      *torrent.Client
	cacheDir string // 元数据缓存目录（infohash.torrent）
}

// NewBTClient 初始化 BT 客户端。dlKB/ulKB 为限速（KB/s，0 表示不限）。
func NewBTClient(dataDir, cacheDir string, dlKB, ulKB int64) (*BTClient, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = dataDir
	cfg.DefaultStorage = storage.NewFile(dataDir)
	if dlKB > 0 {
		cfg.DownloadRateLimiter = rate.NewLimiter(rate.Limit(float64(dlKB)*1024), 1<<20)
	}
	if ulKB > 0 {
		cfg.UploadRateLimiter = rate.NewLimiter(rate.Limit(float64(ulKB)*1024), 1<<20)
	}
	cli, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	return &BTClient{cli: cli, cacheDir: cacheDir}, nil
}

// addTorrent 优先用本地缓存的元数据添加任务，其次 magnet / .torrent 文件。
func (b *BTClient) addTorrent(t *Task) (*torrent.Torrent, error) {
	if t.InfoHash != "" {
		cached := filepath.Join(b.cacheDir, t.InfoHash+".torrent")
		if _, err := os.Stat(cached); err == nil {
			if tt, err := b.cli.AddTorrentFromFile(cached); err == nil {
				return tt, nil
			}
		}
	}
	if strings.HasPrefix(strings.ToLower(t.URL), "magnet:") {
		return b.cli.AddMagnet(t.URL)
	}
	if _, err := os.Stat(t.URL); err == nil {
		return b.cli.AddTorrentFromFile(t.URL)
	}
	return nil, fmt.Errorf("种子文件不存在：%s", t.URL)
}

// Drop 移除指定任务（暂停/删除时），不删数据。
func (b *BTClient) Drop(infoHash string) {
	if b == nil || infoHash == "" {
		return
	}
	for _, tt := range b.cli.Torrents() {
		if tt.InfoHash().HexString() == infoHash {
			tt.Drop()
			return
		}
	}
}

// Close 关闭客户端。
func (b *BTClient) Close() {
	if b != nil {
		_ = b.cli.Close()
	}
}

const metadataTimeout = 10 * time.Minute

// runBT 执行 BT/磁力任务：拉元数据 → 校验已有数据 → 下载 → 完成监听。
func (m *Manager) runBT(ctx context.Context, t *Task) error {
	if m.bt == nil {
		return fmt.Errorf("BT 引擎未初始化")
	}
	tt, err := m.bt.addTorrent(t)
	if err != nil {
		return err
	}
	m.mu.Lock()
	t.InfoHash = tt.InfoHash().HexString()
	m.mu.Unlock()

	// 状态改为“获取元数据”（magnet 需要连 DHT/tracker）
	m.mu.Lock()
	t.Status = StatusMetadata
	m.mu.Unlock()

	select {
	case <-tt.GotInfo():
	case <-time.After(metadataTimeout):
		tt.Drop()
		return fmt.Errorf("获取种子元数据超时（做种少或网络受限，可稍后重试）")
	case <-ctx.Done():
		tt.Drop()
		return ErrPaused
	}

	m.mu.Lock()
	t.MetadataOK = true
	if t.Name == "" || t.Name == "(解析中)" {
		t.Name = sanitizeName(tt.Name())
	}
	t.Size = tt.Length()
	m.mu.Unlock()

	// 缓存元数据，供暂停/重启后快速恢复
	if b, err := bencode.Marshal(tt.Metainfo()); err == nil {
		cachePath := filepath.Join(m.bt.cacheDir, t.InfoHash+".torrent")
		_ = os.WriteFile(cachePath, b, 0o644)
	}

	// 恢复场景：先校验磁盘上已有数据
	if atomic.LoadInt64(&t.Completed) > 0 {
		tt.VerifyData()
	}
	tt.DownloadAll()

	// 监听进度直至完成或取消
	tick := time.NewTicker(600 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			atomic.StoreInt64(&t.Completed, tt.BytesCompleted())
			if t.Size > 0 && tt.BytesCompleted() >= t.Size {
				return nil
			}
		case <-ctx.Done():
			tt.Drop()
			return ErrPaused
		}
	}
}

// DetectKind 依据输入自动识别协议类型；返回空表示无法识别。
func DetectKind(input string) TaskKind {
	s := strings.TrimSpace(input)
	l := strings.ToLower(s)
	switch {
	case strings.HasPrefix(l, "magnet:"):
		return KindBT
	case strings.HasPrefix(l, "http://"), strings.HasPrefix(l, "https://"):
		return KindHTTP
	case strings.HasPrefix(l, "ftp://"), strings.HasPrefix(l, "ftps://"):
		return KindFTP
	}
	if strings.HasSuffix(l, ".torrent") {
		if _, err := os.Stat(s); err == nil {
			return KindBT
		}
	}
	// 本地 .torrent 文件（无后缀大小写差异）
	if _, err := os.Stat(s); err == nil && strings.EqualFold(filepath.Ext(s), ".torrent") {
		return KindBT
	}
	return ""
}

var _ = metainfo.ParseMagnetURI // 保留引用
var _ = errors.New
