// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"

	"go.miloapis.com/controller-template/cmd/controller-template/cmd"
)

// Build metadata set via -ldflags at build time. See Dockerfile.
var (
	version      = "dev"
	gitCommit    = "unknown"
	gitTreeState = "unknown"
	buildDate    = "unknown"
)

func main() {
	root := cmd.NewRootCommand(cmd.BuildInfo{
		Version:      version,
		GitCommit:    gitCommit,
		GitTreeState: gitTreeState,
		BuildDate:    buildDate,
	})
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
