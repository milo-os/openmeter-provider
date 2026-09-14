// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"

	"go.miloapis.com/openmeter-provider/cmd/openmeter/controller-manager"
)

// Build metadata set via -ldflags at build time. See Dockerfile.
var (
	version      = "dev"
	gitCommit    = "unknown"
	gitTreeState = "unknown"
	buildDate    = "unknown"
)

func main() {
	root := controllermanager.NewRootCommand(controllermanager.BuildInfo{
		Version:      version,
		GitCommit:    gitCommit,
		GitTreeState: gitTreeState,
		BuildDate:    buildDate,
	})
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}
