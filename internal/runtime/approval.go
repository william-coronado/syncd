package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// DeleteApprovalTTL is how long a `syncd approve-deletes` token stays
// valid. It is short on purpose: the approval is for the mass delete the
// daemon is refusing right now, not for whichever one comes next.
const DeleteApprovalTTL = time.Hour

// DeleteRefusal records the cycle a watch daemon most recently refused for
// exceeding its delete limit, so `syncd approve-deletes` can show the user
// what they are approving and bind the approval to exactly that plan.
type DeleteRefusal struct {
	Time time.Time `json:"time"`
	// Fingerprint identifies the refused set of deletions.
	Fingerprint string `json:"fingerprint"`
	Local       int    `json:"delete_local"`
	Remote      int    `json:"delete_remote"`
	Tracked     int    `json:"tracked"`
	// SampleLocal / SampleRemote are the first few paths of each kind.
	SampleLocal  []string `json:"sample_local,omitempty"`
	SampleRemote []string `json:"sample_remote,omitempty"`
}

// ErrNoDeleteRefusal reports that no refused cycle is on record.
var ErrNoDeleteRefusal = errors.New("the watch daemon for this pair has not refused any cycle")

// WriteDeleteRefusal records r at path, replacing any earlier record.
func WriteDeleteRefusal(path string, r DeleteRefusal) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("record refused deletions: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("record refused deletions: %w", err)
	}
	return nil
}

// ReadDeleteRefusal returns the record at path, or ErrNoDeleteRefusal.
func ReadDeleteRefusal(path string) (DeleteRefusal, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DeleteRefusal{}, ErrNoDeleteRefusal
		}
		return DeleteRefusal{}, fmt.Errorf("read refused deletions: %w", err)
	}
	var r DeleteRefusal
	if err := json.Unmarshal(b, &r); err != nil {
		return DeleteRefusal{}, fmt.Errorf("read refused deletions %s: %w", path, err)
	}
	return r, nil
}

// WriteDeleteApproval records, at path, a one-shot approval of the plan
// identified by fingerprint.
func WriteDeleteApproval(path, fingerprint string) error {
	if err := os.WriteFile(path, []byte(fingerprint+"\n"), 0o600); err != nil {
		return fmt.Errorf("write delete approval: %w", err)
	}
	return nil
}

// ConsumeDeleteApproval reports whether an unexpired approval for exactly
// the plan identified by fingerprint exists at path, and uses it up.
//
//   - An expired token is removed and never honored.
//   - A token for a different plan is left in place (until it expires): the
//     trees may have changed since it was written, or this may be a
//     different cycle (a scoped batch) from the one that was approved, whose
//     turn is still to come.
//   - A matching token is honored only if it could be removed; one that
//     can't be would otherwise approve every later cycle too.
func ConsumeDeleteApproval(path, fingerprint string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if time.Since(info.ModTime()) > DeleteApprovalTTL {
		_ = os.Remove(path)
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil || fingerprint == "" || strings.TrimSpace(string(b)) != fingerprint {
		return false
	}
	return os.Remove(path) == nil
}
