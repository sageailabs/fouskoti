// Copyright © The Sage Group plc or its licensors.

package main

import (
	"os"
	"slices"

	"github.com/vladlosev/fouskoti/cmd"
)

var (
	version = ""
	commit  = ""
	date    = ""
)

func main() {
	options := cmd.RootCommandOptions{
		VersionCommandOptions: cmd.VersionCommandOptions{
			Version: version,
			Commit:  commit,
			Date:    date,
		},
	}
	rootCommand := cmd.NewRootCommand(&options)

	childCommand, _, _ := rootCommand.Find(os.Args[1:])
	if childCommand == rootCommand {
		os.Args = slices.Insert(os.Args, 1, cmd.ExpandCommandName)
	}

	err := rootCommand.Execute()
	if err != nil {
		os.Exit(1)
	}
}
