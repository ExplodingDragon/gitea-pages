// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package page

// RegisterRequest publishes the instance address configured by the operator.
type RegisterRequest struct {
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

const (
	DomainTXTPending        DomainTXTResult = "pending"
	DomainTXTValid          DomainTXTResult = "valid"
	DomainTXTInvalid        DomainTXTResult = "invalid"
	DomainTXTTemporaryError DomainTXTResult = "temporary_error"
)

// ArchiveLimits are the content bounds configured by Gitea for new deployments.
type ArchiveLimits struct {
	MaxTarGzBytes int64 `json:"max_tar_gz_bytes"`
	MaxSiteBytes  int64 `json:"max_site_bytes"`
	MaxFileBytes  int64 `json:"max_file_bytes"`
	MaxEntries    int   `json:"max_entries"`
}

// SitesResponse is one stable page of a full or incremental listing.
type SitesResponse struct {
	RootURL               string        `json:"root_url"`
	RootUpdatedUnixNano   int64         `json:"root_updated_unix_nano"`
	ArchiveLimits         ArchiveLimits `json:"archive_limits"`
	RequireGiteaTXTRecord bool          `json:"require_gitea_txt_record"`
	Until                 int64         `json:"until"`
	Sites                 []Site        `json:"sites"`
	NextCursor            *Cursor       `json:"next_cursor"`
}

// Cursor identifies the last site in a stable ordered response page.
type Cursor struct {
	PagesUpdatedUnixNano int64 `json:"pages_updated_unix_nano"`
	RepoID               int64 `json:"repo_id"`
}

// InstanceErrorCode is the stable deployment error reported to Gitea.
type InstanceErrorCode string

const (
	InstanceErrorCertificatePending    InstanceErrorCode = "certificate_pending"
	InstanceErrorContentDownloadFailed InstanceErrorCode = "content_download_failed"
	InstanceErrorContentInvalid        InstanceErrorCode = "content_invalid"
	InstanceErrorContentInstallFailed  InstanceErrorCode = "content_install_failed"
	InstanceErrorRouteApplyFailed      InstanceErrorCode = "route_apply_failed"
	InstanceErrorTXTInvalid            InstanceErrorCode = "txt_invalid"
	InstanceErrorACMEFailed            InstanceErrorCode = "acme_failed"
	InstanceErrorEntryProbeFailed      InstanceErrorCode = "entry_probe_failed"
)

// DeploymentResult describes the configuration and addresses actually active on this instance.
type DeploymentResult struct {
	RepoID                       int64             `json:"repo_id"`
	AppliedPagesUpdatedUnixNano  int64             `json:"applied_pages_updated_unix_nano"`
	AppliedRootUpdatedUnixNano   int64             `json:"applied_root_updated_unix_nano"`
	AppliedRequireGiteaTXTRecord bool              `json:"applied_require_gitea_txt_record"`
	ConfigApplied                bool              `json:"config_applied"`
	ActualTarGzSHA256            string            `json:"actual_tar_gz_sha256"`
	DefaultPublished             bool              `json:"default_published"`
	CustomPublished              bool              `json:"custom_published"`
	ErrorCode                    InstanceErrorCode `json:"error_code,omitempty"`
}

// APIError is a structured error returned by Gitea's Pages endpoints.
type APIError struct {
	Code string
}

func (e *APIError) Error() string {
	return e.Code
}

const apiErrorStateChanged = "state_changed"
