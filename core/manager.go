package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config 用户设置。
type Config struct {
	SaveDir              string `json:"saveDir"`
	MaxConcurrent        int    `json:"maxConcurrent"`        // 同时下载任务数
	Threads              int    `json:"threads"`              // 每任务线程数
	Theme                string `json:"theme"`                // dark / light
	BTDownloadKB         int64  `json:"btDownloadKB"`         // BT 下载限速 KB/s，0=不限
	BTUploadKB           int64  `json:"btUploadKB"`           // BT 上传限速 KB/s，0=不限
	DisableClipboardWatch bool  `json:"disableClipboardWatch"` // 关闭剪贴板监听（默认开启）
}

// DefaultConfig 默认设置。
func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		SaveDir:       filepath.Join(home, "Downloads", "CrazyGet"),
		MaxConcurrent: 3,
		Threads:       8,
		Theme:         "dark",
	}
}

// HasURL 判断是否已存在相同 URL 的任务。
func (m *Manager) HasURL(rawURL string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, t := range m.tasks {
		if t.URL == rawURL {
			return true
		}
	}
	return false
}

// Manager 任务管理器：调度、速度统计、持久化。
type Manager struct {
	mu        sync.RWMutex
	cfg       Config
	tasks     []*Task
	cfgPath   string
	tasksPath string

	bt       *BTClient
	saveMu   sync.Mutex
	lastTick map[string][]progressSample // 每任务最近采样（滑动窗口算速度）
	lastSave time.Time
	closing  bool
}

type progressSample struct {
	at   time.Time
	done int64
}

// appDataDir 配置与任务数据目录（%APPDATA%\CrazyGet）。
func appDataDir() string {
	base := os.Getenv("APPDATA")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, "AppData", "Roaming")
	}
	return filepath.Join(base, "CrazyGet")
}

// Load 初始化管理器：读配置、恢复任务、启动 BT 引擎与调度。
func Load() (*Manager, error) {
	dir := appDataDir()
	if err := os.MkdirAll(filepath.Join(dir, "torrents"), 0o755); err != nil {
		return nil, err
	}
	m := &Manager{
		cfgPath:   filepath.Join(dir, "config.json"),
		tasksPath: filepath.Join(dir, "tasks.json"),
		lastTick:  map[string][]progressSample{},
	}
	m.cfg = DefaultConfig()
	if b, err := os.ReadFile(m.cfgPath); err == nil {
		_ = json.Unmarshal(b, &m.cfg)
		m.cfg = sanitizeConfig(m.cfg)
	}
	if b, err := os.ReadFile(m.tasksPath); err == nil {
		var saved []*Task
		if err := json.Unmarshal(b, &saved); err == nil {
			for _, t := range saved {
				nt := newTask()
				*nt = *t
				nt.done = make(chan struct{})
				nt.cancel = nil
				nt.Speed = 0
				// 上次退出时未完成的任务自动恢复
				if nt.Status == StatusRunning || nt.Status == StatusMetadata || nt.Status == StatusPending {
					nt.Status = StatusPending
				}
				m.tasks = append(m.tasks, nt)
			}
		}
	}

	bt, err := NewBTClient(m.cfg.SaveDir, filepath.Join(dir, "torrents"), m.cfg.BTDownloadKB, m.cfg.BTUploadKB)
	if err != nil {
		// BT 引擎失败不阻塞其他功能
		m.bt = nil
	} else {
		m.bt = bt
	}

	m.StartTicker()
	m.schedule()
	return m, nil
}

func sanitizeConfig(c Config) Config {
	if c.SaveDir == "" {
		c = DefaultConfig()
	}
	if c.MaxConcurrent < 1 {
		c.MaxConcurrent = 1
	}
	if c.MaxConcurrent > 8 {
		c.MaxConcurrent = 8
	}
	if c.Threads < 1 {
		c.Threads = 1
	}
	if c.Threads > 32 {
		c.Threads = 32
	}
	if c.Theme != "light" {
		c.Theme = "dark"
	}
	return c
}

// Config 返回设置副本。
func (m *Manager) Config() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// SetConfig 更新设置并持久化。
func (m *Manager) SetConfig(c Config) {
	m.mu.Lock()
	m.cfg = sanitizeConfig(c)
	cfg := m.cfg
	m.mu.Unlock()
	m.saveMu.Lock()
	b, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(m.cfgPath, b, 0o644)
	m.saveMu.Unlock()
}

// Snapshot 任务快照（UI 用）。
func (m *Manager) Snapshot() []Task {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Task, len(m.tasks))
	for i, t := range m.tasks {
		c := *t
		c.Completed = atomic.LoadInt64(&t.Completed)
		out[i] = c
	}
	return out
}

// FindTask 按 ID 查任务（返回只读快照）。
func (m *Manager) FindTask(id string) (Task, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, t := range m.tasks {
		if t.ID == id {
			c := *t
			c.Completed = atomic.LoadInt64(&t.Completed)
			return c, true
		}
	}
	return Task{}, false
}

// Add 新建任务并调度。
func (m *Manager) Add(rawURL, saveDir string) (*Task, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("请输入下载链接")
	}
	kind := DetectKind(rawURL)
	if kind == "" {
		return nil, fmt.Errorf("无法识别的链接（支持 http/https/ftp/ftps/磁力/.torrent 文件）")
	}
	if saveDir == "" {
		saveDir = m.Config().SaveDir
	}
	m.mu.Lock()
	t := newTask()
	t.ID = newID()
	t.Kind = kind
	t.URL = rawURL
	t.SaveDir = saveDir
	t.Size = -1
	t.Status = StatusPending
	t.Threads = m.cfg.Threads
	t.CreatedAt = time.Now()
	t.Name = "(解析中)"
	m.tasks = append(m.tasks, t)
	m.mu.Unlock()
	m.saveTasks()
	m.schedule()
	return t, nil
}

// SetThreads 设置任务线程数（对尚未开始分段的任务生效）。
func (m *Manager) SetThreads(id string, threads int) {
	if threads < 1 || threads > 32 {
		return
	}
	m.mu.Lock()
	for _, t := range m.tasks {
		if t.ID == id && len(t.Segments) == 0 {
			t.Threads = threads
		}
	}
	m.mu.Unlock()
}

// Start 恢复任务（等待中/暂停/失败）。
func (m *Manager) Start(id string) {
	m.mu.Lock()
	for _, t := range m.tasks {
		if t.ID == id {
			switch t.Status {
			case StatusPending:
			case StatusPaused, StatusError:
				t.Status = StatusPending
				t.ErrMsg = ""
			default:
				m.mu.Unlock()
				return
			}
		}
	}
	m.mu.Unlock()
	m.schedule()
}

// StartAll 恢复全部可启动任务。
func (m *Manager) StartAll() {
	m.mu.Lock()
	for _, t := range m.tasks {
		if t.Status == StatusPaused || t.Status == StatusError {
			t.Status = StatusPending
			t.ErrMsg = ""
		}
	}
	m.mu.Unlock()
	m.schedule()
}

// Pause 暂停任务。
func (m *Manager) Pause(id string) {
	m.mu.Lock()
	for _, t := range m.tasks {
		if t.ID == id && (t.Status == StatusRunning || t.Status == StatusMetadata) {
			if t.cancel != nil {
				t.cancel()
			}
		}
	}
	m.mu.Unlock()
}

// PauseAll 暂停全部运行中任务。
func (m *Manager) PauseAll() {
	m.mu.Lock()
	for _, t := range m.tasks {
		if t.Status == StatusRunning || t.Status == StatusMetadata {
			if t.cancel != nil {
				t.cancel()
			}
		}
	}
	m.mu.Unlock()
}

// Remove 移除任务；deleteFiles 时同时删除已下载数据。
func (m *Manager) Remove(id string, deleteFiles bool) {
	m.mu.Lock()
	idx := -1
	var t *Task
	for i, tt := range m.tasks {
		if tt.ID == id {
			idx, t = i, tt
			break
		}
	}
	if idx < 0 {
		m.mu.Unlock()
		return
	}
	running := t.Status == StatusRunning || t.Status == StatusMetadata
	if running && t.cancel != nil {
		t.cancel()
	}
	infoHash := t.InfoHash
	filePath, saveDir, name := t.FilePath, t.SaveDir, t.Name
	m.tasks = append(m.tasks[:idx], m.tasks[idx+1:]...)
	m.mu.Unlock()

	if running && t.done != nil {
		select { // 等待 worker 释放文件句柄（Windows 文件锁）
		case <-t.done:
		case <-time.After(5 * time.Second):
		}
	}
	if m.bt != nil && infoHash != "" {
		m.bt.Drop(infoHash)
	}
	if deleteFiles {
		if filePath != "" {
			_ = os.Remove(filePath)
		}
		if name != "" && name != "(解析中)" {
			_ = os.RemoveAll(filepath.Join(saveDir, name))
		}
		if infoHash != "" && m.bt != nil {
			_ = os.Remove(filepath.Join(m.bt.cacheDir, infoHash+".torrent"))
		}
	}
	m.saveTasks()
}

// ClearCompleted 清除已完成任务（保留文件）。
func (m *Manager) ClearCompleted() {
	m.mu.Lock()
	kept := m.tasks[:0]
	for _, t := range m.tasks {
		if t.Status != StatusCompleted {
			kept = append(kept, t)
		}
	}
	m.tasks = kept
	m.mu.Unlock()
	m.saveTasks()
}

// schedule 调度器：补满并发槽位。
func (m *Manager) schedule() {
	m.mu.Lock()
	running := 0
	for _, t := range m.tasks {
		if t.Status == StatusRunning || t.Status == StatusMetadata {
			running++
		}
	}
	max := m.cfg.MaxConcurrent
	if max < 1 {
		max = 1
	}
	for _, t := range m.tasks {
		if running >= max {
			break
		}
		if t.Status == StatusPending {
			running++
			ctx, cancel := context.WithCancel(context.Background())
			t.cancel = cancel
			t.Status = StatusRunning
			t.ErrMsg = ""
			t.done = make(chan struct{}) // 每次调度使用新的结束信号
			go m.runTask(ctx, t)
		}
	}
	m.mu.Unlock()
}

// runTask 任务执行包装：调度引擎、归档结果。
func (m *Manager) runTask(ctx context.Context, t *Task) {
	defer close(t.done)
	var err error
	switch t.Kind {
	case KindHTTP:
		err = m.runHTTP(ctx, t)
	case KindFTP:
		err = m.runFTP(ctx, t)
	case KindBT:
		err = m.runBT(ctx, t)
	default:
		err = fmt.Errorf("未知任务类型")
	}

	m.mu.Lock()
	switch {
	case err == nil:
		t.Status = StatusCompleted
		now := time.Now()
		t.FinishedAt = &now
		if t.Size > 0 {
			atomic.StoreInt64(&t.Completed, t.Size)
		}
	case ctx.Err() != nil || errors.Is(err, ErrPaused):
		t.Status = StatusPaused
	default:
		t.Status = StatusError
		t.ErrMsg = err.Error()
	}
	t.cancel = nil
	m.mu.Unlock()
	m.saveTasks()
	m.schedule()
}

// StartTicker 启动速度统计 + 节流持久化循环。
func (m *Manager) StartTicker() {
	go func() {
		tk := time.NewTicker(500 * time.Millisecond)
		defer tk.Stop()
		for range tk.C {
			m.tick()
		}
	}()
}

const speedWindowSamples = 6 // 速度滑动窗口采样数（每 500ms 一个，覆盖 3 秒）

func (m *Manager) tick() {
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return
	}
	now := time.Now()
	dirty := false
	for _, t := range m.tasks {
		if t.Status == StatusRunning || t.Status == StatusMetadata {
			done := atomic.LoadInt64(&t.Completed)
			// 滑动窗口采样：用最近若干个采样计算平均速度，避免瞬时波动导致速度间歇归零
			samples := append(m.lastTick[t.ID], progressSample{now, done})
			if len(samples) > speedWindowSamples {
				samples = samples[len(samples)-speedWindowSamples:]
			}
			m.lastTick[t.ID] = samples
			if len(samples) >= 2 {
				oldest := samples[0]
				if dt := now.Sub(oldest.at).Seconds(); dt > 0 {
					inst := float64(done-oldest.done) / dt
					// 指数平滑：新值占 35%，保留 65% 历史惯性，显示曲线更平稳
					t.Speed = int64(inst*0.35 + float64(t.Speed)*0.65)
				}
			}
			dirty = true
		} else {
			t.Speed = 0
			delete(m.lastTick, t.ID)
		}
	}
	m.mu.Unlock()
	// 断点续传状态节流落盘（每 2 秒）
	if dirty && time.Since(m.lastSave) > 2*time.Second {
		m.lastSave = time.Now()
		m.saveTasks()
	}
}

// saveTasks 持久化任务列表（原子写）。
func (m *Manager) saveTasks() {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	m.mu.RLock()
	snap := make([]*Task, len(m.tasks))
	for i, t := range m.tasks {
		c := *t
		c.Completed = atomic.LoadInt64(&t.Completed)
		snap[i] = &c
	}
	m.mu.RUnlock()
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return
	}
	tmp := m.tasksPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, m.tasksPath)
}

// Close 优雅退出：暂停全部任务、关闭 BT 引擎、落盘。
func (m *Manager) Close() {
	m.mu.Lock()
	m.closing = true
	var running []*Task
	for _, t := range m.tasks {
		if t.Status == StatusRunning || t.Status == StatusMetadata {
			running = append(running, t)
			if t.cancel != nil {
				t.cancel()
			}
		}
	}
	bt := m.bt
	m.mu.Unlock()

	for _, t := range running {
		select {
		case <-t.done:
		case <-time.After(3 * time.Second):
		}
	}
	if bt != nil {
		bt.Close()
	}
	m.saveTasks()
}
