package core

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SelfTest 核心功能自测：HTTP 单线程/多线程/断点续传/无Range降级/空文件/持久化。
// 返回 0 表示全部通过。
func SelfTest() int {
	tmp, err := os.MkdirTemp("", "crazyget-selftest")
	if err != nil {
		fmt.Println("创建临时目录失败:", err)
		return 1
	}
	defer os.RemoveAll(tmp)

	// 8MB 随机数据
	data := make([]byte, 8<<20)
	if _, err := rand.Read(data); err != nil {
		fmt.Println("生成测试数据失败:", err)
		return 1
	}
	want := sha256.Sum256(data)
	emptyHash := sha256.Sum256(nil)

	var slowMu sync.Mutex
	slowRS := func(r io.ReadSeeker) io.ReadSeeker {
		return &throttledSeeker{rs: r, mu: &slowMu}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/file.bin", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "file.bin", time.Now(), bytes.NewReader(data))
	})
	mux.HandleFunc("/slow.bin", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "slow.bin", time.Now(), slowRS(bytes.NewReader(data)))
	})
	mux.HandleFunc("/norange.bin", func(w http.ResponseWriter, r *http.Request) {
		// 无视 Range，总是全量 200
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})
	mux.HandleFunc("/empty.bin", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	tests := []struct {
		name string
		fn   func() error
	}{
		{"HTTP 单线程下载", func() error {
			return runDownloadTest(tmp, srv.URL+"/file.bin", 1, want)
		}},
		{"HTTP 多线程(8线程)", func() error {
			return runDownloadTest(tmp, srv.URL+"/file.bin", 8, want)
		}},
		{"HTTP 断点续传(暂停/恢复)", func() error {
			return runResumeTest(tmp, srv.URL+"/slow.bin", want)
		}},
		{"无 Range 服务器降级单流", func() error {
			return runDownloadTest(tmp, srv.URL+"/norange.bin", 8, want)
		}},
		{"空文件下载", func() error {
			return runDownloadTest(tmp, srv.URL+"/empty.bin", 4, emptyHash)
		}},
		{"任务持久化", func() error {
			return runPersistTest(tmp, srv.URL+"/file.bin")
		}},
	}
	fmt.Println("CrazyGet 核心自测")
	fmt.Println("----------------------------------------------")
	pass := 0
	for i, tt := range tests {
		fmt.Printf("[%d/%d] %-34s", i+1, len(tests), tt.name)
		if err := safeCall(tt.fn); err != nil {
			fmt.Printf(" FAIL: %v\n", err)
		} else {
			fmt.Println(" PASS")
			pass++
		}
	}
	fmt.Println("FTP/BT/磁力  需要真实服务器与外网环境，请手动验证（跳过）")
	fmt.Println("----------------------------------------------")
	fmt.Printf("结果: %d/%d 通过\n", pass, len(tests))
	if pass == len(tests) {
		return 0
	}
	return 1
}

func safeCall(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn()
}

// testManager 构造测试用管理器。
func testManager(dir string, threads, conc int) *Manager {
	return &Manager{
		cfg: Config{
			SaveDir:       dir,
			MaxConcurrent: conc,
			Threads:       threads,
			Theme:         "dark",
		},
		cfgPath:   filepath.Join(dir, "config.json"),
		tasksPath: filepath.Join(dir, "tasks.json"),
		lastTick:  map[string][]progressSample{},
	}
}

func runDownloadTest(dir, url string, threads int, want [32]byte) error {
	m := testManager(dir, threads, 4)
	t, err := m.Add(url, dir)
	if err != nil {
		return err
	}
	if err := waitTask(m, t.ID, 60*time.Second, StatusCompleted); err != nil {
		return err
	}
	if strings.HasSuffix(url, "empty.bin") {
		return nil // 空文件无需校验内容
	}
	return checkFile(filepath.Join(dir, t.Name), want)
}

func runResumeTest(dir, url string, want [32]byte) error {
	m := testManager(dir, 8, 4)
	t, err := m.Add(url, dir)
	if err != nil {
		return err
	}
	// 等待下载至少 1.5MB
	deadline := time.Now().Add(60 * time.Second)
	for {
		snap, ok := m.FindTask(t.ID)
		if !ok {
			return fmt.Errorf("任务丢失")
		}
		if snap.Completed > 1500*1024 {
			break
		}
		if snap.Status == StatusError {
			return fmt.Errorf("任务失败: %s", snap.ErrMsg)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待进度超时")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// 暂停
	m.Pause(t.ID)
	if err := waitTask(m, t.ID, 20*time.Second, StatusPaused); err != nil {
		return fmt.Errorf("暂停失败: %w", err)
	}
	// 恢复
	m.Start(t.ID)
	if err := waitTask(m, t.ID, 120*time.Second, StatusCompleted); err != nil {
		return err
	}
	snap, _ := m.FindTask(t.ID)
	if len(snap.Segments) == 0 {
		return fmt.Errorf("未使用分段续传")
	}
	return checkFile(filepath.Join(dir, snap.Name), want)
}

func runPersistTest(dir, url string) error {
	m := testManager(dir, 4, 1)
	if _, err := m.Add(url, dir); err != nil {
		return err
	}
	tasksPath := filepath.Join(dir, "tasks.json")
	if _, err := os.Stat(tasksPath); err != nil {
		return fmt.Errorf("tasks.json 未生成: %w", err)
	}
	b, err := os.ReadFile(tasksPath)
	if err != nil {
		return err
	}
	var saved []*Task
	if err := jsonUnmarshal(b, &saved); err != nil {
		return err
	}
	if len(saved) == 0 {
		return fmt.Errorf("持久化为空")
	}
	if saved[0].URL != url {
		return fmt.Errorf("URL 不一致: %s", saved[0].URL)
	}
	return nil
}

func waitTask(m *Manager, id string, timeout time.Duration, want TaskStatus) error {
	deadline := time.Now().Add(timeout)
	for {
		snap, ok := m.FindTask(id)
		if !ok {
			return fmt.Errorf("任务丢失")
		}
		if snap.Status == want {
			return nil
		}
		if snap.Status == StatusError {
			return fmt.Errorf("任务失败: %s", snap.ErrMsg)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待 %s 超时（当前 %s, %d/%d）", want, snap.Status, snap.Completed, snap.Size)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func checkFile(path string, want [32]byte) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	got := sha256.Sum256(b)
	if got != want {
		return fmt.Errorf("文件校验不一致 (len=%d)", len(b))
	}
	return nil
}
