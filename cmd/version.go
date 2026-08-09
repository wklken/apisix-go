package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var (
	version   = "0.1.0"
	gitCommit = ""
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print apisix-go version",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), version)
		if gitCommit != "" {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "commit: %s\n", gitCommit)
		}
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
