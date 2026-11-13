package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"syncd/internal/fs"
)

// An identical pair whose two copies carry different mtimes (copied without
// preserving it - common when seeding a share by hand) used to be baselined
// with the LOCAL stat only, so every --remote-poll re-hashed the remote copy
// over SMB, forever: the remote file's mtime never matched the manifest's.
// The baseline now records each side's own stat, so the next HashIfChanged
// scan reuses the cached hash on BOTH sides.
func TestRunRobustOnce_IdenticalPairWithDifferentMtimes_RemoteCacheHits(t *testing.T) {
	local, remote := t.TempDir(), t.TempDir()
	lPath, rPath := filepath.Join(local, "a.txt"), filepath.Join(remote, "a.txt")
	for _, p := range []string{lPath, rPath} {
		if err := os.WriteFile(p, []byte("same bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lTime, rTime := time.Unix(1_700_000_000, 0), time.Unix(1_700_009_999, 0)
	if err := os.Chtimes(lPath, lTime, lTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(rPath, rTime, rTime); err != nil {
		t.Fatal(err)
	}

	p := newTestRobustParams(t, local, remote)
	ctx := context.Background()
	if _, failures, err := RunRobustOnce(ctx, p); err != nil || failures != 0 {
		t.Fatalf("RunRobustOnce: failures=%d err=%v", failures, err)
	}

	manifest, err := p.Repo.LoadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := manifest["a.txt"]
	if !ok {
		t.Fatal("identical pair was not baselined")
	}
	if size, mtime := row.RemoteStat(); size != row.Size || mtime != rTime.Unix() {
		t.Fatalf("remote stat = %d/%d, want %d/%d", size, mtime, row.Size, rTime.Unix())
	}

	// Poison the cached hash: a cache HIT returns the sentinel, a miss
	// (re-hash) returns the real BLAKE3 - so this observes whether each
	// side was re-read, without any hashing seam.
	row.Hash = "cached-sentinel"
	manifest["a.txt"] = row
	scans, err := fs.ScanPair(ctx, local, remote, manifest, fs.HashIfChanged, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := scans.Local.Files["a.txt"].Hash; got != "cached-sentinel" {
		t.Fatalf("local side re-hashed (hash %q); want cache hit", got)
	}
	if got := scans.Remote.Files["a.txt"].Hash; got != "cached-sentinel" {
		t.Fatalf("remote side re-hashed (hash %q); want cache hit", got)
	}
}
