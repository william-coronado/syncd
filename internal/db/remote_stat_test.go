package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"

	_ "modernc.org/sqlite"
)

// A manifest created by an older syncd (no remote_size/remote_mtime
// columns) must be migrated in place, with its existing rows loading as
// "remote stat same as local".
func TestOpen_MigratesLegacyManifestSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE file_manifest (path TEXT PRIMARY KEY, size INTEGER NOT NULL, mtime INTEGER NOT NULL, hash TEXT NOT NULL, last_synced INTEGER NOT NULL);
		INSERT INTO file_manifest VALUES ('old.txt', 3, 100, 'h', 200);`); err != nil {
		t.Fatal(err)
	}
	_ = legacy.Close()

	for i := 0; i < 2; i++ { // second Open must be a no-op migration
		sqlDB, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d: %v", i+1, err)
		}
		rows, err := NewManifestRepo(sqlDB).LoadAll(context.Background())
		_ = sqlDB.Close()
		if err != nil {
			t.Fatalf("LoadAll #%d: %v", i+1, err)
		}
		got := rows["old.txt"]
		if s, m := got.RemoteStat(); s != 3 || m != 100 {
			t.Fatalf("legacy row RemoteStat = %d/%d, want 3/100 (fallback to local)", s, m)
		}
	}
}

func TestManifestRepo_RemoteStatRoundTrip(t *testing.T) {
	repo := openTestDB(t)
	ctx := context.Background()
	rows := []ManifestRow{
		{Path: "same.txt", Size: 1, Mtime: 10, Hash: "a", LastSynced: 1},
		{Path: "diff.txt", Size: 2, Mtime: 20, Hash: "b", LastSynced: 1, RemoteSize: 2, RemoteMtime: 99},
	}
	if err := repo.ApplyUpdates(ctx, rows, nil); err != nil {
		t.Fatal(err)
	}
	got, err := repo.LoadAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range rows {
		if got[want.Path] != want {
			t.Fatalf("%s: got %+v, want %+v", want.Path, got[want.Path], want)
		}
	}
	if s, m := got["diff.txt"].RemoteStat(); s != 2 || m != 99 {
		t.Fatalf("diff.txt RemoteStat = %d/%d, want 2/99", s, m)
	}
}

func TestRemoteStatView(t *testing.T) {
	same := map[string]ManifestRow{"a": {Path: "a", Size: 1, Mtime: 10}}
	if v := RemoteStatView(same); len(v) != 1 || v["a"].Mtime != 10 {
		t.Fatalf("unexpected view %+v", v)
	}
	mixed := map[string]ManifestRow{
		"a": {Path: "a", Size: 1, Mtime: 10},
		"b": {Path: "b", Size: 2, Mtime: 20, RemoteSize: 3, RemoteMtime: 30},
	}
	v := RemoteStatView(mixed)
	if v["a"].Size != 1 || v["a"].Mtime != 10 {
		t.Fatalf("a: %+v", v["a"])
	}
	if v["b"].Size != 3 || v["b"].Mtime != 30 {
		t.Fatalf("b: view must expose the remote stat, got %+v", v["b"])
	}
	if mixed["b"].Size != 2 {
		t.Fatal("RemoteStatView must not mutate its input")
	}
}

func TestManifestRepo_LoadPaths(t *testing.T) {
	repo := openTestDB(t)
	ctx := context.Background()
	var rows []ManifestRow
	want := map[string]struct{}{}
	for i := 0; i < manifestLoadChunk*2+7; i++ { // spans several chunks
		p := filepath.ToSlash(filepath.Join("d", string(rune('a'+i%26)), "f"+strconv.Itoa(i)))
		rows = append(rows, ManifestRow{Path: p, Size: int64(i), Mtime: 1, Hash: "h"})
		if i%3 == 0 {
			want[p] = struct{}{}
		}
	}
	if err := repo.ApplyUpdates(ctx, rows, nil); err != nil {
		t.Fatal(err)
	}
	want["missing/path"] = struct{}{}
	got, err := repo.LoadPaths(ctx, want)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want)-1 {
		t.Fatalf("got %d rows, want %d", len(got), len(want)-1)
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Fatalf("unrequested row %q returned", p)
		}
	}
}

func TestManifestRepo_LoadPrefixMatchesWholeSegments(t *testing.T) {
	repo := openTestDB(t)
	ctx := context.Background()
	var rows []ManifestRow
	for _, p := range []string{"a/b", "a/b/x", "a/b/c/y", "a/bc", "a/bc/z", "a/b0", "a/b.txt"} {
		rows = append(rows, ManifestRow{Path: p, Size: 1, Mtime: 1, Hash: "h"})
	}
	if err := repo.ApplyUpdates(ctx, rows, nil); err != nil {
		t.Fatal(err)
	}
	got, err := repo.LoadPrefix(ctx, "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want exactly a/b/x and a/b/c/y, got %v", got)
	}
	for _, p := range []string{"a/b/x", "a/b/c/y"} {
		if _, ok := got[p]; !ok {
			t.Fatalf("missing %s in %v", p, got)
		}
	}
}
