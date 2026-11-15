package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
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
	// Fold aliases conservatively, including for missing roots and network
	// mounts whose case behavior is not known. Only the lock is folded.
	fold := cases.Fold()
	return pairKey(fold.String(norm.NFC.String(a)), fold.String(norm.NFC.String(b))), nil
}

func pairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	sum := sha256.Sum256([]byte(a + "\x00" + b))
	return "roots-" + hex.EncodeToString(sum[:6])
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

// AcquirePairLocks takes the common lock plus the known pre-upgrade lock
// names. Old watch used ordered resolved paths, and old sync used unordered
// absolute paths. Hold all of them for the full run to prevent mixed versions
// of syncd from touching the same configured pair.
func AcquirePairLocks(primary, stateDir, left, right string) (func(), error) {
	key, err := RootPairKey(left, right)
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	shared := filepath.Join(home, ".local", "state", "syncd")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		return nil, err
	}

	canonicalLeft, err := canonicalLockRoot(left)
	if err != nil {
		return nil, err
	}
	canonicalRight, err := canonicalLockRoot(right)
	if err != nil {
		return nil, err
	}
	absLeft, err := filepath.Abs(left)
	if err != nil {
		return nil, err
	}
	absRight, err := filepath.Abs(right)
	if err != nil {
		return nil, err
	}
	names := []string{"sync-" + key + ".lock", "sync-" + pairKey(absLeft, absRight) + ".lock", "sync-" + pairKey(canonicalLeft, canonicalRight) + ".lock"}
	for _, pair := range [][2]string{{canonicalLeft, canonicalRight}, {canonicalRight, canonicalLeft}} {
		sum := sha256.Sum256([]byte(pair[0] + "\x00" + pair[1]))
		names = append(names, "sync-"+hex.EncodeToString(sum[:6])+".lock")
	}
	// Old watch retained relative spelling when invoked with relative paths.
	for _, pair := range [][2]string{{left, right}, {right, left}} {
		a, aerr := filepath.EvalSymlinks(pair[0])
		b, berr := filepath.EvalSymlinks(pair[1])
		if aerr != nil || berr != nil {
			continue
		}
		sum := sha256.Sum256([]byte(a + "\x00" + b))
		names = append(names, "sync-"+hex.EncodeToString(sum[:6])+".lock")
	}

	paths := []string{primary}
	for _, name := range names {
		paths = append(paths, filepath.Join(shared, name), filepath.Join(stateDir, name))
	}
	return acquireLocks(paths)
}
