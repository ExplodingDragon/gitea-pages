// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func runNginxCommand(parent context.Context, command []string, candidate string, timeout time.Duration) error {
	args := make([]string, len(command))
	for i, v := range command {
		args[i] = strings.ReplaceAll(v, "{candidate}", candidate)
	}
	if len(args) == 0 || args[0] == "" {
		return errors.New("empty configured command")
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func nginxQuote(s string) string { return strconv.Quote(s) }
func readOptionalFile(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return b, err == nil, err
}

func restoreFile(path string, data []byte, exists bool) error {
	if !exists {
		return os.Remove(path)
	}
	return atomicWriteFile(path, data, 0o644)
}

func snapshotMarkers(dir string) (map[string][]byte, error) {
	r := map[string][]byte{}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}
			r[e.Name()] = b
		}
	}
	return r, nil
}

func writeMarkers(dir string, markers map[string]nginxMarker, removeStale bool) error {
	keep := map[string]struct{}{"config-ready": {}}
	for _, m := range markers {
		name := filepath.Base(m.path)
		keep[name] = struct{}{}
		if err := atomicWriteFile(m.path, []byte(m.body), 0o644); err != nil {
			return err
		}
	}
	if !removeStale {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			if _, ok := keep[e.Name()]; !ok {
				if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
		}
	}
	return nil
}

func restoreMarkers(dir string, previous map[string][]byte) error {
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			if _, ok := previous[e.Name()]; !ok {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	for name, b := range previous {
		if err := atomicWriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}
