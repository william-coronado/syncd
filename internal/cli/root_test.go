package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// executeRoot runs rootCmd with args, capturing cobra's own output.
func executeRoot(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs(args)
	t.Cleanup(func() {
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
		syncLeft, syncRight, syncAssume = "", "", false
		// PersistentPreRun sets this on the command that ran; the cobra
		// tree is package state shared across tests.
		syncCmd.SilenceUsage = false
	})
	err := rootCmd.Execute()
	return out.String(), err
}

// A runtime failure (here: a missing mirror source) must surface as just
// the error - not printed twice, and not buried under the flag listing.
func TestRootCmd_RuntimeErrorPrintsNoUsageAndNoDuplicate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	out, err := executeRoot(t, "sync", "--left", filepath.Join(dir, "missing"), "--right", filepath.Join(dir, "dst"), "--yes")
	if err == nil {
		t.Fatal("want an error for a missing source")
	}
	if strings.Contains(out, "Usage:") {
		t.Fatalf("runtime error dumped usage:\n%s", out)
	}
	if strings.Contains(out, err.Error()) {
		t.Fatalf("cobra printed the error itself (Execute prints it again):\n%s", out)
	}
}

// Invocation mistakes still get usage help.
func TestRootCmd_FlagErrorStillPrintsUsage(t *testing.T) {
	out, err := executeRoot(t, "sync", "--no-such-flag")
	if err == nil {
		t.Fatal("want an error for an unknown flag")
	}
	if !strings.Contains(out, "Usage:") {
		t.Fatalf("flag error should print usage, got:\n%s", out)
	}
}
