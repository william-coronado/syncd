package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

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

func TestNewLogger_NoLogFileWhenNotSilent(t *testing.T) {
	logger, err := NewLogger(false, filepath.Join(t.TempDir(), "unused.log"))
	if err != nil {
		t.Fatalf("NewLogger: %v", err)
	}
	defer logger.Close()
	if logger.IsSilent() {
		t.Fatal("expected IsSilent() to be false")
	}
}
