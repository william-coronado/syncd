package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"syncd/internal/app"
	"syncd/internal/reconcile"
	"syncd/internal/runtime"
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

// TestSyncLockPath_DistinctPathsForDistinctJobs checks that the per-job
// lock file is actually per-job: two different job names must never
// collide on the same lock path, or a lock held by one job would block a
// completely unrelated job's run too.
func TestSyncLockPath_DistinctPathsForDistinctJobs(t *testing.T) {
	withTempHome(t)

	a, err := syncLockPath("job-a")
	if err != nil {
		t.Fatalf("syncLockPath(job-a): %v", err)
	}
	b, err := syncLockPath("job-b")
	if err != nil {
		t.Fatalf("syncLockPath(job-b): %v", err)
	}
	if a == b {
		t.Fatalf("expected distinct lock paths for distinct job names, both got %q", a)
	}
	if !strings.HasSuffix(a, "sync-job-a.lock") {
		t.Errorf("path = %q, want suffix sync-job-a.lock", a)
	}
}

// TestRunSyncJob_HeldLockReturnsClearErrorNamingTheJob is a regression test
// for Task 5c: `syncd sync` previously took no lock at all, so two
// concurrent runs of the same two-way job could race on both jobs.db and
// the files themselves. Once a job's lock is held elsewhere, runSyncJob
// must fail that job with a clear, non-panicking error that names the job
// - not silently proceed and race.
func TestRunSyncJob_HeldLockReturnsClearErrorNamingTheJob(t *testing.T) {
	withTempHome(t)

	left, right := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(left, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	job := SyncJob{Name: "locked-job", Left: left, Right: right, Mode: string(reconcile.FastMirror)}

	key, err := jobStateKey(job.Name, job.Left, job.Right)
	if err != nil {
		t.Fatalf("jobStateKey: %v", err)
	}
	lockPath, err := syncLockPath(key)
	if err != nil {
		t.Fatalf("syncLockPath: %v", err)
	}
	heldLock, err := runtime.AcquireLock(lockPath)
	if err != nil {
		t.Fatalf("pre-acquire lock: %v", err)
	}
	defer heldLock.Release()

	jobsDBPath, err := defaultJobsDBPath()
	if err != nil {
		t.Fatalf("defaultJobsDBPath: %v", err)
	}

	err = runSyncJob(context.Background(), job, syncRunOptions{Assume: true, JobsDBPath: jobsDBPath})
	if err == nil {
		t.Fatal("expected an error when the job's lock is already held")
	}
	if !strings.Contains(err.Error(), job.Name) {
		t.Errorf("expected error to name the job %q, got: %v", job.Name, err)
	}

	// The held lock must have prevented the run outright - the file must
	// not have been copied.
	if _, statErr := os.Stat(filepath.Join(right, "a.txt")); !os.IsNotExist(statErr) {
		t.Error("job ran despite its lock being held")
	}
}

// TestRunSyncJobs_HeldLockDoesNotAbortOtherJobs is the batch-level half of
// the same fix: the lock is per-job, so one job's lock being held must not
// stop unrelated jobs in the same batch from running.
func TestRunSyncJobs_HeldLockDoesNotAbortOtherJobs(t *testing.T) {
	withTempHome(t)

	leftA, rightA := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(leftA, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	leftB, rightB := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(leftB, "b.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write b.txt: %v", err)
	}

	lockedJob := SyncJob{Name: "locked-job", Left: leftA, Right: rightA, Mode: string(reconcile.FastMirror)}
	freeJob := SyncJob{Name: "free-job", Left: leftB, Right: rightB, Mode: string(reconcile.FastMirror)}

	lockedKey, err := jobStateKey(lockedJob.Name, lockedJob.Left, lockedJob.Right)
	if err != nil {
		t.Fatalf("jobStateKey: %v", err)
	}
	lockPath, err := syncLockPath(lockedKey)
	if err != nil {
		t.Fatalf("syncLockPath: %v", err)
	}
	heldLock, err := runtime.AcquireLock(lockPath)
	if err != nil {
		t.Fatalf("pre-acquire lock: %v", err)
	}
	defer heldLock.Release()

	jobsDBPath, err := defaultJobsDBPath()
	if err != nil {
		t.Fatalf("defaultJobsDBPath: %v", err)
	}

	err = runSyncJobs(context.Background(), []SyncJob{lockedJob, freeJob}, syncRunOptions{Assume: true, JobsDBPath: jobsDBPath})
	if err == nil {
		t.Fatal("expected an aggregate error since locked-job's lock is held")
	}

	// free-job must still have completed despite locked-job's failure.
	if b, readErr := os.ReadFile(filepath.Join(rightB, "b.txt")); readErr != nil || string(b) != "hi" {
		t.Errorf("free-job did not complete: err=%v content=%q", readErr, b)
	}
	// locked-job's destination must remain untouched.
	if _, statErr := os.Stat(filepath.Join(rightA, "a.txt")); !os.IsNotExist(statErr) {
		t.Error("locked-job appears to have run despite its held lock")
	}
}

// TestRunSyncJobs_DryRunLeavesBothTreesByteForByteUntouched is the
// CLI-level half of Task 5e's --dry-run requirement: driven the same way
// runSyncCmd drives it (through runSyncJobs/syncRunOptions), a --dry-run
// job must leave both the left and right trees completely untouched, even
// with Assume: true (--dry-run must not require --yes, and conversely
// --yes must not make a dry run mutate anything either).
func TestRunSyncJobs_DryRunLeavesBothTreesByteForByteUntouched(t *testing.T) {
	withTempHome(t)

	left, right := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(left, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write left a.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(right, "stale.txt"), []byte("old"), 0o644); err != nil {
		t.Fatalf("write right stale.txt: %v", err)
	}

	job := SyncJob{Name: "dry-job", Left: left, Right: right, Mode: string(reconcile.FastMirror)}
	jobsDBPath, err := defaultJobsDBPath()
	if err != nil {
		t.Fatalf("defaultJobsDBPath: %v", err)
	}

	err = runSyncJobs(context.Background(), []SyncJob{job}, syncRunOptions{
		Assume:     true,
		DryRun:     true,
		JobsDBPath: jobsDBPath,
	})
	if err != nil {
		t.Fatalf("runSyncJobs dry-run returned error: %v", err)
	}

	// Left tree: untouched (mirror only ever writes to the right).
	leftContent, err := os.ReadFile(filepath.Join(left, "a.txt"))
	if err != nil || string(leftContent) != "hi" {
		t.Errorf("left a.txt changed: err=%v content=%q", err, leftContent)
	}
	if entries, _ := os.ReadDir(left); len(entries) != 1 {
		t.Errorf("left tree gained/lost entries: %v", entries)
	}

	// Right tree: a real (non-dry) mirror would copy a.txt in and delete
	// stale.txt. Neither must have happened.
	if _, statErr := os.Stat(filepath.Join(right, "a.txt")); !os.IsNotExist(statErr) {
		t.Error("dry-run copied a.txt to right - tree was mutated")
	}
	rightContent, err := os.ReadFile(filepath.Join(right, "stale.txt"))
	if err != nil || string(rightContent) != "old" {
		t.Errorf("dry-run deleted or modified stale.txt: err=%v content=%q", err, rightContent)
	}
	if entries, _ := os.ReadDir(right); len(entries) != 1 {
		t.Errorf("right tree gained/lost entries: %v", entries)
	}
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
	if base != "hash-weird%2Fname%20with%20spaces%3Aand%2Astuff.db" {
		t.Errorf("unexpected sanitized filename: %q", base)
	}
}

// TestSanitizeJobDBName_DistinctNamesNeverCollide is a regression test for a
// review finding (GLM-5.2): the previous sanitizer mapped every unsafe
// character to a shared literal '_', so "a/b" and "a_b" - two distinct job
// names - both sanitized to "a_b" and ended up sharing one hash-<id>.db and
// one sync-<id>.lock, silently cross-contaminating their baselines.
// Percent-encoding (this fix) is injective: distinct inputs must never
// collide on the same sanitized output.
func TestSanitizeJobDBName_DistinctNamesNeverCollide(t *testing.T) {
	pairs := [][2]string{
		{"a/b", "a_b"},
		{"a b", "a_b"},
		{"a%2Fb", "a/b"}, // a literal '%' in the input must itself be escaped
	}
	for _, pair := range pairs {
		got1 := sanitizeJobDBName(pair[0])
		got2 := sanitizeJobDBName(pair[1])
		if got1 == got2 {
			t.Errorf("sanitizeJobDBName(%q) and sanitizeJobDBName(%q) collided on %q", pair[0], pair[1], got1)
		}
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

// TestJobStateKey_DistinctForSameNameDifferentRoots is a regression test for
// review finding P1 #1 (Codex): hashJobDBPath/syncLockPath, and job_state's
// "job" column, used to be keyed on the bare display Name alone. Two config
// jobs that happen to share a Name - or one job later repointed at a
// different root pair - would then silently reuse each other's persisted
// state (a --hash job's three-way manifest, or a fast two-way job's
// job_state rows in the shared jobs.db), which can misclassify a brand-new
// remote-only file as "unchanged, deleted locally" and delete it.
// jobStateKey closes this by folding a hash of the canonicalized (Left,
// Right) pair into the identity, so same-Name jobs with different roots -
// or the same job repointed at a new pair - get separate state instead.
func TestJobStateKey_DistinctForSameNameDifferentRoots(t *testing.T) {
	leftA, rightA := t.TempDir(), t.TempDir()
	leftB, rightB := t.TempDir(), t.TempDir()

	keyA, err := jobStateKey("shared-name", leftA, rightA)
	if err != nil {
		t.Fatalf("jobStateKey A: %v", err)
	}
	keyB, err := jobStateKey("shared-name", leftB, rightB)
	if err != nil {
		t.Fatalf("jobStateKey B: %v", err)
	}
	if keyA == keyB {
		t.Fatalf("expected distinct state keys for the same Name with different root pairs, both got %q", keyA)
	}
}

// TestJobStateKey_StableForSameInputs checks the key is deterministic across
// calls - callers (runSyncJob) recompute it, they don't persist it, so it
// must be stable given the same (name, left, right) every time.
func TestJobStateKey_StableForSameInputs(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()

	first, err := jobStateKey("stable-job", left, right)
	if err != nil {
		t.Fatalf("jobStateKey: %v", err)
	}
	second, err := jobStateKey("stable-job", left, right)
	if err != nil {
		t.Fatalf("jobStateKey: %v", err)
	}
	if first != second {
		t.Fatalf("expected jobStateKey to be deterministic, got %q then %q", first, second)
	}
}

// TestJobStateKey_DistinctForDifferentNamesSameRoots ensures the Name
// component still participates in the key: two differently-named jobs
// pointed at the same root pair (an unusual but legal config) must not
// collide either.
func TestJobStateKey_DistinctForDifferentNamesSameRoots(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()

	keyA, err := jobStateKey("job-a", left, right)
	if err != nil {
		t.Fatalf("jobStateKey A: %v", err)
	}
	keyB, err := jobStateKey("job-b", left, right)
	if err != nil {
		t.Fatalf("jobStateKey B: %v", err)
	}
	if keyA == keyB {
		t.Fatalf("expected distinct state keys for different Names on the same root pair, both got %q", keyA)
	}
}
