// Package core 实现 CrazyGet 的下载核心：任务模型、调度与持久化。
package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

// TaskKind 任务协议类型。
type TaskKind string

const (
	KindHTTP TaskKind = "http"
	KindFTP  TaskKind = "ftp"
	KindBT   TaskKind = "bt"
)

// TaskStatus 任务状态。
type TaskStatus string

const (
	StatusPending   TaskStatus = "等待中"
	StatusRunning   TaskStatus = "下载中"
	StatusMetadata  TaskStatus = "获取元数据" // BT 拉取种子信息阶段
	StatusPaused    TaskStatus = "已暂停"
	StatusCompleted TaskStatus = "已完成"
	StatusError     TaskStatus = "失败"
)

// Segment 表示一个分段（闭区间 [Start, End]），Done 为该段已下载字节数。
// HTTP/FTP 多线程断点续传的基本单元。
type Segment struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	Done  int64 `json:"done"`
}

// Len 返回分段总长度。
func (s Segment) Len() int64 { return s.End - s.Start + 1 }

// Task 一个下载任务。
type Task struct {
	ID         string     `json:"id"`
	Kind       TaskKind   `json:"kind"`
	URL        string     `json:"url"` // http(s)/ftp(s) 链接、magnet: 链接或本地 .torrent 路径
	Name       string     `json:"name"`
	SaveDir    string     `json:"saveDir"`
	FilePath   string     `json:"filePath"`
	Size       int64      `json:"size"` // -1 表示未知
	Completed  int64      `json:"completed"`
	Status     TaskStatus `json:"status"`
	ErrMsg     string     `json:"errMsg,omitempty"`
	Threads    int        `json:"threads"`
	CreatedAt  time.Time  `json:"createdAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`

	// HTTP/FTP 断点续传状态
	AcceptRanges bool      `json:"acceptRanges"`
	ETag         string    `json:"etag,omitempty"`
	Segments     []Segment `json:"segments,omitempty"`

	// BT 专属
	InfoHash   string `json:"infoHash,omitempty"`
	MetadataOK bool   `json:"metadataOk,omitempty"`

	// 运行时（不持久化到配置，speed 仅供前端展示）
	Speed  int64 `json:"speed,omitempty"` // 字节/秒
	cancel context.CancelFunc
	done   chan struct{}
}

// newID 生成随机任务 ID。
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// newTask 构造一个空运行时任务。
func newTask() *Task {
	return &Task{done: make(chan struct{})}
}
