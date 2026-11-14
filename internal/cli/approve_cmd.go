package cli

import (
	"errors"
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
(and --state-dir/--lock-path, if the daemon was given them). It shows the
deletions the daemon refused and approves exactly those: if the trees change
so that the daemon plans something else, the approval is not used. It is
single-use and expires after an hour.`,
	RunE: func(_ *cobra.Command, _ []string) error {
		localAbs, err := filepath.Abs(approveLocal)
		if err != nil {
			return fmt.Errorf("resolve --local: %w", err)
		}
		remoteAbs, err := filepath.Abs(approveRemote)
		if err != nil {
			return fmt.Errorf("resolve --remote: %w", err)
		}
		refusal, err := app.ApproveDeletes(app.WatchConfig{
			LocalRoot: localAbs, RemoteRoot: remoteAbs, StateDir: approveStateDir, LockPath: approveLockPath,
		})
		if errors.Is(err, runtime.ErrNoDeleteRefusal) {
			return fmt.Errorf("%w; there is nothing to approve (check --local/--remote, and --state-dir if the daemon was given one)", err)
		}
		if err != nil {
			return err
		}
		fmt.Printf("refused %s: %d deletion(s) from local and %d from remote, of %d tracked file(s)\n",
			refusal.Time.Local().Format("2006-01-02 15:04:05"), refusal.Local, refusal.Remote, refusal.Tracked)
		printRefusalSample("delete from local", refusal.SampleLocal, refusal.Local)
		printRefusalSample("delete from remote", refusal.SampleRemote, refusal.Remote)
		fmt.Printf("approved: the daemon may carry out exactly these deletions once, within the next %s\n", runtime.DeleteApprovalTTL)
		return nil
	},
}

func printRefusalSample(label string, sample []string, total int) {
	if total == 0 {
		return
	}
	fmt.Printf("  %s:\n", label)
	for _, p := range sample {
		fmt.Printf("    %s\n", p)
	}
	if more := total - len(sample); more > 0 {
		fmt.Printf("    ... and %d more\n", more)
	}
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
