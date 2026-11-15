package fs

import (
	"context"
	"errors"
	iofs "io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestScanRootReadFailureWithTrailingSlash(t *testing.T) {
	root := t.TempDir()
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	original := walkScanTree
	t.Cleanup(func() { walkScanTree = original })
	walkScanTree = func(_ iofs.FS, name string, visit iofs.WalkDirFunc) error {
		entry := iofs.FileInfoToDirEntry(info)
		if err := visit(name, entry, nil); err != nil {
			return err
		}
		err := visit(name, entry, syscall.EIO)
		if err == iofs.SkipDir {
			return nil
		}
		return err
	}
	matcher, err := LoadIgnoreMatcher(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ScanTree(context.Background(), root+"/", nil, false, matcher, HashNever, nil); !errors.Is(err, syscall.EIO) {
		t.Fatalf("expected root enumeration failure, got %v", err)
	}
}

func TestCopyCheckedPreservesNewEntryDuringPublish(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "file"), "incoming")
	writeFile(t, filepath.Join(dst, "file"), "original")
	source, err := OpenTree(src)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	dest, err := OpenTree(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer dest.Close()

	err = CopyChecked(source, "file", dest, "file", true, func(held *Tree, leaf string) error {
		hash, err := held.Hash(leaf)
		if err != nil || hash == "" {
			t.Fatalf("original was not held: %v", err)
		}
		return os.WriteFile(filepath.Join(dst, "file"), []byte("concurrent"), 0600)
	}, nil)
	if err == nil {
		t.Fatal("expected publication to defer")
	}
	if b, err := os.ReadFile(filepath.Join(dst, "file")); err != nil || string(b) != "concurrent" {
		t.Fatalf("concurrent data changed: %q %v", b, err)
	}
	retained, err := filepath.Glob(filepath.Join(dst, holdingPrefix+"*"))
	if err != nil || len(retained) != 1 {
		t.Fatalf("expected a recovery file, got %v %v", retained, err)
	}
	if b, err := os.ReadFile(retained[0]); err != nil || string(b) != "original" {
		t.Fatalf("original data changed: %q %v", b, err)
	}
	matcher, err := LoadIgnoreMatcher(dst)
	if err != nil {
		t.Fatal(err)
	}
	// The retained original is reported on every scan, never reconciled,
	// and does not stall the cycle (on a shared remote that would stall
	// every client).
	res, err := dest.Scan(context.Background(), ".", nil, false, matcher, HashNever, nil)
	if err != nil {
		t.Fatalf("scan aborted on a retained original: %v", err)
	}
	if len(res.Files) != 1 || len(res.Issues) != 1 || filepath.Join(dst, res.Issues[0].Path) != retained[0] {
		t.Fatalf("retained original not reported or reconciled: files=%v issues=%v", res.Files, res.Issues)
	}
}

func TestRemoveFromLeavesReplacementAtOriginalName(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "file"), "original")
	source, err := OpenTree(root)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	err = (*Trash)(nil).RemoveFrom(source, "file", func(held *Tree, leaf string) error {
		if _, err := held.Lstat(leaf); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(root, "file"), []byte("replacement"), 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "file")); err != nil || string(b) != "replacement" {
		t.Fatalf("replacement lost: %q %v", b, err)
	}
}

func TestTrashReservationRefusesSymlinkDirectory(t *testing.T) {
	root, state, outside := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "file"), "original")
	tr := NewTrash(filepath.Join(state, "trash"))
	if err := os.Symlink(outside, tr.Dir); err != nil {
		t.Fatal(err)
	}
	if err := tr.Remove(root, "file"); err == nil {
		t.Fatal("trash accepted a symlink directory")
	}
	if b, err := os.ReadFile(filepath.Join(root, "file")); err != nil || string(b) != "original" {
		t.Fatalf("source lost: %q %v", b, err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("outside trash directory changed: %v %v", entries, err)
	}
}

func TestCrossDeviceTrashLeavesReplacementAtOriginalName(t *testing.T) {
	root, trash := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(root, "file"), "original")
	old := moveIntoTrash
	t.Cleanup(func() { moveIntoTrash = old })
	moveIntoTrash = func(_ *Tree, _ string, _ *Tree, _ string, _ bool) error {
		if err := os.WriteFile(filepath.Join(root, "file"), []byte("replacement"), 0600); err != nil {
			return err
		}
		return unix.EXDEV
	}
	tr := NewTrash(trash)
	if err := tr.Remove(root, "file"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "file")); err != nil || string(b) != "replacement" {
		t.Fatalf("replacement lost: %q %v", b, err)
	}
	saved := filepath.Join(trash, trashNow().Format(trashDayLayout), RootFolder(root), "file")
	if b, err := os.ReadFile(saved); err != nil || string(b) != "original" {
		t.Fatalf("original lost: %q %v", b, err)
	}
}

func TestScanSkipsHoldingDirectoryWithIssue(t *testing.T) {
	root := t.TempDir()
	hold := filepath.Join(root, "sub", holdingPrefix+".f.tmp-1-1")
	if err := os.MkdirAll(hold, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hold, "f"), []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "g"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A held file (claimEntry's sibling name) is skipped the same way.
	heldFile := holdingPrefix + ".g.tmp-2-2"
	if err := os.WriteFile(filepath.Join(root, "sub", heldFile), []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}
	matcher, err := LoadIgnoreMatcher(root)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ScanTree(context.Background(), root, nil, false, matcher, HashNever, nil)
	if err != nil {
		t.Fatalf("holding directory aborted the scan: %v", err)
	}
	if _, ok := res.Files["sub/g"]; !ok || len(res.Files) != 1 {
		t.Fatalf("files = %v", res.Files)
	}
	if len(res.Issues) != 2 {
		t.Fatalf("held entries not reported: %v", res.Issues)
	}
	reported := map[string]bool{}
	for _, issue := range res.Issues {
		reported[issue.Path] = true
	}
	if !reported["sub/"+holdingPrefix+".f.tmp-1-1"] || !reported["sub/"+heldFile] {
		t.Fatalf("held entries not reported: %v", res.Issues)
	}
}

// A device lacking both exclusive primitives is remembered, so later
// publications there skip the two failing calls.
func TestPublishRemembersDeviceWithoutExclusivePrimitives(t *testing.T) {
	origRename, origLink := exclusiveRename, hardLink
	resetMemo := func() {
		noExclusive.Range(func(k, _ any) bool { noExclusive.Delete(k); return true })
	}
	t.Cleanup(func() { exclusiveRename, hardLink = origRename, origLink; resetMemo() })
	resetMemo()
	calls := 0
	exclusiveRename = func(*os.File, string, *os.File, string) error { calls++; return unix.ENOTSUP }
	hardLink = func(*os.File, string, *os.File, string) error { calls++; return unix.ENOTSUP }

	root := t.TempDir()
	for _, n := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := publishExclusive(dir, "a", dir, "a2"); err != nil || calls != 2 {
		t.Fatalf("first publish: err=%v calls=%d", err, calls)
	}
	if err := publishExclusive(dir, "b", dir, "a2"); !os.IsExist(err) || calls != 2 {
		t.Fatalf("memoized publish: err=%v calls=%d (want EEXIST, no new calls)", err, calls)
	}
}

// The descriptor-stack walk returns what per-call root walks returned.
func TestScanThroughDescriptorStackMatchesTree(t *testing.T) {
	root := t.TempDir()
	files := []string{"a/b/c/d/deep", "a/b/side", "a/x", "z/y/w", "top"}
	for _, f := range files {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	matcher, err := LoadIgnoreMatcher(root)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ScanTree(context.Background(), root, nil, false, matcher, HashAlways, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != len(files) {
		t.Fatalf("files = %v", res.Files)
	}
	for _, f := range files {
		want, err := FileBLAKE3(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			t.Fatal(err)
		}
		if res.Files[f].Hash != want {
			t.Fatalf("%s hash = %q, want %q", f, res.Files[f].Hash, want)
		}
	}
	for _, d := range []string{"a", "a/b", "a/b/c", "a/b/c/d", "z", "z/y"} {
		if _, ok := res.Dirs[d]; !ok {
			t.Fatalf("dir %s missing: %v", d, res.Dirs)
		}
	}
}

// Some SMB mounts support neither exclusive rename nor hard links. Deletes
// and overwrites must still work there, and an absent-destination publish
// must still refuse to replace an existing name.
func TestPublishWithoutExclusivePrimitives(t *testing.T) {
	origRename, origLink := exclusiveRename, hardLink
	t.Cleanup(func() { exclusiveRename, hardLink = origRename, origLink })
	exclusiveRename = func(*os.File, string, *os.File, string) error { return unix.ENOTSUP }
	hardLink = func(*os.File, string, *os.File, string) error { return unix.EPERM }

	src, dst := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "f"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := OpenTree(src)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d, err := OpenTree(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if err := CopyChecked(s, "f", d, "f", false, nil, nil); !os.IsExist(err) {
		t.Fatalf("absent publish over an existing name: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "f")); string(b) != "old" {
		t.Fatalf("existing destination replaced: %q", b)
	}
	if err := CopyChecked(s, "f", d, "f", true, func(*Tree, string) error { return nil }, nil); err != nil {
		t.Fatalf("checked overwrite: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "f")); string(b) != "new" {
		t.Fatalf("overwrite content = %q", b)
	}
	if err := (*Trash)(nil).RemoveFrom(d, "f", nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	entries, _ := os.ReadDir(dst)
	if len(entries) != 0 {
		t.Fatalf("leftovers after delete: %v", entries)
	}
}
