package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "syncd",
	Short: "Fast ad-hoc and always-on directory synchronization",
	// Execute prints the returned error itself; without this cobra prints
	// it too, so every failure showed up twice.
	SilenceErrors: true,
	// Usage help is for mistakes in how the command was invoked (unknown
	// flag, missing required flag), which cobra reports before any
	// PersistentPreRun. Once a command actually starts running, a runtime
	// failure (missing source dir, held lock, failed copy) must not bury
	// its error under the full flag listing.
	PersistentPreRun: func(cmd *cobra.Command, _ []string) {
		cmd.SilenceUsage = true
	},
}

// Execute runs the root command; each subcommand registers itself in its own file's init().
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
