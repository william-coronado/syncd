package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"syncd/internal/app"
	"syncd/internal/runtime"
)

// The service must run exactly what the user asked for: the roots, the two
// always-on service flags, the flags they passed (paths made absolute) -
// and none of the defaults, nor the install-only --name/--bin.
func TestServiceWatchArgs(t *testing.T) {
	var o watchFlags
	cmd := &cobra.Command{Use: "install"}
	addWatchFlags(cmd, &o)
	var name, bin string
	cmd.Flags().StringVar(&name, "name", "", "")
	cmd.Flags().StringVar(&bin, "bin", "", "")
	if err := cmd.ParseFlags([]string{
		"--local", "/data/local", "--remote", "/mnt/dev",
		"--max-delete", "200", "--remote-poll", "1m", "--state-dir", "rel/state",
		"--no-trash", "--name", "dev", "--bin", "/opt/syncd", "--silent",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := o.watchConfig()
	if err != nil {
		t.Fatal(err)
	}

	got := serviceWatchArgs(cmd.Flags(), cfg)
	absState, _ := filepath.Abs("rel/state")
	want := []string{
		"watch", "--local", "/data/local", "--remote", "/mnt/dev", "--silent", "--wait-for-remote",
		"--max-delete=200", "--no-trash=true", "--remote-poll=1m0s", "--state-dir=" + absState,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("args:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// What the service is given must be accepted by `syncd watch` itself.
	var parsed watchFlags
	watch := &cobra.Command{Use: "watch"}
	addWatchFlags(watch, &parsed)
	if err := watch.ParseFlags(got[1:]); err != nil {
		t.Fatalf("watch rejects the service's arguments: %v", err)
	}
	if !parsed.waitForRemote || !parsed.silent || !parsed.noTrash || parsed.maxDelete != "200" {
		t.Fatalf("round trip lost flags: %+v", parsed)
	}
}

func TestWatchConfigRejectsBadSafetyFlags(t *testing.T) {
	base := watchFlags{local: "/a", remote: "/b", workers: 1, remotePoll: 1, deepScan: 1, maxDelete: "50%", trashDays: 30}
	if _, err := base.watchConfig(); err != nil {
		t.Fatalf("valid flags: %v", err)
	}
	bad := base
	bad.maxDelete = "many"
	if _, err := bad.watchConfig(); err == nil || !strings.Contains(err.Error(), "--max-delete") {
		t.Errorf("bad --max-delete: %v", err)
	}
	bad = base
	bad.trashDays = -1
	if _, err := bad.watchConfig(); err == nil || !strings.Contains(err.Error(), "--trash-days") {
		t.Errorf("bad --trash-days: %v", err)
	}
}

func TestCheckServiceRoots(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()

	// Mounted but never initialized: refuse, and say how to fix it.
	_, err := checkServiceRoots(app.WatchConfig{LocalRoot: local, RemoteRoot: remote})
	if err == nil || !strings.Contains(err.Error(), "syncd init") {
		t.Fatalf("uninitialized remote: %v", err)
	}

	if _, err := runtime.EnsureRemoteCanary(remote); err != nil {
		t.Fatal(err)
	}
	if notes, err := checkServiceRoots(app.WatchConfig{LocalRoot: local, RemoteRoot: remote}); err != nil || len(notes) != 0 {
		t.Fatalf("ready pair: notes=%v err=%v", notes, err)
	}

	// Not mounted right now: fine, with a note.
	notes, err := checkServiceRoots(app.WatchConfig{LocalRoot: local, RemoteRoot: filepath.Join(remote, "unmounted")})
	if err != nil || len(notes) != 1 {
		t.Fatalf("unmounted remote: notes=%v err=%v", notes, err)
	}

	if _, err := checkServiceRoots(app.WatchConfig{LocalRoot: filepath.Join(local, "missing"), RemoteRoot: remote}); err == nil {
		t.Fatal("accepted a missing local root")
	}
}
