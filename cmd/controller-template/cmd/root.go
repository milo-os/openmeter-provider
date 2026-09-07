// SPDX-License-Identifier: AGPL-3.0-only

// Package cmd contains the cobra command definitions for the controller-template binary.
package cmd

import "github.com/spf13/cobra"

// BuildInfo carries version metadata injected at build time via -ldflags.
type BuildInfo struct {
	Version      string
	GitCommit    string
	GitTreeState string
	BuildDate    string
}

// NewRootCommand returns the controller-template root cobra command.
// It has no RunE — invoking 'controller-template' with no subcommand prints help.
func NewRootCommand(info BuildInfo) *cobra.Command {
	root := &cobra.Command{
		Use:   "controller-template",
		Short: "Milo control plane controller template",
		// No RunE — 'controller-template' with no subcommand prints help.
	}
	root.AddCommand(newOperatorCommand(info))
	return root
}
