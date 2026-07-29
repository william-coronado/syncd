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
