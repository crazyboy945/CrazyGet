package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// joinPath 目录 + 文件名。
func joinPath(dir, name string) string { return filepath.Join(dir, name) }

// makeDirs 递归建目录。
func makeDirs(dir string) error { return os.MkdirAll(dir, 0o755) }

// osCreate 创建/截断文件。
func osCreate(p string) (*os.File, error) { return os.Create(p) }

// openFileRW 打开文件用于随机写（不存在则创建）。
func openFileRW(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
}

// writerAt 供 FTP/HTTP worker 写文件的接口（*os.File 满足）。
type writerAt interface {
	WriteAt(p []byte, off int64) (int, error)
}

// uniquePath 目标目录下重名时自动追加 " (n)" 后缀。
func uniquePath(name, dir string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	cand := filepath.Join(dir, name)
	for i := 1; ; i++ {
		if _, err := os.Stat(cand); err != nil {
			return filepath.Base(cand)
		}
		cand = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
	}
}

// formatBytes 字节数人性化显示。
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
