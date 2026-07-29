package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"syncd/internal/app"
	"syncd/internal/reconcile"
)

// withTempHome points os.UserHomeDir() (via $HOME) at a fresh temp
// directory for the duration of the test, so syncdStateDir/defaultJobsDBPath
// /hashJobDBPath never touch the real developer/CI home directory.
func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestHashJobDBPath_DistinctPathsForDistinctJobs(t *testing.T) {
	withTempHome(t)

	pathA, err := hashJobDBPath("job-a")
	if err != nil {
		t.Fatalf("hashJobDBPath(job-a): %v", err)
	}
	pathB, err := hashJobDBPath("job-b")
	if err != nil {
		t.Fatalf("hashJobDBPath(job-b): %v", err)
	}
	if pathA == pathB {
		t.Fatalf("expected distinct db paths for distinct job names, both got %q", pathA)
	}
	if !strings.HasSuffix(pathA, "hash-job-a.db") {
		t.Errorf("pathA = %q, want suffix hash-job-a.db", pathA)
	}
	if !strings.HasSuffix(pathB, "hash-job-b.db") {
		t.Errorf("pathB = %q, want suffix hash-job-b.db", pathB)
	}

	// Two ad-hoc runs with different --left/--right pairs already get
	// distinct adHocJobName hashes as their SyncJob.Name (see
	// resolveSyncJobs); hashJobDBPath must keep that distinction, not
	// collapse them onto a shared db.
	adHocA := adHocJobName("/left/one", "/right/one")
	adHocB := adHocJobName("/left/two", "/right/two")
	if adHocA == adHocB {
		t.Fatalf("test setup broken: adHocJobName collided for distinct pairs")
	}
	pathAdHocA, err := hashJobDBPath(adHocA)
	if err != nil {
		t.Fatalf("hashJobDBPath(%s): %v", adHocA, err)
	}
	pathAdHocB, err := hashJobDBPath(adHocB)
	if err != nil {
		t.Fatalf("hashJobDBPath(%s): %v", adHocB, err)
	}
	if pathAdHocA == pathAdHocB {
		t.Fatalf("expected distinct db paths for distinct ad-hoc jobs, both got %q", pathAdHocA)
	}
}

func TestHashJobDBPath_SanitizesUnsafeCharacters(t *testing.T) {
	withTempHome(t)

	path, err := hashJobDBPath("weird/name with spaces:and*stuff")
	if err != nil {
		t.Fatalf("hashJobDBPath: %v", err)
	}
	base := filepath.Base(path)
	if strings.ContainsAny(base, "/ :*") {
		t.Errorf("expected unsafe characters stripped from db filename, got %q", base)
	}
	if base != "hash-weird_name_with_spaces_and_stuff.db" {
		t.Errorf("unexpected sanitized filename: %q", base)
	}
}

func TestHashJobDBPath_DifferentFromFastJobsDBPath(t *testing.T) {
	withTempHome(t)

	jobsPath, err := defaultJobsDBPath()
	if err != nil {
		t.Fatalf("defaultJobsDBPath: %v", err)
	}
	hashPath, err := hashJobDBPath("some-job")
	if err != nil {
		t.Fatalf("hashJobDBPath: %v", err)
	}
	if jobsPath == hashPath {
		t.Fatalf("hash job db must not collide with the shared fast-mode jobs.db, both got %q", jobsPath)
	}
}

// TestHashSync_TwoJobsSharingARelPathDoNotCorruptEachOther is an
// end-to-end regression test for the bug this per-job db-path scheme fixes:
// before it, every --hash job shared one jobs.db, and file_manifest is keyed
// by relative path only (no job/name column) - so two --hash jobs whose
// trees both contain "x.txt" at the root would stomp on each other's
// three-way baseline. It drives app.SyncOnce directly (the same call
// runSyncCmd makes per job) with DBPath computed by hashJobDBPath, exactly
// as the fixed runSyncCmd loop now does.
func TestHashSync_TwoJobsSharingARelPathDoNotCorruptEachOther(t *testing.T) {
	withTempHome(t)
	ctx := context.Background()

	setupJob := func(name, leftContent string) (left, right string) {
		left = t.TempDir()
		right = t.TempDir()
		if err := os.WriteFile(filepath.Join(left, "x.txt"), []byte(leftContent), 0o644); err != nil {
			t.Fatalf("write %s left x.txt: %v", name, err)
		}
		return left, right
	}

	leftA, rightA := setupJob("job-a", "content-a")
	leftB, rightB := setupJob("job-b", "content-b")

	runHashJob := func(name, left, right string) {
		t.Helper()
		dbPath, err := hashJobDBPath(name)
		if err != nil {
			t.Fatalf("hashJobDBPath(%s): %v", name, err)
		}
		opts := app.SyncOptions{
			Name:    name,
			Left:    left,
			Right:   right,
			Mode:    reconcile.FastMirror, // ignored by the --hash/robust path
			UseHash: true,
			Assume:  true,
			Workers: 2,
			DBPath:  dbPath,
		}
		if err := app.SyncOnce(ctx, opts); err != nil {
			t.Fatalf("SyncOnce(%s): %v", name, err)
		}
	}

	// Run both jobs twice each, interleaved, so any cross-job manifest
	// corruption (job B's scan/plan seeing job A's file_manifest row for
	// "x.txt", or vice versa) would surface as a wrong three-way decision
	// (e.g. an incorrectly-detected conflict or a skipped upload).
	runHashJob("job-a", leftA, rightA)
	runHashJob("job-b", leftB, rightB)
	runHashJob("job-a", leftA, rightA)
	runHashJob("job-b", leftB, rightB)

	gotA, err := os.ReadFile(filepath.Join(rightA, "x.txt"))
	if err != nil {
		t.Fatalf("read job-a right x.txt: %v", err)
	}
	if string(gotA) != "content-a" {
		t.Errorf("job-a right x.txt = %q, want %q (cross-job corruption?)", gotA, "content-a")
	}

	gotB, err := os.ReadFile(filepath.Join(rightB, "x.txt"))
	if err != nil {
		t.Fatalf("read job-b right x.txt: %v", err)
	}
	if string(gotB) != "content-b" {
		t.Errorf("job-b right x.txt = %q, want %q (cross-job corruption?)", gotB, "content-b")
	}

	// The two jobs must have ended up with separate manifest dbs.
	dbA, err := hashJobDBPath("job-a")
	if err != nil {
		t.Fatalf("hashJobDBPath(job-a): %v", err)
	}
	dbB, err := hashJobDBPath("job-b")
	if err != nil {
		t.Fatalf("hashJobDBPath(job-b): %v", err)
	}
	if dbA == dbB {
		t.Fatalf("expected separate manifest dbs, both got %q", dbA)
	}
	if _, err := os.Stat(dbA); err != nil {
		t.Errorf("expected job-a manifest db to exist at %s: %v", dbA, err)
	}
	if _, err := os.Stat(dbB); err != nil {
		t.Errorf("expected job-b manifest db to exist at %s: %v", dbB, err)
	}
}
