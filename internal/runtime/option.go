// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"log/slog"
	"net/http"
)

type serviceOptions struct {
	logger     *slog.Logger
	httpClient *http.Client
}

// Option customizes an embedded Pages service.
type Option func(*serviceOptions)

// WithLogger directs service and built-in Publisher logs to logger.
func WithLogger(logger *slog.Logger) Option {
	return func(options *serviceOptions) {
		if logger != nil {
			options.logger = logger
		}
	}
}

// WithHTTPClient sets the client used for authenticated Gitea communication.
func WithHTTPClient(client *http.Client) Option {
	return func(options *serviceOptions) {
		if client != nil {
			options.httpClient = client
		}
	}
}
