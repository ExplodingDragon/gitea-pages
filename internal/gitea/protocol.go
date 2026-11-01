// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gitea

import "gitea.dev/page/internal/artifact"

// registerRequest publishes the instance address configured by the operator.
type registerRequest struct {
	RootURL string `json:"root_url"`
}

// RegisterResponse contains the address and Gitea-assigned change time.
type RegisterResponse struct {
	RootURL             string `json:"root_url"`
	RootUpdatedUnixNano int64  `json:"root_updated_unix_nano"`
}

// Site is the current deployable state of one Gitea repository.
type Site struct {
	RepoID               int64   `json:"repo_id"`
	Deleted              bool    `json:"deleted"`
	Enabled              bool    `json:"enabled"`
	OwnerName            string  `json:"owner"`
	RepoName             string  `json:"repository"`
	PagesUpdatedUnixNano int64   `json:"pages_updated_unix_nano"`
	TarGzSHA256          string  `json:"tar_gz_sha256"`
	CustomDomain         *Domain `json:"custom_domain,omitempty"`
}

// Domain is the current custom-domain state attached to a site.
type Domain struct {
	ID                     int64           `json:"id"`
	Host                   string          `json:"host"`
	TXTToken               string          `json:"txt_token"`
	TXTResult              DomainTXTResult `json:"txt_result"`
	TXTLastSuccessUnixNano int64           `json:"txt_last_success_unix_nano"`
}

// DomainTXTResult is Gitea's latest ownership-check result for a custom domain.
type DomainTXTResult string

// DomainTXTValid means Gitea last confirmed the ownership record.
const DomainTXTValid DomainTXTResult = "valid"

// SitesResponse is one stable page of a full or incremental listing.
type SitesResponse struct {
	RootURL               string          `json:"root_url"`
	RootUpdatedUnixNano   int64           `json:"root_updated_unix_nano"`
	ArchiveLimits         artifact.Limits `json:"archive_limits"`
	RequireGiteaTXTRecord bool            `json:"require_gitea_txt_record"`
	Until                 int64           `json:"until"`
	Sites                 []Site          `json:"sites"`
	NextCursor            *Cursor         `json:"next_cursor"`
}

// Cursor identifies the last site in a stable ordered response page.
type Cursor struct {
	PagesUpdatedUnixNano int64 `json:"pages_updated_unix_nano"`
	RepoID               int64 `json:"repo_id"`
}

// ErrorCode is the stable deployment error reported to Gitea.
type ErrorCode string

const (
	ErrorCertificatePending    ErrorCode = "certificate_pending"
	ErrorContentDownloadFailed ErrorCode = "content_download_failed"
	ErrorContentInvalid        ErrorCode = "content_invalid"
	ErrorContentInstallFailed  ErrorCode = "content_install_failed"
	ErrorRouteApplyFailed      ErrorCode = "route_apply_failed"
	ErrorTXTInvalid            ErrorCode = "txt_invalid"
	ErrorACMEFailed            ErrorCode = "acme_failed"
	ErrorEntryProbeFailed      ErrorCode = "entry_probe_failed"
)

// DeploymentResult describes the configuration and addresses actually active on this instance.
type DeploymentResult struct {
	RepoID                       int64     `json:"repo_id"`
	AppliedPagesUpdatedUnixNano  int64     `json:"applied_pages_updated_unix_nano"`
	AppliedRootUpdatedUnixNano   int64     `json:"applied_root_updated_unix_nano"`
	AppliedRequireGiteaTXTRecord bool      `json:"applied_require_gitea_txt_record"`
	ConfigApplied                bool      `json:"config_applied"`
	ActualTarGzSHA256            string    `json:"actual_tar_gz_sha256"`
	DefaultPublished             bool      `json:"default_published"`
	CustomPublished              bool      `json:"custom_published"`
	ErrorCode                    ErrorCode `json:"error_code,omitempty"`
}

// APIError is a structured error returned by Gitea's Pages endpoints.
type APIError struct {
	Code string
}

func (e *APIError) Error() string {
	return e.Code
}

// APIErrorStateChanged asks Pages to synchronize the latest state before retrying.
const APIErrorStateChanged = "state_changed"
