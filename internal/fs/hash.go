package fs

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"lukechampine.com/blake3"
)

func FileBLAKE3(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open hash source: %w", err)
	}
	defer f.Close()

	h := blake3.New(32, nil)
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash read: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// FileBLAKE3InRoot hashes through a confined descriptor, never through an
// absolute path whose parent might have been replaced by a symlink.
func FileBLAKE3InRoot(root, rel string) (string, error) {
	r, err := OpenTree(root)
	if err != nil {
		return "", err
	}
	defer r.Close()
	return hashRegularInRoot(r, rel)
}

func hashRegularInRoot(r *Tree, rel string) (string, error) {
	f, err := r.openRegular(rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return hashReader(f)
}

func hashReader(r io.Reader) (string, error) {
	h := blake3.New(32, nil)
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (t *Tree) Hash(rel string) (string, error) { return hashRegularInRoot(t, rel) }
