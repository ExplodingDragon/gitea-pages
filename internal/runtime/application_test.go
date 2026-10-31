// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestReadyListenerSignalsWhenServerAccepts(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{}, 1)
	server := &http.Server{Handler: http.NotFoundHandler()}
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(&readyListener{Listener: listener, ready: ready})
	}()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("listener did not start accepting connections")
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != http.ErrServerClosed {
		t.Fatalf("server error = %v", err)
	}
}

func TestRunStartsAndStopsBuiltInHTTP(t *testing.T) {
	registered := make(chan struct{}, 1)
	gitea := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/internal/pages/instance" {
			http.NotFound(w, r)
			return
		}
		var request registerRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode registration: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if err := json.NewEncoder(w).Encode(registerResponse{RootURL: request.RootURL, RootUpdatedUnixNano: 1}); err != nil {
			t.Errorf("encode registration: %v", err)
			return
		}
		registered <- struct{}{}
	}))
	defer gitea.Close()
	giteaURL, err := url.Parse(gitea.URL)
	if err != nil {
		t.Fatal(err)
	}
	rootURL, err := url.Parse("http://pages.example.test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	dataRoot := t.TempDir()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			GiteaURL:      giteaURL,
			Token:         "instance-token",
			RootURL:       rootURL,
			DataRoot:      dataRoot,
			HTTPListen:    "127.0.0.1:0",
			HTTPSMode:     HTTPSDisabled,
			SyncInterval:  time.Minute,
			DeployTimeout: time.Minute,
		})
	}()
	select {
	case <-registered:
		cancel()
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("Pages did not register after starting its listener")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Pages did not stop after cancellation")
	}
}
