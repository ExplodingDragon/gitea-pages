// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package gitea

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

func TestDownloadContentChecksSizeAndDigest(t *testing.T) {
	payload := []byte("compressed Pages artifact")
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer instance-token" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	base, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name    string
		digest  string
		limit   int64
		wantErr bool
	}{
		{name: "valid", digest: digest, limit: int64(len(payload))},
		{name: "too large", digest: digest, limit: int64(len(payload) - 1), wantErr: true},
		{name: "digest mismatch", digest: strings.Repeat("a", 64), limit: int64(len(payload)), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			destination, err := os.CreateTemp(t.TempDir(), "artifact-")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = destination.Close() }()
			err = NewClient(base, "instance-token").DownloadContent(t.Context(), 42, test.digest, test.limit, destination)
			if (err != nil) != test.wantErr {
				t.Fatalf("DownloadContent error = %v, wantErr %v", err, test.wantErr)
			}
			if test.wantErr {
				return
			}
			if _, err := destination.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(destination)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(payload) {
				t.Fatalf("downloaded content = %q", got)
			}
		})
	}
}
