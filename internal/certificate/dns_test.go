// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package certificate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-acme/lego/v5/challenge"
)

func TestDNSProviderUsesParametersAndRestoresEnvironment(t *testing.T) {
	t.Setenv("EXEC_PATH", "/original/provider")
	t.Setenv("EXEC_MODE", "original")
	provider, err := newDNS01Provider("exec", map[string]string{
		"EXEC_PATH": "/bin/true",
		"EXEC_MODE": "RAW",
	})
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("EXEC_PATH") != "/original/provider" || os.Getenv("EXEC_MODE") != "original" {
		t.Fatal("DNS provider parameters were not restored")
	}
	if err := provider.Present(t.Context(), "example.test", "token", "key-auth"); err != nil {
		t.Fatal(err)
	}
	if err := provider.CleanUp(t.Context(), "example.test", "token", "key-auth"); err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(challenge.ProviderTimeout); !ok {
		t.Fatal("lego provider timeout behavior was lost")
	}
	if _, ok := provider.(interface{ Sequential() time.Duration }); !ok {
		t.Fatal("lego provider sequential behavior was lost")
	}
}

func TestDNSProviderRestoresEnvironmentAfterError(t *testing.T) {
	const secret = "provider-secret-value"
	t.Setenv("PAGES_DNS_TEST_PARAMETER", "original")
	_, err := newDNS01Provider("unknown-pages-provider", map[string]string{"PAGES_DNS_TEST_PARAMETER": secret})
	if err == nil {
		t.Fatal("unknown DNS provider was accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("DNS provider error exposed a parameter value")
	}
	if os.Getenv("PAGES_DNS_TEST_PARAMETER") != "original" {
		t.Fatal("DNS provider parameter was not restored after an error")
	}
}

func TestConcurrentDNSProviderConstructionKeepsParametersIsolated(t *testing.T) {
	const count = 8
	root := t.TempDir()
	providers := make([]challenge.Provider, count)
	errs := make([]error, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range count {
		marker := filepath.Join(root, fmt.Sprintf("provider-%d.log", i))
		program := filepath.Join(root, fmt.Sprintf("provider-%d.sh", i))
		content := fmt.Sprintf("#!/bin/sh\nprintf x >> %q\n", marker)
		if err := os.WriteFile(program, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			providers[index], errs[index] = newDNS01Provider("exec", map[string]string{
				"EXEC_PATH": program,
				"EXEC_MODE": "RAW",
			})
		}(i)
	}
	close(start)
	wg.Wait()
	for i, provider := range providers {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if err := provider.Present(context.Background(), "example.test", "token", "key-auth"); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(root, fmt.Sprintf("provider-%d.log", i))
		content, err := os.ReadFile(marker)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != "x" {
			t.Fatalf("provider %d used another provider's parameters", i)
		}
	}
}
