package runtime

import (
	"fmt"
	"os"
	"time"
)

// DeleteApprovalTTL is how long a `syncd approve-deletes` token stays
// valid. It is short on purpose: the approval is for the mass delete the
// daemon is refusing right now, not for whichever one comes next.
const DeleteApprovalTTL = time.Hour

// WriteDeleteApproval records a one-shot approval at path.
func WriteDeleteApproval(path string) error {
	if err := os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return fmt.Errorf("write delete approval: %w", err)
	}
	return nil
}

// ConsumeDeleteApproval reports whether an unexpired approval exists at
// path, removing it either way so it can never authorize a second cycle.
func ConsumeDeleteApproval(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	_ = os.Remove(path)
	return time.Since(info.ModTime()) <= DeleteApprovalTTL
}
