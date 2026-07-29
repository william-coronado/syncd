package runtime

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// captureStderr redirects os.Stderr for the duration of fn and returns
// everything written to it. Logger.Error/Warn must always reach stderr,
// silent or not, so tests need to observe that stream directly.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	return buf.String()
}

// captureStdout mirrors captureStderr for stdout, used to confirm Info never
// spams the CLI even for a non-silent logger.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	return buf.String()
}

func TestNewLogger_RotatesOversizedLogFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "sync.log")

	oversized := bytes.Repeat([]byte("x"), maxLogSizeBytes+1)
	if err := os.WriteFile(logPath, oversized, 0o644); err != nil {
		t.Fatalf("seed oversized log: %v", err)
	}

	logger, err := NewLogger(true, logPath)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	oldPath := logPath + ".old"
	oldInfo, err := os.Stat(oldPath)
	if err != nil {
		t.Fatalf("expected rotated file at %s: %v", oldPath, err)
	}
	if oldInfo.Size() != int64(len(oversized)) {
		t.Fatalf("rotated file size = %d, want %d", oldInfo.Size(), len(oversized))
	}

	newInfo, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("expected fresh log file at %s: %v", logPath, err)
	}
	if newInfo.Size() != 0 {
		t.Fatalf("expected fresh log file to start empty, got size %d", newInfo.Size())
	}
}

func TestNewLogger_DoesNotRotateUnderCap(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "sync.log")
	if err := os.WriteFile(logPath, []byte("small log content"), 0o644); err != nil {
		t.Fatalf("seed log: %v", err)
	}

	logger, err := NewLogger(true, logPath)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	if _, err := os.Stat(logPath + ".old"); !os.IsNotExist(err) {
		t.Fatalf("expected no rotation for a small log file, but .old exists (err=%v)", err)
	}
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if string(content) != "small log content" {
		t.Fatalf("expected existing small log content preserved (opened with O_APPEND), got %q", content)
	}
}

// TestNewLogger_OpensLogFileWhenNotSilent replaces the old
// TestNewLogger_NoLogFileWhenNotSilent, which encoded the bug this task
// fixes: a non-silent Logger used to skip opening the log file entirely,
// silently discarding every structured event (task-2-brief.md, 2a). The log
// file must now always be opened when a logPath is given, regardless of
// --silent; only IsSilent()'s meaning (suppress stdout chatter) is
// unaffected.
func TestNewLogger_OpensLogFileWhenNotSilent(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "sync.log")

	logger, err := NewLogger(false, logPath)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	if logger.IsSilent() {
		t.Fatal("expected IsSilent() to be false")
	}

	logger.Info("hello", map[string]any{"k": "v"})

	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("expected log file to exist for a non-silent logger with a log path: %v", err)
	}
	if !strings.Contains(string(content), "hello") {
		t.Fatalf("expected structured Info event in log file, got %q", content)
	}
}

// TestNewLogger_NoFileWithEmptyLogPath covers the sync --hash / init contract
// (NewLogger(false, "")): no log file should be created or required, but
// Error must still reach the operator via stderr.
func TestNewLogger_NoFileWithEmptyLogPath(t *testing.T) {
	logger, err := NewLogger(false, "")
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	out := captureStderr(t, func() {
		logger.Error("no file case", nil)
	})
	if !strings.Contains(out, "no file case") {
		t.Fatalf("expected Error to reach stderr even with no log file, got %q", out)
	}
}

// TestLogger_ErrorReachesStderrRegardlessOfSilent is the core regression test
// for 2a: --silent must never be able to hide an error from the operator.
func TestLogger_ErrorReachesStderrRegardlessOfSilent(t *testing.T) {
	for _, silent := range []bool{true, false} {
		silent := silent
		t.Run("silent="+strconv.FormatBool(silent), func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, "sync.log")
			logger, err := NewLogger(silent, logPath)
			if err != nil {
				t.Fatalf("NewLogger: %v", err)
			}
			defer logger.Close()

			out := captureStderr(t, func() {
				logger.Error("watch: remote canary check failed", map[string]any{"error": "boom"})
			})
			if !strings.Contains(out, "watch: remote canary check failed") {
				t.Fatalf("expected Error message on stderr, got %q", out)
			}

			content, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("expected structured event written to log file: %v", err)
			}
			if !strings.Contains(string(content), "watch: remote canary check failed") {
				t.Fatalf("expected structured event in log file, got %q", content)
			}
		})
	}
}

// TestLogger_WarnReachesStderrRegardlessOfSilent mirrors the Error case for
// Warn, per the brief's "Error and Warn must always reach the operator".
func TestLogger_WarnReachesStderrRegardlessOfSilent(t *testing.T) {
	logger, err := NewLogger(true, filepath.Join(t.TempDir(), "sync.log"))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	out := captureStderr(t, func() {
		logger.Warn("retrying after transient error", nil)
	})
	if !strings.Contains(out, "retrying after transient error") {
		t.Fatalf("expected Warn message on stderr, got %q", out)
	}
}

// TestLogger_InfoDoesNotPrintToStdoutWhenNotSilent confirms Info's existing
// behavior is preserved: structured to the log file, never printed to
// stdout (that would spam a non-silent CLI run).
func TestLogger_InfoDoesNotPrintToStdoutWhenNotSilent(t *testing.T) {
	logger, err := NewLogger(false, filepath.Join(t.TempDir(), "sync.log"))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	out := captureStdout(t, func() {
		logger.Info("quiet info", nil)
	})
	if out != "" {
		t.Fatalf("expected Info not to print to stdout, got %q", out)
	}
}

// TestLogger_PrintlnStillGoesToStdoutWhenNotSilent locks in the brief's
// requirement that Println/Printf keep their current behavior exactly.
func TestLogger_PrintlnStillGoesToStdoutWhenNotSilent(t *testing.T) {
	logger, err := NewLogger(false, "")
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	out := captureStdout(t, func() {
		logger.Println("hello operator")
	})
	if !strings.Contains(out, "hello operator") {
		t.Fatalf("expected Println to print to stdout when not silent, got %q", out)
	}
}

// TestLogger_PrintlnBecomesStructuredInfoWhenSilent locks in the other half:
// silent mode routes Println through Info instead of stdout.
func TestLogger_PrintlnBecomesStructuredInfoWhenSilent(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "sync.log")
	logger, err := NewLogger(true, logPath)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	out := captureStdout(t, func() {
		logger.Println("quiet operator message")
	})
	if out != "" {
		t.Fatalf("expected no stdout output when silent, got %q", out)
	}

	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(content), "quiet operator message") {
		t.Fatalf("expected Println to route to structured Info in the log file, got %q", content)
	}
}

// TestLogger_RotatesWhileRunning is the 2b regression test: rotation must be
// checked from write(), not only at NewLogger construction time, so a
// long-running watch daemon actually rotates once it crosses the cap.
func TestLogger_RotatesWhileRunning(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "sync.log")

	logger, err := NewLogger(true, logPath)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	// Each entry is ~4KB of context; write until the cap is crossed and
	// rotation happens, stopping immediately so nothing further lands in the
	// pre-rotation file - this is checking write()'s own rotation decision,
	// not racing a fixed iteration count against JSON-encoding overhead.
	filler := strings.Repeat("x", 4096)
	maxIterations := maxLogSizeBytes/len(filler) + 16
	rotated := false
	for i := 0; i < maxIterations; i++ {
		logger.Info("filler", map[string]any{"data": filler})
		if _, err := os.Stat(logPath + ".old"); err == nil {
			rotated = true
			break
		}
	}
	if !rotated {
		t.Fatalf("expected rotation to have occurred within %d writes of ~%dB each", maxIterations, len(filler))
	}

	logger.Info("after-rotation", nil)
	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log after rotation: %v", err)
	}
	if !strings.Contains(string(content), "after-rotation") {
		t.Fatalf("expected logging to continue into the fresh file after rotation, got %q", content)
	}
	if strings.Contains(string(content), `"filler"`) {
		t.Fatalf("expected fresh file to not contain pre-rotation entries, got %q", content)
	}
}

// TestLogger_ConcurrentWritesAreRaceFree exercises Error/Info/Warn from many
// goroutines at once - Logger is written from concurrent worker goroutines
// in production (execute.RunWorkerPool). Run with -race.
func TestLogger_ConcurrentWritesAreRaceFree(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "sync.log")
	logger, err := NewLogger(false, logPath)
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()

	captureStderr(t, func() {
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				logger.Info("concurrent info", map[string]any{"i": i})
				logger.Error("concurrent error", map[string]any{"i": i})
				logger.Warn("concurrent warn", map[string]any{"i": i})
			}(i)
		}
		wg.Wait()
	})
}
