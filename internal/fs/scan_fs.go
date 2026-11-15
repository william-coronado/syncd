package fs

import (
	"io"
	iofs "io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// scanFS serves one depth-first walk (Tree.Scan) from descriptors of the
// directories on the current path, instead of re-resolving every
// directory and every hashed file from the root. Tree's per-call walk
// costs one openat per path component; on a network share each is a round
// trip, so a full scan paid depth-many opens per directory and per hashed
// file.
//
// Confinement is unchanged in kind: each directory is still opened with
// O_NOFOLLOW|O_DIRECTORY from its (already confined) parent descriptor,
// exactly one step of walkDirectory. What differs is only when ancestors
// are resolved - once per visit rather than once per call - so a subtree
// renamed elsewhere mid-scan is read where it was found, as a walk already
// in progress through that directory would read it.
//
// At most depth+1 descriptors are open: entering a directory closes those
// that are not its ancestors.
type scanFS struct {
	t     *Tree
	stack []scanDir
}

type scanDir struct {
	rel string // slash-separated, "." for the walk's root
	f   *os.File
}

func newScanFS(t *Tree) *scanFS { return &scanFS{t: t} }

func (s *scanFS) close() {
	for _, d := range s.stack {
		d.f.Close()
	}
	s.stack = nil
}

// dir returns a descriptor for rel, owned by the stack.
func (s *scanFS) dir(rel string) (*os.File, error) {
	rel = path.Clean(rel)
	for len(s.stack) > 0 {
		top := s.stack[len(s.stack)-1]
		if top.rel == rel {
			return top.f, nil
		}
		if top.rel == "." || strings.HasPrefix(rel, top.rel+"/") {
			break
		}
		top.f.Close()
		s.stack = s.stack[:len(s.stack)-1]
	}
	var f *os.File
	var err error
	if n := len(s.stack); n > 0 && path.Dir(rel) == s.stack[n-1].rel {
		f, err = openChildDir(s.stack[n-1].f, path.Base(rel))
	} else {
		f, err = s.t.directory(filepath.FromSlash(rel), 0)
	}
	if err != nil {
		return nil, err
	}
	s.stack = append(s.stack, scanDir{rel: rel, f: f})
	return f, nil
}

func (s *scanFS) Open(name string) (iofs.File, error) { return s.t.Open(name) }

func (s *scanFS) Stat(name string) (os.FileInfo, error) { return s.t.Stat(name) }

func (s *scanFS) ReadDir(name string) ([]iofs.DirEntry, error) {
	d, err := s.dir(name)
	if err != nil {
		return nil, err
	}
	// The descriptor is shared with later lookups in this directory; read
	// from the start whatever an earlier read left behind.
	if _, err := d.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return readDirAt(d)
}

// hash hashes the regular file rel through its parent's descriptor.
func (s *scanFS) hash(rel string) (string, error) {
	d, err := s.dir(path.Dir(rel))
	if err != nil {
		return "", err
	}
	f, err := openRegularAt(d, path.Base(rel))
	if err != nil {
		return "", err
	}
	defer f.Close()
	return hashReader(f)
}
