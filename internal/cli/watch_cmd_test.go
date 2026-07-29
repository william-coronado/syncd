package cli

import (
	"strings"
	"testing"
	"time"
)

// TestValidateWatchFlags covers the CLI-level guard against the values that
// would otherwise reach watch.Run unchecked: --workers < 1 (pre-existing
// check) and a non-positive --remote-poll/--deep-scan, which panics
// time.NewTicker deep inside the watch loop if it ever gets there.
func TestValidateWatchFlags(t *testing.T) {
	cases := []struct {
		name                 string
		workers              int
		remotePoll, deepScan time.Duration
		wantErr              bool
		wantErrSubstr        string
	}{
		{name: "valid", workers: 1, remotePoll: 30 * time.Second, deepScan: time.Hour, wantErr: false},
		{name: "zero workers", workers: 0, remotePoll: 30 * time.Second, deepScan: time.Hour, wantErr: true, wantErrSubstr: "--workers"},
		{name: "zero remote-poll", workers: 1, remotePoll: 0, deepScan: time.Hour, wantErr: true, wantErrSubstr: "--remote-poll"},
		{name: "negative remote-poll", workers: 1, remotePoll: -time.Second, deepScan: time.Hour, wantErr: true, wantErrSubstr: "--remote-poll"},
		{name: "zero deep-scan", workers: 1, remotePoll: 30 * time.Second, deepScan: 0, wantErr: true, wantErrSubstr: "--deep-scan"},
		{name: "negative deep-scan", workers: 1, remotePoll: 30 * time.Second, deepScan: -time.Hour, wantErr: true, wantErrSubstr: "--deep-scan"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWatchFlags(tc.workers, tc.remotePoll, tc.deepScan)
			if tc.wantErr && err == nil {
				t.Fatalf("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantErrSubstr)
			}
		})
	}
}
