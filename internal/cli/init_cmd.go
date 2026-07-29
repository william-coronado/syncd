package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"syncd/internal/app"
	"syncd/internal/runtime"
)

var (
	initLocal  string
	initRemote string
)

// initCmd bootstraps the local/remote roots and the remote canary file used
// by the always-on sync daemon (app.Run --init). It performs no
// reconciliation; it just makes sure both directories exist and the remote
// side is marked as a valid sync target before the daemon's first real run.
var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Bootstrap local/remote roots and the remote canary file for always-on sync",
	RunE:  runInitCmd,
}

func init() {
	initCmd.Flags().StringVar(&initLocal, "local", "", "Local directory path (required)")
	initCmd.Flags().StringVar(&initRemote, "remote", "", "Remote directory path (required)")
	_ = initCmd.MarkFlagRequired("local")
	_ = initCmd.MarkFlagRequired("remote")

	rootCmd.AddCommand(initCmd)
}

func runInitCmd(_ *cobra.Command, _ []string) error {
	localAbs, err := filepath.Abs(initLocal)
	if err != nil {
		return fmt.Errorf("resolve --local: %w", err)
	}
	remoteAbs, err := filepath.Abs(initRemote)
	if err != nil {
		return fmt.Errorf("resolve --remote: %w", err)
	}

	logger, err := runtime.NewLogger(false, "")
	if err != nil {
		return err
	}
	defer logger.Close()

	return app.RunInit(localAbs, remoteAbs, logger)
}
