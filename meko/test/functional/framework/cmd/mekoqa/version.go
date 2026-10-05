package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the mekoqa version",
		RunE: func(c *cobra.Command, _ []string) error {
			out := c.OutOrStdout()
			if jsonOutput {
				return printEnvelope(out, "mekoqa.version", "version", map[string]any{"version": version})
			}
			fmt.Fprintln(out, version)
			return nil
		},
	}
}
