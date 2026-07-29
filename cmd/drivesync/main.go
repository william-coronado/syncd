package main

import (
	"context"
	"fmt"
	"os"

	"drivesync/internal/app"
	"drivesync/internal/cli"
)

func main() {
	cfg, err := cli.ParseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(2)
	}

	ctx := context.Background()
	if err := app.Run(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}
