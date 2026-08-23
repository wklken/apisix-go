package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wklken/apisix-go/pkg/version"
)

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print apisix-go version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			info := []string{
				"Version: " + version.Version,
				"Commit: " + version.Commit,
				"Build Time: " + version.BuildTime,
				"Go Version: " + version.GoVersion,
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), strings.Join(info, "\n"))
		},
	}
}
