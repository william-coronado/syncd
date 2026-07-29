package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxLogSizeBytes caps sync.log before it gets rotated. syncd is meant to
// run unattended (systemd/launchd, as a watch daemon or a scheduled sync)
// indefinitely, so the log can't be allowed to grow forever between manual
// cleanups.
const maxLogSizeBytes = 20 * 1024 * 1024 // 20 MB

const (
	levelInfo  = "info"
	levelWarn  = "warn"
	levelError = "error"
)

type Logger struct {
	silent bool
	// logPath is the path the log file was opened from, needed to rotate it
	// (close, rename to .old, reopen) in place from write(). Empty when the
	// logger has no log file at all (e.g. sync --hash, init).
	logPath string
	file    *os.File
	// fileSize tracks the current log file's size so write() can decide to
	// rotate without stat-ing the file on every single line.
	fileSize int64
	mu       sync.Mutex
}

type LogEvent struct {
	Timestamp string         `json:"ts"`
	Level     string         `json:"level"`
	Message   string         `json:"message"`
	Context   map[string]any `json:"context,omitempty"`
}

// NewLogger opens the log file at logPath, if any, regardless of silent:
// --silent only controls whether Println/Printf go to stdout, it must never
// be able to make errors or the log file itself disappear. Callers that have
// no log file to write (sync --hash, init) pass logPath == "" and get a
// Logger that reports errors on stderr only.
func NewLogger(silent bool, logPath string) (*Logger, error) {
	l := &Logger{silent: silent, logPath: logPath}
	if logPath == "" {
		return l, nil
	}
	if err := rotateLogIfNeeded(logPath); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat log file: %w", err)
	}
	l.file = f
	l.fileSize = info.Size()
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
	l.write(levelInfo, msg, ctx)
}

func (l *Logger) Warn(msg string, ctx map[string]any) {
	l.write(levelWarn, msg, ctx)
}

func (l *Logger) Error(msg string, ctx map[string]any) {
	l.write(levelError, msg, ctx)
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

// write records a structured event to the log file (if one is open) and,
// for level == "warn"/"error", always prints a human-readable line to
// stderr - --silent suppresses stdout chatter, it must never hide an error
// or warning from the operator (task-2-brief.md, 2a).
func (l *Logger) write(level, msg string, ctx map[string]any) {
	ev := LogEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Level:     level,
		Message:   msg,
		Context:   ctx,
	}
	b, marshalErr := json.Marshal(ev)

	l.mu.Lock()
	defer l.mu.Unlock()

	if marshalErr == nil && l.file != nil {
		l.writeToFileLocked(append(b, '\n'))
	}
	if level == levelWarn || level == levelError {
		fmt.Fprintln(os.Stderr, formatHuman(level, msg, ctx))
	}
}

// writeToFileLocked writes b to the open log file and rotates in place
// (close, rename to .old, reopen) once the running size crosses
// maxLogSizeBytes. Tracking fileSize in memory avoids an os.Stat call on
// every line - the only per-write cost is an integer comparison. Must be
// called with l.mu held.
func (l *Logger) writeToFileLocked(b []byte) {
	n, err := l.file.Write(b)
	l.fileSize += int64(n)
	if err != nil || l.fileSize < maxLogSizeBytes {
		return
	}
	l.rotateLocked()
}

// rotateLocked closes the current log file, renames it to logPath+".old"
// (overwriting any previous .old), and reopens a fresh file at logPath so a
// long-running watch daemon keeps logging past the size cap instead of
// growing the file forever. Must be called with l.mu held.
func (l *Logger) rotateLocked() {
	if err := l.file.Close(); err != nil {
		l.file = nil
		return
	}
	if err := os.Rename(l.logPath, l.logPath+".old"); err != nil {
		l.file = nil
		return
	}
	f, err := os.OpenFile(l.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		l.file = nil
		return
	}
	l.file = f
	l.fileSize = 0
}

// formatHuman renders a warn/error event as a single stderr line. Context
// keys are sorted for stable, testable output.
func formatHuman(level, msg string, ctx map[string]any) string {
	label := strings.ToUpper(level)
	if len(ctx) == 0 {
		return fmt.Sprintf("%s: %s", label, msg)
	}
	keys := make([]string, 0, len(ctx))
	for k := range ctx {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, ctx[k]))
	}
	return fmt.Sprintf("%s: %s (%s)", label, msg, strings.Join(parts, ", "))
}
