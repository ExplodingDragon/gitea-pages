// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package page

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// NginxPublisher renders, validates, and activates the external Nginx entry.
type NginxPublisher struct {
	cfg            Config
	store          *Store
	resources      platformResources
	removeSnapshot func(string) error
}
type nginxLocation struct{ base, root, marker string }
type nginxMarker struct{ path, body string }

// NewNginxPublisher prepares the configured Nginx output directories.
func NewNginxPublisher(cfg Config, store *Store) (*NginxPublisher, error) {
	if err := os.MkdirAll(cfg.NginxRoot, 0o755); err != nil {
		return nil, err
	}
	if cfg.NginxHTTP01Root != "" {
		if info, err := os.Lstat(cfg.NginxHTTP01Root); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("nginx HTTP-01 directory must not be a symbolic link")
		} else if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err := os.MkdirAll(cfg.NginxHTTP01Root, 0o755); err != nil {
			return nil, err
		}
		realHTTP01, err := filepath.EvalSymlinks(cfg.NginxHTTP01Root)
		if err != nil {
			return nil, err
		}
		realData, err := filepath.EvalSymlinks(cfg.DataRoot)
		if err != nil {
			return nil, err
		}
		realNginx, err := filepath.EvalSymlinks(cfg.NginxRoot)
		if err != nil {
			return nil, err
		}
		if pathWithin(realData, realHTTP01) || pathWithin(realNginx, realHTTP01) {
			return nil, errors.New("nginx HTTP-01 directory contains Pages data or output")
		}
		// An interrupted lego order is restarted as a fresh order. Remove its
		// old challenge files before publishing this process's configuration.
		entries, err := os.ReadDir(cfg.NginxHTTP01Root)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if err := os.RemoveAll(filepath.Join(cfg.NginxHTTP01Root, entry.Name())); err != nil {
				return nil, err
			}
		}
	}
	resources, err := loadPlatformResources(cfg.CustomRoot)
	if err != nil {
		return nil, err
	}
	p := &NginxPublisher{cfg: cfg, store: store, resources: resources, removeSnapshot: os.RemoveAll}
	if err := p.exportPublic(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *NginxPublisher) exportPublic() error {
	root := filepath.Join(p.cfg.NginxRoot, "public")
	currentAssets := make(map[string]struct{})
	for name, body := range p.resources.files {
		if name != "index.html" && name != "errors/404.html" && !strings.HasPrefix(name, "assets/") {
			continue
		}
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := atomicWriteFile(target, body, 0o644); err != nil {
			return err
		}
		if strings.HasPrefix(name, "assets/") {
			currentAssets[filepath.Clean(target)] = struct{}{}
		}
	}

	assetsRoot := filepath.Join(root, "assets")
	directories := make([]string, 0)
	if err := filepath.WalkDir(assetsRoot, func(name string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if name != assetsRoot {
				directories = append(directories, name)
			}
			return nil
		}
		if _, ok := currentAssets[filepath.Clean(name)]; ok {
			return nil
		}
		return os.Remove(name)
	}); err != nil && !os.IsNotExist(err) {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		_ = os.Remove(directories[i])
	}
	return nil
}

func (p *NginxPublisher) challengeDir(host string) string {
	host = normalizeHost(host)
	if host == "" || strings.ContainsAny(host, "/\\") {
		return ""
	}
	return filepath.Join(p.cfg.NginxHTTP01Root, host)
}

func (p *NginxPublisher) SetChallenge(host, token, key string) {
	if p.cfg.NginxHTTP01Root == "" || token == "" || token != filepath.Base(token) || strings.ContainsAny(token, `/\\`) {
		return
	}
	d := p.challengeDir(host)
	if d == "" {
		return
	}
	_ = os.MkdirAll(d, 0o755)
	_ = atomicWriteFile(filepath.Join(d, token), []byte(key), 0o644)
}

func (p *NginxPublisher) ClearChallenge(host, token string) {
	if p.cfg.NginxHTTP01Root == "" || token == "" || token != filepath.Base(token) || strings.ContainsAny(token, `/\\`) {
		return
	}
	if d := p.challengeDir(host); d != "" {
		_ = os.Remove(filepath.Join(d, token))
	}
}

func (p *NginxPublisher) Apply(ctx context.Context) error {
	// Keep the certificate set stable from rendering through reload. Otherwise
	// a concurrent renewal could change a certificate after it was included in
	// the ready-state digest but before nginx loaded the generated config.
	certificateFilesMu.Lock()
	defer certificateFilesMu.Unlock()

	initialized, rootURL, sites := p.store.routingSnapshot()
	// An interrupted full listing or an instance root change has no trusted
	// route set. The generated entry must therefore contain no site routes.
	if !initialized || rootURL != p.cfg.RootURL.String() {
		sites = nil
	}
	hosts := make(map[string][]nginxLocation)
	markers := make(map[string]nginxMarker)
	for _, site := range sites {
		if site.Site.Deleted || !site.Site.Enabled {
			continue
		}
		// An active custom Host needs an HTTP challenge endpoint before it has
		// a certificate or a published site route. The challenge file itself is
		// created only while the authorized ACME flow is running.
		if site.Site.CustomDomain != nil {
			host := normalizeHost(site.Site.CustomDomain.Host)
			if _, ok := hosts[host]; !ok {
				hosts[host] = nil
			}
		}
		host, base, ok := defaultSiteRoute(site.Site, p.cfg.RootURL.Hostname())
		if ok {
			loc := nginxLocation{base, p.store.SiteDir(site.Site.RepoID) + "/current/", p.routeMarker(host, base, site.Site.RepoID)}
			hosts[host] = append(hosts[host], loc)
			if site.InstalledDigest != "" && site.DefaultPublished {
				markers[loc.marker] = nginxMarker{loc.marker, markerBody(host, loc.base, site.Site.RepoID)}
			}
		}
		if site.Site.CustomDomain != nil {
			host := normalizeHost(site.Site.CustomDomain.Host)
			loc := nginxLocation{"/", p.store.SiteDir(site.Site.RepoID) + "/current/", p.routeMarker(host, "/", site.Site.RepoID)}
			hosts[host] = append(hosts[host], loc)
			if site.InstalledDigest != "" && site.CustomPublished {
				markers[loc.marker] = nginxMarker{loc.marker, markerBody(host, "/", site.Site.RepoID)}
			}
		}
	}
	rootHost := normalizeHost(p.cfg.RootURL.Hostname())
	if _, ok := hosts[rootHost]; !ok {
		hosts[rootHost] = nil
	}
	config, err := p.render(hosts)
	if err != nil {
		return err
	}
	certificateDigest, err := p.certificateDigest()
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(config))
	digest := hex.EncodeToString(sum[:])
	readyState := digest + "\n" + certificateDigest + "\n"
	target := filepath.Join(p.cfg.NginxRoot, "pages.conf")
	targetMatches := false
	if info, e := os.Lstat(target); e == nil && info.Mode().IsRegular() {
		if old, readErr := os.ReadFile(target); readErr == nil {
			targetMatches = string(old) == config
		}
	}
	markerDir := filepath.Join(p.cfg.DataRoot, "route-state")
	if targetMatches && p.confirmedReadyState() == readyState {
		if err := os.MkdirAll(markerDir, 0o755); err != nil {
			return err
		}
		previousMarkers, err := snapshotMarkers(markerDir)
		if err != nil {
			return err
		}
		if err := writeMarkers(markerDir, markers, true); err != nil {
			_ = restoreMarkers(markerDir, previousMarkers)
			return fmt.Errorf("write nginx route markers: %w", err)
		}
		p.cleanupSnapshots(digest)
		return nil
	}
	confDir := filepath.Join(p.cfg.NginxRoot, "config", "snapshots", digest)
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return err
	}
	candidate := filepath.Join(confDir, "pages.conf")
	if err := os.WriteFile(candidate, []byte(config), 0o644); err != nil {
		return err
	}
	if err := runNginxCommand(ctx, p.cfg.NginxTestCommand, candidate, p.cfg.NginxCommandTimeout); err != nil {
		return fmt.Errorf("nginx config test: %w", err)
	}
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		return err
	}
	previousMarkers, err := snapshotMarkers(markerDir)
	if err != nil {
		return err
	}
	readyPath := filepath.Join(markerDir, "config-ready")
	previousReady, previousReadyExists, err := readOptionalFile(readyPath)
	if err != nil {
		return err
	}
	oldTarget, oldExists, err := readOptionalFile(target)
	if err != nil {
		return err
	}
	// The generated nginx config checks config-ready before serving a route.
	// Close that gate while nginx loads the candidate. This keeps the old
	// running configuration from observing new route markers, and leaves the
	// new configuration closed if the process stops before the commit below.
	if err := os.Remove(readyPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	candidateConfig, err := os.ReadFile(candidate)
	if err != nil {
		_ = restoreFile(readyPath, previousReady, previousReadyExists, 0o644)
		return err
	}
	if err := atomicWriteFile(target, candidateConfig, 0o644); err != nil {
		_ = restoreFile(readyPath, previousReady, previousReadyExists, 0o644)
		return err
	}
	if err := runNginxCommand(ctx, p.cfg.NginxReloadCommand, target, p.cfg.NginxCommandTimeout); err != nil {
		// Certificate files are intentionally kept. The ready-state digest still
		// describes the old running configuration, so the next Apply retries the
		// new certificate material without introducing a second certificate
		// transaction.
		_ = restoreFile(target, oldTarget, oldExists, 0o644)
		_ = restoreMarkers(markerDir, previousMarkers)
		_ = restoreFile(readyPath, previousReady, previousReadyExists, 0o644)
		return fmt.Errorf("nginx reload: %w", err)
	}
	// The reload has installed the candidate. Commit the route markers and the
	// readiness gate afterwards, so a crash during reload cannot publish a
	// configuration that nginx has not accepted.
	if err := writeMarkers(markerDir, markers, true); err != nil {
		_ = os.Remove(readyPath)
		return fmt.Errorf("write nginx route markers: %w", err)
	}
	if err := atomicWriteFile(readyPath, []byte(readyState), 0o644); err != nil {
		_ = os.Remove(readyPath)
		return fmt.Errorf("write nginx ready state: %w", err)
	}
	p.cleanupSnapshots(digest)
	return nil
}

// cleanupSnapshots removes only Pages-generated snapshot directories that are
// no longer current. A cleanup error is deliberately reported and ignored:
// the new configuration has already passed nginx's checks and reload, so a
// housekeeping failure must not turn a successful publication into an error.
// The next Apply retries the same cleanup, including the no-change path above.
func (p *NginxPublisher) cleanupSnapshots(currentDigest string) {
	root := filepath.Join(p.cfg.NginxRoot, "config", "snapshots")
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		slog.Error("reading Nginx snapshots for cleanup failed", "error", err)
		return
	}

	for _, entry := range entries {
		name := entry.Name()
		if len(name) != sha256.Size*2 {
			continue
		}
		if _, err := hex.DecodeString(name); err != nil {
			continue
		}
		if name == currentDigest || entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			continue
		}
		candidate := filepath.Join(root, name, "pages.conf")
		info, err := os.Lstat(candidate)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := p.removeSnapshot(filepath.Join(root, name)); err != nil && !os.IsNotExist(err) {
			slog.Error("cleaning Nginx snapshot failed", "snapshot", name, "error", err)
		}
	}
}

func (p *NginxPublisher) confirmedReadyState() string {
	b, err := os.ReadFile(filepath.Join(p.cfg.DataRoot, "route-state", "config-ready"))
	if err != nil {
		return ""
	}
	return string(b)
}

func (p *NginxPublisher) certificateDigest() (string, error) {
	root := filepath.Join(p.cfg.NginxRoot, "certs")
	hash := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) && path == root {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		_, _ = hash.Write([]byte(relative))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(body)
		_, _ = hash.Write([]byte{0})
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
