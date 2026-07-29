package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// maxLogSizeBytes caps sync.log before it gets rotated. syncd is meant to
// run unattended (systemd/launchd, as a watch daemon or a scheduled sync)
// indefinitely, so the log can't be allowed to grow forever between manual
// cleanups.
const maxLogSizeBytes = 20 * 1024 * 1024 // 20 MB

type Logger struct {
	silent bool
	file   *os.File
	mu     sync.Mutex
}

type LogEvent struct {
	Timestamp string         `json:"ts"`
	Level     string         `json:"level"`
	Message   string         `json:"message"`
	Context   map[string]any `json:"context,omitempty"`
}

func NewLogger(silent bool, logPath string) (*Logger, error) {
	l := &Logger{silent: silent}
	if !silent {
		return l, nil
	}
	if err := rotateLogIfNeeded(logPath); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	l.file = f
	return l, nil
}

// rotateLogIfNeeded renames an existing over-cap log file to logPath+".old"
// (overwriting any previous .old) so the caller can open a fresh log file.
// Self-contained on purpose - macOS (newsyslog) and Ubuntu (logrotate) use
// different native rotation tools, and this keeps behavior identical on
// both without depending on either being configured.
func rotateLogIfNeeded(logPath string) error {
	info, err := os.Stat(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat log file: %w", err)
	}
	if info.Size() < maxLogSizeBytes {
		return nil
	}
	if err := os.Rename(logPath, logPath+".old"); err != nil {
		return fmt.Errorf("rotate log file: %w", err)
	}
	return nil
}

func (l *Logger) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	return l.file.Close()
}

func (l *Logger) Info(msg string, ctx map[string]any) {
	l.write("info", msg, ctx)
}

func (l *Logger) Warn(msg string, ctx map[string]any) {
	l.write("warn", msg, ctx)
}

func (l *Logger) Error(msg string, ctx map[string]any) {
	l.write("error", msg, ctx)
}

func (l *Logger) Println(msg string) {
	if l.silent {
		l.Info(msg, nil)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Println(msg)
}

func (l *Logger) Printf(format string, args ...any) {
	l.Println(fmt.Sprintf(format, args...))
}

func (l *Logger) IsSilent() bool {
	return l.silent
}

func (l *Logger) write(level, msg string, ctx map[string]any) {
	if !l.silent {
		return
	}
	ev := LogEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Level:     level,
		Message:   msg,
		Context:   ctx,
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.file.Write(append(b, '\n'))
}
