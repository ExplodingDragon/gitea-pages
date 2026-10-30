// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// giteaClient calls the authenticated Gitea Pages synchronization endpoints.
type giteaClient struct {
	base  *url.URL
	token string
	http  *http.Client
}

func (c *giteaClient) registerInstance(ctx context.Context, root *url.URL) (registerResponse, error) {
	var out registerResponse
	err := c.doJSON(ctx, http.MethodPut, "/api/internal/pages/instance", registerRequest{RootURL: root.String()}, &out)
	return out, err
}

func (c *giteaClient) listSites(ctx context.Context, full bool, since, until, afterTime, afterRepo int64) (sitesResponse, error) {
	endpoint := "/api/internal/pages/sites"
	if !full {
		endpoint += "/updated"
	}
	q := url.Values{"limit": {"100"}}
	if !full {
		q.Set("since", strconv.FormatInt(since, 10))
	}
	if until > 0 {
		q.Set("until", strconv.FormatInt(until, 10))
	}
	if afterTime > 0 {
		q.Set("after_time", strconv.FormatInt(afterTime, 10))
		q.Set("after_repo_id", strconv.FormatInt(afterRepo, 10))
	}
	var out sitesResponse
	if err := c.doJSON(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil, &out); err != nil {
		return out, err
	}
	return out, nil
}

func (c *giteaClient) downloadContent(ctx context.Context, repoID int64, digest string, maxBytes int64, dst *os.File) error {
	if !isSHA256Digest(digest) || maxBytes <= 0 {
		return errors.New("invalid content digest")
	}
	endpoint := path.Join("/api/internal/pages/sites", strconv.FormatInt(repoID, 10), "content", digest)
	req, err := c.request(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return readAPIError(resp)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return err
	}
	if n > maxBytes {
		return fmt.Errorf("content archive exceeds %d bytes", maxBytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != digest {
		return fmt.Errorf("content digest mismatch: got %s", got)
	}
	return nil
}

func (c *giteaClient) reportResult(ctx context.Context, result deploymentResult) error {
	return c.doJSON(ctx, http.MethodPost, "/api/internal/pages/results", result, nil)
}

func (c *giteaClient) request(ctx context.Context, method, endpoint string, body any) (*http.Request, error) {
	parsed, err := url.ParseRequestURI(endpoint)
	if err != nil || parsed.Path == "" || parsed.Host != "" || parsed.Scheme != "" {
		return nil, fmt.Errorf("invalid internal endpoint %q", endpoint)
	}
	u := *c.base
	u.Path = path.Join(strings.TrimSuffix(c.base.Path, "/"), parsed.Path)
	u.RawQuery = parsed.RawQuery
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), r)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (c *giteaClient) doJSON(ctx context.Context, method, endpoint string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := c.request(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return readAPIError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4*1024*1024)).Decode(out)
}

func readAPIError(resp *http.Response) error {
	var v struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1*1024*1024)).Decode(&v)
	if v.Code == "" {
		v.Code = resp.Status
	}
	return &apiError{Code: v.Code}
}
