// Package cli 让每个可执行文件都能在命令行直接调用（不进 core/gateway 也能联调）：
// 统一日志落盘、自驱动 IPC 子进程、HTTP 探测。
package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger 同时写文件与 stderr：文件留证，stderr 供人工实时观察。
// 行首带 RFC3339 毫秒时间戳，便于把三方日志（gateway/core/plugin）按时间对齐。
type Logger struct {
	mu   sync.Mutex
	name string
	f    *os.File
	out  io.Writer
}

// NewLogger 打开（追加）日志文件；path 为空则只写 stderr。
func NewLogger(name, path string) (*Logger, error) {
	l := &Logger{name: name, out: os.Stderr}
	if path == "" {
		return l, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建日志目录失败: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开日志文件失败: %w", err)
	}
	l.f = f
	return l, nil
}

// Path 返回日志文件路径（未落盘为空）。
func (l *Logger) Path() string {
	if l == nil || l.f == nil {
		return ""
	}
	return l.f.Name()
}

func (l *Logger) write(level, format string, a ...any) {
	if l == nil {
		return
	}
	line := fmt.Sprintf("%s [%s] %s%s\n", time.Now().Format("2006-01-02T15:04:05.000"), l.name, level, fmt.Sprintf(format, a...))
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.out != nil {
		_, _ = io.WriteString(l.out, line)
	}
	if l.f != nil {
		_, _ = io.WriteString(l.f, line)
		_ = l.f.Sync() // 立即落盘：联调时进程可能被强杀
	}
}

// Infof 一般信息。
func (l *Logger) Infof(format string, a ...any) { l.write("", format, a...) }

// Debugf 细节（IPC 帧、HTTP 头等）。
func (l *Logger) Debugf(format string, a ...any) { l.write("DEBUG ", format, a...) }

// Errorf 错误。
func (l *Logger) Errorf(format string, a ...any) { l.write("ERROR ", format, a...) }

// Raw 原样写一行（已是完整内容，不再加时间戳，用于粘贴上游报文）。
func (l *Logger) Raw(s string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.out != nil {
		_, _ = io.WriteString(l.out, s)
	}
	if l.f != nil {
		_, _ = io.WriteString(l.f, s)
	}
}

// RawFile 只写文件不写 stderr：命令行模式正文已打到 stdout，避免终端出现两份。
func (l *Logger) RawFile(s string) {
	if l == nil || l.f == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = io.WriteString(l.f, s)
}

// Close 关闭文件。
func (l *Logger) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	return l.f.Close()
}

// DefaultLogPath 约定日志落在 <dataDir>/logs/<name>.log。
func DefaultLogPath(dataDir, name string) string {
	if dataDir == "" {
		dataDir = "."
	}
	return filepath.Join(dataDir, "logs", name+".log")
}
