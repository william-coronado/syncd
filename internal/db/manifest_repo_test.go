package db

import (
	"context"
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *ManifestRepo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "manifest.db")
	sqlDB, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return NewManifestRepo(sqlDB)
}

func TestManifestRepo_LoadAllOnEmptyDB(t *testing.T) {
	repo := openTestDB(t)
	ctx := context.Background()

	rows, err := repo.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected empty manifest, got %+v", rows)
	}
}

func TestManifestRepo_UpsertThenLoad(t *testing.T) {
	repo := openTestDB(t)
	ctx := context.Background()

	row := ManifestRow{Path: "a.txt", Size: 10, Mtime: 100, Hash: "h1", LastSynced: 200}
	if err := repo.ApplyUpdates(ctx, []ManifestRow{row}, nil); err != nil {
		t.Fatalf("ApplyUpdates insert: %v", err)
	}

	rows, err := repo.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	got, ok := rows["a.txt"]
	if !ok {
		t.Fatalf("expected a.txt in manifest, got %+v", rows)
	}
	if got != row {
		t.Fatalf("loaded row = %+v, want %+v", got, row)
	}
}

func TestManifestRepo_UpsertOverwritesExistingRow(t *testing.T) {
	repo := openTestDB(t)
	ctx := context.Background()

	original := ManifestRow{Path: "a.txt", Size: 10, Mtime: 100, Hash: "h1", LastSynced: 200}
	updated := ManifestRow{Path: "a.txt", Size: 20, Mtime: 300, Hash: "h2", LastSynced: 400}

	if err := repo.ApplyUpdates(ctx, []ManifestRow{original}, nil); err != nil {
		t.Fatalf("ApplyUpdates insert: %v", err)
	}
	if err := repo.ApplyUpdates(ctx, []ManifestRow{updated}, nil); err != nil {
		t.Fatalf("ApplyUpdates update: %v", err)
	}

	rows, err := repo.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly one row after upsert, got %+v", rows)
	}
	if rows["a.txt"] != updated {
		t.Fatalf("loaded row = %+v, want %+v", rows["a.txt"], updated)
	}
}

func TestManifestRepo_Delete(t *testing.T) {
	repo := openTestDB(t)
	ctx := context.Background()

	row := ManifestRow{Path: "a.txt", Size: 10, Mtime: 100, Hash: "h1", LastSynced: 200}
	if err := repo.ApplyUpdates(ctx, []ManifestRow{row}, nil); err != nil {
		t.Fatalf("ApplyUpdates insert: %v", err)
	}
	if err := repo.ApplyUpdates(ctx, nil, []string{"a.txt"}); err != nil {
		t.Fatalf("ApplyUpdates delete: %v", err)
	}

	rows, err := repo.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("expected empty manifest after delete, got %+v", rows)
	}
}

func TestManifestRepo_UpsertFillsLastSyncedWhenZero(t *testing.T) {
	repo := openTestDB(t)
	ctx := context.Background()

	row := ManifestRow{Path: "a.txt", Size: 10, Mtime: 100, Hash: "h1"} // LastSynced left at zero
	if err := repo.ApplyUpdates(ctx, []ManifestRow{row}, nil); err != nil {
		t.Fatalf("ApplyUpdates insert: %v", err)
	}

	rows, err := repo.LoadAll(ctx)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if rows["a.txt"].LastSynced == 0 {
		t.Fatal("expected LastSynced to be populated automatically, got 0")
	}
}
