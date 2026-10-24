package main

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"crazyget/core"

	"github.com/atotto/clipboard"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App 暴露给前端 JS 的绑定层。
type App struct {
	m   *core.Manager
	ctx context.Context
}

// linkRe 匹配可下载链接：http(s)/ftp(s) URL 或磁力链接。
var linkRe = regexp.MustCompile(`^(?:(?:https?|ftps?)://\S+|magnet:\?xt=urn:btih:\S+)$`)

// NewApp 创建绑定层。
func NewApp(m *core.Manager) *App { return &App{m: m} }

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	go a.watchClipboard()
}

func (a *App) shutdown(ctx context.Context) {
	if a.m != nil {
		a.m.Close()
	}
}

// watchClipboard 周期检查剪贴板，发现新的下载链接时通知前端。
func (a *App) watchClipboard() {
	var lastText string
	ignored := map[string]bool{}
	ticker := time.NewTicker(800 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		if a.m == nil || a.ctx == nil {
			continue
		}
		if a.m.Config().DisableClipboardWatch {
			continue
		}
		text, err := clipboard.ReadAll()
		if err != nil {
			continue
		}
		text = strings.TrimSpace(text)
		if text == "" || len(text) > 2048 || text == lastText || ignored[text] {
			continue
		}
		if !linkRe.MatchString(text) {
			continue
		}
		if a.m.HasURL(text) { // 已有相同任务则静默
			lastText = text
			continue
		}
		lastText = text
		runtime.EventsEmit(a.ctx, "clipboard-link", text)
	}
}

// IgnoreClipboard 前端忽略某链接，本次会话内不再提示。
func (a *App) IgnoreClipboard(text string) {
	if len(ignoredSet) > 50 {
		ignoredSet = map[string]bool{}
	}
	ignoredSet[text] = true
}

var ignoredSet = map[string]bool{}

// GetTasks 返回全部任务快照（前端每 400ms 轮询）。
func (a *App) GetTasks() []core.Task { return a.m.Snapshot() }

// AddTask 新建任务。返回创建的任务（含解析后的名称/大小为空，异步探测）。
func (a *App) AddTask(url, saveDir string) (core.Task, error) {
	t, err := a.m.Add(url, saveDir)
	if err != nil {
		return core.Task{}, err
	}
	return *t, nil
}

// StartTask / PauseTask 恢复与暂停。
func (a *App) StartTask(id string) { a.m.Start(id) }
func (a *App) PauseTask(id string) { a.m.Pause(id) }
func (a *App) StartAll()           { a.m.StartAll() }
func (a *App) PauseAll()           { a.m.PauseAll() }

// RemoveTask 删除任务（可选删文件）。
func (a *App) RemoveTask(id string, deleteFiles bool) { a.m.Remove(id, deleteFiles) }

// ClearCompleted 清除已完成任务。
func (a *App) ClearCompleted() { a.m.ClearCompleted() }

// GetConfig 读取设置。
func (a *App) GetConfig() core.Config { return a.m.Config() }

// SaveConfig 保存设置。
func (a *App) SaveConfig(cfg core.Config) error {
	a.m.SetConfig(cfg)
	return nil
}

// SetThreads 设置指定任务线程数。
func (a *App) SetThreads(id string, threads int) { a.m.SetThreads(id, threads) }

// PickTorrent 弹出文件对话框选择 .torrent，返回路径（取消返回空）。
func (a *App) PickTorrent() (string, error) {
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "选择种子文件",
		Filters: []runtime.FileFilter{
			{DisplayName: "种子文件 (*.torrent)", Pattern: "*.torrent"},
		},
	})
}

// PickFolder 弹出目录选择对话框。
func (a *App) PickFolder() (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title: "选择保存目录",
	})
}

// OpenFolder 在资源管理器中定位任务文件。
func (a *App) OpenFolder(taskID string) {
	t, ok := a.m.FindTask(taskID)
	if !ok {
		return
	}
	target := t.FilePath
	if t.Kind == core.KindBT && t.Name != "" && t.Name != "(解析中)" {
		target = t.FilePath
		if target == "" {
			target = t.SaveDir + "\\" + t.Name
		}
	}
	if target == "" {
		return
	}
	if _, err := os.Stat(target); err != nil {
		target = t.SaveDir
	}
	if target == "" {
		return
	}
	_ = exec.Command("explorer", "/select,"+target).Start()
}
