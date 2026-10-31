// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// Package page synchronizes repository websites from Gitea and publishes
// validated content through a caller-provided Publisher.
package page

import (
	"context"
	"log/slog"
	"net/http"

	"gitea.dev/page/internal/runtime"
)

// Config contains the synchronization, storage, publication, DNS, and TLS
// settings used by Service and the built-in publisher.
type Config = runtime.Config

// HTTPSMode identifies which component owns the public HTTPS entry.
type HTTPSMode = runtime.HTTPSMode

const (
	HTTPSDisabled = runtime.HTTPSDisabled
	HTTPSPages    = runtime.HTTPSPages
	HTTPSProxy    = runtime.HTTPSProxy
)

// LoadConfig reads and validates one Pages YAML configuration file.
func LoadConfig(path string) (Config, error) { return runtime.LoadConfig(path) }

// Run starts the built-in HTTP publisher and synchronization service.
func Run(ctx context.Context, cfg Config, options ...Option) error {
	return runtime.Run(ctx, cfg, options...)
}

// Content is an immutable, validated website directory prepared by Service.
type Content = runtime.Content

// Deployment is the complete desired publication for one repository.
type Deployment = runtime.Deployment

// PublicationProblem describes an address failure after its configuration was
// applied.
type PublicationProblem = runtime.PublicationProblem

const (
	PublicationProblemNone               = runtime.PublicationProblemNone
	PublicationProblemCertificatePending = runtime.PublicationProblemCertificatePending
	PublicationProblemACMEFailed         = runtime.PublicationProblemACMEFailed
	PublicationProblemEntryProbeFailed   = runtime.PublicationProblemEntryProbeFailed
)

// Publication records the addresses confirmed by a Publisher.
type Publication = runtime.Publication

// ActiveDeployment is one locally confirmed deployment restored at startup.
type ActiveDeployment = runtime.ActiveDeployment

// Deployments streams active deployments without copying the complete local
// site set into another collection.
type Deployments = runtime.Deployments

// Publisher applies website content to visitor-facing routes.
type Publisher = runtime.Publisher

// Option customizes an embedded Pages service.
type Option = runtime.Option

// WithLogger directs service and built-in publisher logs to logger.
func WithLogger(logger *slog.Logger) Option { return runtime.WithLogger(logger) }

// WithHTTPClient sets the client used for authenticated Gitea communication.
func WithHTTPClient(client *http.Client) Option { return runtime.WithHTTPClient(client) }

// Service synchronizes Gitea Pages state and publishes validated content.
type Service = runtime.Service

// NewService creates an embeddable Pages service for a Publisher.
func NewService(cfg Config, publisher Publisher, options ...Option) (*Service, error) {
	return runtime.NewService(cfg, publisher, options...)
}
