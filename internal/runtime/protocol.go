// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

// registerRequest publishes the instance address configured by the operator.
type registerRequest struct {
	RootURL string `json:"root_url"`
}

// registerResponse contains the address and Gitea-assigned change time.
type registerResponse struct {
	RootURL             string `json:"root_url"`
	RootUpdatedUnixNano int64  `json:"root_updated_unix_nano"`
}

// site is the current deployable state of one Gitea repository.
type site struct {
	RepoID               int64   `json:"repo_id"`
	Deleted              bool    `json:"deleted"`
	Enabled              bool    `json:"enabled"`
	OwnerName            string  `json:"owner"`
	RepoName             string  `json:"repository"`
	PagesUpdatedUnixNano int64   `json:"pages_updated_unix_nano"`
	TarGzSHA256          string  `json:"tar_gz_sha256"`
	CustomDomain         *domain `json:"custom_domain,omitempty"`
}

// domain is the current custom-domain state attached to a site.
type domain struct {
	ID                     int64           `json:"id"`
	Host                   string          `json:"host"`
	TXTToken               string          `json:"txt_token"`
	TXTResult              domainTXTResult `json:"txt_result"`
	TXTLastSuccessUnixNano int64           `json:"txt_last_success_unix_nano"`
}

// domainTXTResult is Gitea's latest ownership-check result for a custom domain.
type domainTXTResult string

const (
	domainTXTPending        domainTXTResult = "pending"
	domaintxtValid          domainTXTResult = "valid"
	domaintxtInvalid        domainTXTResult = "invalid"
	domaintxtTemporaryError domainTXTResult = "temporary_error"
)

// archiveLimits are the content bounds configured by Gitea for new deployments.
type archiveLimits struct {
	MaxTarGzBytes int64 `json:"max_tar_gz_bytes"`
	MaxSiteBytes  int64 `json:"max_site_bytes"`
	MaxFileBytes  int64 `json:"max_file_bytes"`
	MaxEntries    int   `json:"max_entries"`
}

// sitesResponse is one stable page of a full or incremental listing.
type sitesResponse struct {
	RootURL               string        `json:"root_url"`
	RootUpdatedUnixNano   int64         `json:"root_updated_unix_nano"`
	ArchiveLimits         archiveLimits `json:"archive_limits"`
	RequireGiteaTXTRecord bool          `json:"require_gitea_txt_record"`
	Until                 int64         `json:"until"`
	Sites                 []site        `json:"sites"`
	NextCursor            *cursor       `json:"next_cursor"`
}

// cursor identifies the last site in a stable ordered response page.
type cursor struct {
	PagesUpdatedUnixNano int64 `json:"pages_updated_unix_nano"`
	RepoID               int64 `json:"repo_id"`
}

// instanceErrorCode is the stable deployment error reported to Gitea.
type instanceErrorCode string

const (
	instanceErrorCertificatePending    instanceErrorCode = "certificate_pending"
	instanceErrorContentDownloadFailed instanceErrorCode = "content_download_failed"
	instanceErrorContentInvalid        instanceErrorCode = "content_invalid"
	instanceErrorContentInstallFailed  instanceErrorCode = "content_install_failed"
	instanceErrorRouteApplyFailed      instanceErrorCode = "route_apply_failed"
	instanceErrortxtInvalid            instanceErrorCode = "txt_invalid"
	instanceErrorACMEFailed            instanceErrorCode = "acme_failed"
	instanceErrorEntryProbeFailed      instanceErrorCode = "entry_probe_failed"
)

// deploymentResult describes the configuration and addresses actually active on this instance.
type deploymentResult struct {
	RepoID                       int64             `json:"repo_id"`
	AppliedPagesUpdatedUnixNano  int64             `json:"applied_pages_updated_unix_nano"`
	AppliedRootUpdatedUnixNano   int64             `json:"applied_root_updated_unix_nano"`
	AppliedRequireGiteaTXTRecord bool              `json:"applied_require_gitea_txt_record"`
	ConfigApplied                bool              `json:"config_applied"`
	ActualTarGzSHA256            string            `json:"actual_tar_gz_sha256"`
	DefaultPublished             bool              `json:"default_published"`
	CustomPublished              bool              `json:"custom_published"`
	ErrorCode                    instanceErrorCode `json:"error_code,omitempty"`
}

// apiError is a structured error returned by Gitea's Pages endpoints.
type apiError struct {
	Code string
}

func (e *apiError) Error() string {
	return e.Code
}

const apiErrorStateChanged = "state_changed"
