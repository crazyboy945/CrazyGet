package core

import (
	"encoding/json"
	"sync"
	"time"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// throttledSeeker 限速读取器（自测用）：全局互斥 + 每块延迟。
type throttledSeeker struct {
	rs     interface {
		Read(p []byte) (int, error)
		Seek(offset int64, whence int) (int64, error)
	}
	mu *sync.Mutex
}

func (t *throttledSeeker) Read(p []byte) (int, error) {
	t.mu.Lock()
	n, err := t.rs.Read(p)
	time.Sleep(18 * time.Millisecond) // 全局串行 ≈ 1.7 MB/s
	t.mu.Unlock()
	return n, err
}

func (t *throttledSeeker) Seek(offset int64, whence int) (int64, error) {
	return t.rs.Seek(offset, whence)
}
