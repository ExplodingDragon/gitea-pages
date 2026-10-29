package page

type RegisterRequest struct {
	RootURL string `json:"root_url"`
}
type RegisterResponse struct {
	RootURL             string `json:"root_url"`
	RootUpdatedUnixNano int64  `json:"root_updated_unix_nano"`
}

type Site struct {
	RepoID               int64    `json:"repo_id"`
	Deleted              bool     `json:"deleted"`
	Enabled              bool     `json:"enabled"`
	OwnerName            string   `json:"owner"`
	RepoName             string   `json:"repository"`
	PagesUpdatedUnixNano int64    `json:"pages_updated_unix_nano"`
	TarGzSHA256          string   `json:"tar_gz_sha256"`
	CustomDomains        []Domain `json:"custom_domains,omitempty"`
}
type Domain struct {
	ID                     int64  `json:"id"`
	Host                   string `json:"host"`
	TXTToken               string `json:"txt_token"`
	TXTResult              string `json:"txt_result"`
	TXTLastSuccessUnixNano int64  `json:"txt_last_success_unix_nano"`
	State                  string `json:"state"`
}
type SitesResponse struct {
	RootURL                  string  `json:"root_url"`
	RootUpdatedUnixNano      int64   `json:"root_updated_unix_nano"`
	TXTVerificationEnabled   bool    `json:"txt_verification_enabled"`
	TXTPolicyUpdatedUnixNano int64   `json:"txt_policy_updated_unix_nano"`
	NextSince                int64   `json:"next_since"`
	Until                    int64   `json:"until"`
	Sites                    []Site  `json:"sites"`
	NextCursor               *Cursor `json:"next_cursor"`
}
type Cursor struct {
	PagesUpdatedUnixNano int64 `json:"pages_updated_unix_nano"`
	RepoID               int64 `json:"repo_id"`
}

type Result struct {
	RepoID                          int64  `json:"repo_id"`
	AppliedPagesUpdatedUnixNano     int64  `json:"applied_pages_updated_unix_nano"`
	AppliedRootUpdatedUnixNano      int64  `json:"applied_root_updated_unix_nano"`
	AppliedTXTPolicyUpdatedUnixNano int64  `json:"applied_txt_policy_updated_unix_nano"`
	ConfigApplied                   bool   `json:"config_applied"`
	ActualTarGzSHA256               string `json:"actual_tar_gz_sha256"`
	DefaultPublished                bool   `json:"default_published"`
	CustomPublished                 bool   `json:"custom_published"`
	ErrorCode                       string `json:"error_code,omitempty"`
}

type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return e.Code
	}
	return e.Message
}
