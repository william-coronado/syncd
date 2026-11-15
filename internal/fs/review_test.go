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
	retained, err := filepath.Glob(filepath.Join(dst, holdingPrefix+"*", "file"))
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
	if _, err := dest.Scan(context.Background(), ".", nil, false, matcher, HashNever, nil); err == nil {
		t.Fatal("scan continued while a displaced entry awaited recovery")
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
