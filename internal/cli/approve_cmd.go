package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"syncd/internal/app"
	"syncd/internal/runtime"
)

var (
	approveLocal    string
	approveRemote   string
	approveStateDir string
	approveLockPath string
)

// approveDeletesCmd lets a running watch daemon carry out one cycle it has
// been refusing for exceeding --max-delete.
var approveDeletesCmd = &cobra.Command{
	Use:   "approve-deletes",
	Short: "Let a running watch daemon carry out one cycle it refused for exceeding --max-delete",
	Long: `When a 'syncd watch' cycle would delete more files than --max-delete allows,
the daemon skips it, logs why, and keeps skipping it: a mass delete is what a
wiped local tree or the wrong share behind the mount point looks like.

If the deletions are intended, run this with the daemon's --local/--remote
(and --state-dir/--lock-path, if the daemon was given them). The daemon's
next over-limit cycle then runs once. The approval is single-use and expires
after an hour.`,
	RunE: func(_ *cobra.Command, _ []string) error {
		localAbs, err := filepath.Abs(approveLocal)
		if err != nil {
			return fmt.Errorf("resolve --local: %w", err)
		}
		remoteAbs, err := filepath.Abs(approveRemote)
		if err != nil {
			return fmt.Errorf("resolve --remote: %w", err)
		}
		if _, err := app.ApproveDeletes(app.WatchConfig{
			LocalRoot: localAbs, RemoteRoot: remoteAbs, StateDir: approveStateDir, LockPath: approveLockPath,
		}); err != nil {
			return err
		}
		fmt.Printf("approved: the watch daemon for %s <-> %s may run one cycle over its delete limit within the next %s\n",
			localAbs, remoteAbs, runtime.DeleteApprovalTTL)
		return nil
	},
}

func init() {
	approveDeletesCmd.Flags().StringVar(&approveLocal, "local", "", "The daemon's local directory (required)")
	approveDeletesCmd.Flags().StringVar(&approveRemote, "remote", "", "The daemon's remote directory (required)")
	approveDeletesCmd.Flags().StringVar(&approveStateDir, "state-dir", "", "The daemon's --state-dir, if it was given one")
	approveDeletesCmd.Flags().StringVar(&approveLockPath, "lock-path", "", "The daemon's --lock-path, if it was given one")
	_ = approveDeletesCmd.MarkFlagRequired("local")
	_ = approveDeletesCmd.MarkFlagRequired("remote")
	rootCmd.AddCommand(approveDeletesCmd)
}
