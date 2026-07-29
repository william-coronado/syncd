package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestJobStateReplaceAndLoad(t *testing.T) {
	dbh, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()
	repo := NewJobStateRepo(dbh)
	ctx := context.Background()

	in := map[string]JobFileState{"a.txt": {Path: "a.txt", Size: 5, Mtime: 100}}
	if err := repo.Replace(ctx, "docs", in); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Load(ctx, "docs")
	if err != nil {
		t.Fatal(err)
	}
	if got["a.txt"].Size != 5 || got["a.txt"].Mtime != 100 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	// Replace is total: removing a key drops it.
	if err := repo.Replace(ctx, "docs", map[string]JobFileState{}); err != nil {
		t.Fatal(err)
	}
	got, _ = repo.Load(ctx, "docs")
	if len(got) != 0 {
		t.Fatalf("expected empty after replace, got %d", len(got))
	}
}

func TestJobStateNamespacesAreIsolated(t *testing.T) {
	dbh, _ := Open(filepath.Join(t.TempDir(), "m.db"))
	defer dbh.Close()
	repo := NewJobStateRepo(dbh)
	ctx := context.Background()
	_ = repo.Replace(ctx, "job1", map[string]JobFileState{"x": {Path: "x", Size: 1, Mtime: 1}})
	_ = repo.Replace(ctx, "job2", map[string]JobFileState{"y": {Path: "y", Size: 2, Mtime: 2}})
	g1, _ := repo.Load(ctx, "job1")
	if _, ok := g1["y"]; ok {
		t.Error("job2 data leaked into job1")
	}
}
