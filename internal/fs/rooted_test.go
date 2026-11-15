package fs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRootedOperationsRejectSymlinkAncestors(t *testing.T) {
	root, outside, other := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(outside, "secret"), "private")
	writeFile(t, filepath.Join(other, "source"), "replacement")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for name, operation := range map[string]func() error{
		"read":               func() error { _, err := FileBLAKE3InRoot(root, "link/secret"); return err },
		"copy source":        func() error { return AtomicCopyFileInRoots(root, "link/secret", other, "copied") },
		"copy destination":   func() error { return AtomicCopyFileInRoots(other, "source", root, "link/secret") },
		"delete":             func() error { return RemoveInRoot(root, "link/secret") },
		"rename source":      func() error { return RenameInRoots(root, "link/secret", other, "moved") },
		"rename destination": func() error { return RenameInRoots(other, "source", root, "link/secret") },
		"mkdir":              func() error { return MkdirInRoot(root, "link/new") },
		"trash":              func() error { return NewTrash(t.TempDir()).Remove(root, "link/secret") },
	} {
		t.Run(name, func(t *testing.T) {
			if err := operation(); err == nil {
				t.Fatal("operation followed a symlink outside the root")
			}
		})
	}
	if b, err := os.ReadFile(filepath.Join(outside, "secret")); err != nil || string(b) != "private" {
		t.Fatalf("outside file changed: %q, %v", b, err)
	}
}

func TestAtomicCopyRejectsParentSwappedAfterValidation(t *testing.T) {
	source, dest, outside := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(source, "source"), "payload")
	if err := os.Mkdir(filepath.Join(dest, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	original := tempSiblingPathFunc
	t.Cleanup(func() { tempSiblingPathFunc = original })
	tempSiblingPathFunc = func(dst string) string {
		if err := os.Rename(filepath.Join(dest, "sub"), filepath.Join(dest, "old")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(dest, "sub")); err != nil {
			t.Fatal(err)
		}
		return original(dst)
	}
	if err := AtomicCopyFileInRoots(source, "source", dest, "sub/copied"); err == nil {
		t.Fatal("copy followed a swapped parent")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside directory modified: %v, %v", entries, err)
	}
}

func TestAtomicCopyRetainsSubsecondMtime(t *testing.T) {
	source, dest := t.TempDir(), t.TempDir()
	name := filepath.Join(source, "source")
	writeFile(t, name, "content")
	mtime := time.Unix(1700000000, 123456789)
	if err := os.Chtimes(name, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := AtomicCopyFileInRoots(source, "source", dest, "copied"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(dest, "copied"))
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("timestamp lost precision: %s -> %s", before.ModTime(), after.ModTime())
	}
}

func TestRootedCopyRejectsRootReplacedBySymlink(t *testing.T) {
	source, dest, outside := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(source, "source"), "payload")
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dest); err != nil {
		t.Fatal(err)
	}
	if err := AtomicCopyFileInRoots(source, "source", dest, "copied"); err == nil {
		t.Fatal("accepted a symlink root")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside directory modified: %v %v", entries, err)
	}
}
