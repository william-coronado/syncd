package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// RootPairKey is the exclusion identity shared by sync and watch. Unlike a
// manifest identity it is independent of root order and display/job name.
func RootPairKey(left, right string) (string, error) {
	a, err := canonicalLockRoot(left)
	if err != nil {
		return "", err
	}
	b, err := canonicalLockRoot(right)
	if err != nil {
		return "", err
	}
	if a > b {
		a, b = b, a
	}
	sum := sha256.Sum256([]byte(a + "\x00" + b))
	return "roots-" + hex.EncodeToString(sum[:6]), nil
}

func canonicalLockRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	// An initial sync may create the destination. Resolve its existing
	// ancestor so a symlinked parent has the same identity before and after.
	var tail string
	for cur := abs; ; cur = filepath.Dir(cur) {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, tail), nil
		}
		if !os.IsNotExist(err) || filepath.Dir(cur) == cur {
			return "", fmt.Errorf("resolve lock root %s: %w", root, err)
		}
		tail = filepath.Join(filepath.Base(cur), tail)
	}
}
