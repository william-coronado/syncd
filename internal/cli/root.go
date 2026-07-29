package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "syncd",
	Short: "Fast ad-hoc and always-on directory synchronization",
}

// Execute runs the root command; each subcommand registers itself in its own file's init().
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
