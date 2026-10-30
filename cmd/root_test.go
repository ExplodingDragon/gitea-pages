// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRootCommandListsServingModes(t *testing.T) {
	cmd := newRootCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"standalone", "nginx"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("help does not list %q:\n%s", expected, output.String())
		}
	}
}
