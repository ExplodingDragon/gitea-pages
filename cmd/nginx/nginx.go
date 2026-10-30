// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package nginx defines the command that publishes through an external Nginx
// process.
package nginx

import (
	"github.com/spf13/cobra"

	"gitea.dev/page"
)

// NewCommand creates the Nginx serving command.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "nginx",
		Short: "Publish sites through an external Nginx process",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := page.LoadConfig(page.ModeNginx)
			if err != nil {
				return err
			}
			publisher, err := page.NewNginxPublisher(cfg)
			if err != nil {
				return err
			}
			service, err := page.NewService(cfg, publisher)
			if err != nil {
				return err
			}
			defer service.Close()
			return service.Run(cmd.Context())
		},
	}
}
