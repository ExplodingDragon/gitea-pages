// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package standalone

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestReadyListenerSignalsBeforeRequestsAreServed(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	ready := make(chan struct{}, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	done := make(chan error, 1)
	go func() {
		done <- server.Serve(&readyListener{Listener: listener, ready: ready})
	}()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("listener did not start accepting connections")
	}
	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != http.ErrServerClosed {
		t.Fatalf("server error = %v", err)
	}
}
