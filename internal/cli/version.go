package cli

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

var version string

// Version returns the build version, falling back to the module version.
func Version() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}

func newVersionCommand(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:               "version",
		Short:             "Print the b2bk version",
		Args:              cobra.NoArgs,
		DisableAutoGenTag: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(f.Streams.Out, Version())
			return err
		},
	}
}
