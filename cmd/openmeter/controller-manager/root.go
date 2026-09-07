// SPDX-License-Identifier: AGPL-3.0-only

// Package controllermanager contains the cobra command definitions for the
// openmeter-provider binary's controller-manager subcommand.
package controllermanager

import "github.com/spf13/cobra"

// BuildInfo carries version metadata injected at build time via -ldflags.
type BuildInfo struct {
	Version      string
	GitCommit    string
	GitTreeState string
	BuildDate    string
}

// NewRootCommand returns the openmeter-provider root cobra command.
// It has no RunE — invoking 'openmeter-provider' with no subcommand prints help.
func NewRootCommand(info BuildInfo) *cobra.Command {
	root := &cobra.Command{
		Use:   "openmeter-provider",
		Short: "Milo openmeter provider (controller-runtime manager)",
		// No RunE — 'openmeter-provider' with no subcommand prints help.
	}
	root.AddCommand(newControllerManagerCommand(info))
	return root
}
