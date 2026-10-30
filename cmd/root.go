// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"github.com/spf13/cobra"

	"gitea.dev/page/cmd/nginx"
	"gitea.dev/page/cmd/standalone"
)

func newRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "gitea-pages",
		Short:        "Publish repository websites managed by Gitea",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
	}
	cmd.AddCommand(standalone.NewCommand(), nginx.NewCommand())
	return cmd
}
