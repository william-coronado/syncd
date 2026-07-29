package fs

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/text/unicode/norm"
)

func NormalizeRelativePath(root, absPath string) (string, error) {
	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		return "", fmt.Errorf("relative path: %w", err)
	}
	rel = filepath.ToSlash(rel)
	rel = strings.TrimPrefix(rel, "./")
	rel = norm.NFC.String(rel)
	return rel, nil
}
