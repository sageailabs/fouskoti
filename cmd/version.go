// Copyright © The Sage Group plc or its licensors.

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

type VersionCommandOptions struct {
	Version string
	Commit  string
	Date    string
}

const VersionCommandName = "version"

func NewVersionCommand(options *VersionCommandOptions) *cobra.Command {
	command := &cobra.Command{
		Use:   VersionCommandName,
		Short: "Reports the tool version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("%s\n", options.Version)
		},
		SilenceUsage: true,
	}

	return command
}
