// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

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
	"sync"
)

// NginxPublisher renders, validates, and activates the external Nginx entry.
type NginxPublisher struct {
	cfg            Config
	resources      platformResources
	removeSnapshot func(string) error
	logger         *slog.Logger
	operationMu    sync.Mutex
	deployments    map[int64]ActiveDeployment
	challengeHosts map[string]struct{}
}
type nginxLocation struct{ base, root, marker, probeBody string }
type nginxMarker struct{ path, body string }

// NewNginxPublisher prepares the configured Nginx output directories.
func NewNginxPublisher(cfg Config) (*NginxPublisher, error) {
	if cfg.RootURL == nil || cfg.RootURL.Hostname() == "" || cfg.RootURL.Scheme != "http" && cfg.RootURL.Scheme != "https" {
		return nil, errors.New("pages root URL must use HTTP or HTTPS")
	}
	cfg.Mode = ModeNginx
	if !filepath.IsAbs(cfg.DataRoot) || !filepath.IsAbs(cfg.NginxRoot) {
		return nil, errors.New("pages data and Nginx roots must be absolute paths")
	}
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
	p := &NginxPublisher{
		cfg: cfg, resources: resources, removeSnapshot: os.RemoveAll, logger: slog.Default(),
		deployments: make(map[int64]ActiveDeployment), challengeHosts: make(map[string]struct{}),
	}
	if err := p.exportPublic(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *NginxPublisher) setLogger(logger *slog.Logger) {
	p.logger = logger
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

func (p *NginxPublisher) setChallenge(host, token, key string) {
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

func (p *NginxPublisher) clearChallenge(host, token string) {
	if p.cfg.NginxHTTP01Root == "" || token == "" || token != filepath.Base(token) || strings.ContainsAny(token, `/\\`) {
		return
	}
	if d := p.challengeDir(host); d != "" {
		_ = os.Remove(filepath.Join(d, token))
	}
}

func (p *NginxPublisher) Restore(ctx context.Context, deployments Deployments) error {
	p.operationMu.Lock()
	defer p.operationMu.Unlock()
	p.deployments = make(map[int64]ActiveDeployment)
	p.challengeHosts = make(map[string]struct{})
	if err := deployments.ForEach(func(active ActiveDeployment) error {
		if err := switchCurrentContent(active.Deployment); err != nil {
			return err
		}
		p.deployments[active.RepositoryID] = active
		if active.CustomURL != nil {
			p.challengeHosts[normalizeHost(active.CustomURL.Hostname())] = struct{}{}
		}
		return nil
	}); err != nil {
		return err
	}
	return p.apply(ctx)
}

func (p *NginxPublisher) prepareCustomDomain(ctx context.Context, host string) error {
	p.operationMu.Lock()
	defer p.operationMu.Unlock()
	host = normalizeHost(host)
	if _, ok := p.challengeHosts[host]; ok {
		return nil
	}
	p.challengeHosts[host] = struct{}{}
	if err := p.apply(ctx); err != nil {
		delete(p.challengeHosts, host)
		return err
	}
	return nil
}

func (p *NginxPublisher) initializeCertificates(certificates *certificateManager) error {
	return certificates.exportNginx(p.cfg.NginxRoot)
}

func (p *NginxPublisher) certificatesChanged(ctx context.Context, certificates *certificateManager) error {
	p.operationMu.Lock()
	defer p.operationMu.Unlock()
	if err := certificates.exportNginx(p.cfg.NginxRoot); err != nil {
		return err
	}
	return p.apply(ctx)
}

func (p *NginxPublisher) Publish(ctx context.Context, deployment Deployment) (Publication, error) {
	p.operationMu.Lock()
	defer p.operationMu.Unlock()
	old, hadOld := p.deployments[deployment.RepositoryID]
	oldChallengeHosts := make(map[string]struct{}, len(p.challengeHosts))
	for host := range p.challengeHosts {
		oldChallengeHosts[host] = struct{}{}
	}
	oldLink, oldLinkExists, err := readCurrentLink(deployment.Content.Path())
	if err != nil {
		return Publication{}, err
	}
	rollback := func(cause error) error {
		if hadOld {
			p.deployments[deployment.RepositoryID] = old
		} else {
			delete(p.deployments, deployment.RepositoryID)
		}
		p.challengeHosts = oldChallengeHosts
		if err := restoreCurrentLink(deployment.Content.Path(), oldLink, oldLinkExists); err != nil {
			cause = errors.Join(cause, err)
		}
		if err := p.apply(ctx); err != nil {
			cause = errors.Join(cause, err)
		}
		return cause
	}
	if err := switchCurrentContent(deployment); err != nil {
		return Publication{}, err
	}
	requested := Publication{DefaultPublished: true, CustomPublished: deployment.CustomURL != nil}
	p.deployments[deployment.RepositoryID] = ActiveDeployment{Deployment: deployment, Publication: requested}
	if deployment.CustomURL != nil {
		p.challengeHosts[normalizeHost(deployment.CustomURL.Hostname())] = struct{}{}
	}
	if hadOld && old.CustomURL != nil && (deployment.CustomURL == nil || normalizeHost(old.CustomURL.Hostname()) != normalizeHost(deployment.CustomURL.Hostname())) {
		delete(p.challengeHosts, normalizeHost(old.CustomURL.Hostname()))
	}
	if err := p.apply(ctx); err != nil {
		return Publication{}, rollback(err)
	}
	if err := probePlatform(ctx, p.cfg); err != nil {
		return Publication{}, rollback(err)
	}
	result := Publication{}
	if err := probeDeploymentURL(ctx, p.cfg, deployment.DefaultURL, deployment.RepositoryID, deployment.Content.Digest()); err == nil {
		result.DefaultPublished = true
	} else {
		result.Problem = PublicationProblemEntryProbeFailed
	}
	if deployment.CustomURL != nil {
		if err := probeDeploymentURL(ctx, p.cfg, *deployment.CustomURL, deployment.RepositoryID, deployment.Content.Digest()); err == nil {
			result.CustomPublished = true
		} else {
			result.Problem = PublicationProblemEntryProbeFailed
		}
	}
	p.deployments[deployment.RepositoryID] = ActiveDeployment{Deployment: deployment, Publication: result}
	if err := p.apply(ctx); err != nil {
		return Publication{}, rollback(err)
	}
	return result, nil
}

func (p *NginxPublisher) Unpublish(ctx context.Context, repoID int64) error {
	p.operationMu.Lock()
	defer p.operationMu.Unlock()
	old, ok := p.deployments[repoID]
	if !ok {
		return nil
	}
	delete(p.deployments, repoID)
	if old.CustomURL != nil {
		delete(p.challengeHosts, normalizeHost(old.CustomURL.Hostname()))
	}
	if err := p.apply(ctx); err != nil {
		p.deployments[repoID] = old
		if old.CustomURL != nil {
			p.challengeHosts[normalizeHost(old.CustomURL.Hostname())] = struct{}{}
		}
		return err
	}
	current := filepath.Join(filepath.Dir(old.Content.Path()), "current")
	if err := os.Remove(current); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func readCurrentLink(contentPath string) (string, bool, error) {
	link, err := os.Readlink(filepath.Join(filepath.Dir(contentPath), "current"))
	if os.IsNotExist(err) {
		return "", false, nil
	}
	return link, err == nil, err
}

func switchCurrentContent(deployment Deployment) error {
	dir := filepath.Dir(deployment.Content.Path())
	target := filepath.Base(deployment.Content.Path())
	tmp := filepath.Join(dir, ".current.tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, "current")); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func restoreCurrentLink(contentPath, link string, exists bool) error {
	dir := filepath.Dir(contentPath)
	current := filepath.Join(dir, "current")
	if !exists {
		if err := os.Remove(current); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	tmp := filepath.Join(dir, ".current.tmp")
	_ = os.Remove(tmp)
	if err := os.Symlink(link, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, current); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (p *NginxPublisher) apply(ctx context.Context) error {
	// Keep the certificate set stable from rendering through reload. Otherwise
	// a concurrent renewal could change a certificate after it was included in
	// the ready-state digest but before nginx loaded the generated config.
	certificateFilesMu.Lock()
	defer certificateFilesMu.Unlock()

	hosts := make(map[string][]nginxLocation)
	markers := make(map[string]nginxMarker)
	for host := range p.challengeHosts {
		hosts[host] = nil
	}
	for _, active := range p.deployments {
		deployment, publication := active.Deployment, active.Publication
		host, base := normalizeHost(deployment.DefaultURL.Hostname()), deployment.DefaultURL.EscapedPath()
		if base == "" {
			base = "/"
		}
		if !strings.HasSuffix(base, "/") {
			base += "/"
		}
		root := filepath.Join(filepath.Dir(deployment.Content.Path()), "current") + "/"
		loc := nginxLocation{base: base, root: root, marker: p.routeMarker(host, base, deployment.RepositoryID), probeBody: fmt.Sprintf("%d %s\n", deployment.RepositoryID, deployment.Content.Digest())}
		hosts[host] = append(hosts[host], loc)
		if publication.DefaultPublished {
			markers[loc.marker] = nginxMarker{loc.marker, loc.probeBody}
		}
		if deployment.CustomURL != nil {
			host = normalizeHost(deployment.CustomURL.Hostname())
			loc = nginxLocation{base: "/", root: root, marker: p.routeMarker(host, "/", deployment.RepositoryID), probeBody: fmt.Sprintf("%d %s\n", deployment.RepositoryID, deployment.Content.Digest())}
			hosts[host] = append(hosts[host], loc)
			if publication.CustomPublished {
				markers[loc.marker] = nginxMarker{loc.marker, loc.probeBody}
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
		_ = restoreFile(readyPath, previousReady, previousReadyExists)
		return err
	}
	if err := atomicWriteFile(target, candidateConfig, 0o644); err != nil {
		_ = restoreFile(readyPath, previousReady, previousReadyExists)
		return err
	}
	if err := runNginxCommand(ctx, p.cfg.NginxReloadCommand, target, p.cfg.NginxCommandTimeout); err != nil {
		// Certificate files are intentionally kept. The ready-state digest still
		// describes the old running configuration, so the next Apply retries the
		// new certificate material without introducing a second certificate
		// transaction.
		_ = restoreFile(target, oldTarget, oldExists)
		_ = restoreMarkers(markerDir, previousMarkers)
		_ = restoreFile(readyPath, previousReady, previousReadyExists)
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
		p.logger.Error("reading Nginx snapshots for cleanup failed", "error", err)
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
			p.logger.Error("cleaning Nginx snapshot failed", "snapshot", name, "error", err)
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
