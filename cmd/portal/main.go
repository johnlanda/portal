// Package main is the entrypoint for the portal CLI.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/johnlanda/portal/internal/cli"
)

// Version is set at build time via -ldflags.
var Version = "dev"

func main() {
	rootCmd := &cobra.Command{
		Use:     "portal",
		Short:   "Portal creates secure Envoy reverse tunnels between Kubernetes clusters",
		Version: Version,
	}

	rootCmd.AddCommand(
		cli.NewGenerateCmd(),
		cli.NewRotateCertsCmd(),
		cli.NewConnectCmd(),
		cli.NewDisconnectCmd(),
		cli.NewStatusCmd(),
		cli.NewListCmd(),
		cli.NewExposeCmd(),
	)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
