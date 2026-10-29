// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"gitea.dev/page"
)

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--help" || os.Args[1] == "-h") {
		usage(os.Stdout)
		return
	}

	if len(os.Args) != 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	mode, err := page.ParseMode(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		usage(os.Stderr)
		os.Exit(2)
	}

	if err := run(mode); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(mode page.Mode) error {
	cfg, err := page.LoadConfig(mode)
	if err != nil {
		return err
	}
	store, err := page.OpenStore(cfg.DataRoot, cfg.EtcdQuotaBytes)
	if err != nil {
		return err
	}
	defer store.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := page.NewClient(cfg)
	var server *page.Server
	if mode == page.ModeStandalone {
		server, err = page.NewServer(store, cfg.RootURL, cfg.CustomRoot)
		if err != nil {
			return err
		}
	}
	certManager, acmeManager, err := configureCertificates(cfg, store)
	if err != nil {
		return err
	}

	var applier interface{ Apply(context.Context) error }
	if mode == page.ModeNginx {
		applier, err = page.NewNginxPublisher(cfg, store)
		if err != nil {
			return err
		}
		if certManager != nil {
			if err := certManager.ExportNginx(cfg.NginxRoot); err != nil {
				return err
			}
		}
	}
	syncer := page.NewSyncer(cfg, client, store, server, applier)
	if certManager != nil {
		if responder, ok := applier.(page.ChallengeResponder); ok {
			syncer.SetCertificateServices(certManager, acmeManager, cfg.HTTP01ProbeAddresses, responder)
		} else {
			syncer.SetCertificateServices(certManager, acmeManager, cfg.HTTP01ProbeAddresses, server)
		}
	}
	if mode == page.ModeNginx {
		return syncer.Run(ctx)
	}
	return serveStandalone(ctx, cfg, server, syncer, certManager)
}

func configureCertificates(cfg page.Config, store *page.Store) (*page.CertificateManager, *page.ACMEManager, error) {
	if cfg.RootURL.Scheme != "https" {
		return nil, nil, nil
	}

	certificates, err := page.NewCertificateManager(store, cfg.CertRoot)
	if err != nil {
		return nil, nil, err
	}
	if err := certificates.EnsureDefaultCertificateHosts(cfg.RootURL.Hostname(), "pages-check."+cfg.RootURL.Hostname()); err != nil {
		return nil, nil, err
	}

	var acme *page.ACMEManager
	if cfg.ACMEDirectoryURL != "" {
		acme, err = page.NewACMEManager(store, cfg.ACMEDirectoryURL, cfg.ACMEEmail)
		if err != nil {
			return nil, nil, err
		}
		acme.SetExternalAccountBinding(cfg.ACMEEABKID, cfg.ACMEEABKey)
	}
	if cfg.DNSProvider != "" {
		if _, err := page.NewDNS01Provider(cfg.DNSProvider, cfg.DNSPresentCommand, cfg.DNSCleanupCommand); err != nil {
			return nil, nil, err
		}
	}
	if !certificates.HasUsable("") && (acme == nil || cfg.DNSProvider == "") {
		return nil, nil, fmt.Errorf("HTTPS Pages requires a usable default certificate or configured ACME DNS-01 provider")
	}
	// The certificate maintenance loop performs issuance asynchronously after
	// the service has started. This keeps startup independent of the ACME
	// authority and lets the instance report a normal deployment state while
	// the default certificate is being prepared.
	return certificates, acme, nil
}

type readyListener struct {
	net.Listener
	ready chan<- struct{}
	once  sync.Once
}

func (l *readyListener) Accept() (net.Conn, error) {
	l.once.Do(func() { l.ready <- struct{}{} })
	return l.Listener.Accept()
}

func serveStandalone(ctx context.Context, cfg page.Config, server *page.Server, syncer *page.Syncer, certificates *page.CertificateManager) error {
	if server == nil || syncer == nil {
		return errors.New("standalone server and syncer are required")
	}
	httpListener, err := net.Listen("tcp", cfg.HTTPListen)
	if err != nil {
		return fmt.Errorf("listen Pages HTTP: %w", err)
	}
	servers := make([]*http.Server, 0, 2)
	ready := make(chan struct{}, 2)
	listeners := make([]net.Listener, 1)
	listeners[0] = &readyListener{Listener: httpListener, ready: ready}
	httpServer := &http.Server{Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	servers = append(servers, httpServer)

	if cfg.RootURL.Scheme == "https" {
		if certificates == nil {
			_ = httpListener.Close()
			return errors.New("HTTPS standalone server requires certificate services")
		}
		httpsListener, listenErr := net.Listen("tcp", cfg.HTTPSListen)
		if listenErr != nil {
			_ = httpListener.Close()
			return fmt.Errorf("listen Pages HTTPS: %w", listenErr)
		}
		listeners = append(listeners, tls.NewListener(&readyListener{Listener: httpsListener, ready: ready}, certificates.TLSConfig()))
		servers = append(servers, &http.Server{Handler: server.WithHTTPS().Handler(), ReadHeaderTimeout: 10 * time.Second})
	}

	serveErrors := make(chan error, len(servers)+1)
	serveDone := make(chan struct{}, len(servers)+1)
	for i, httpServer := range servers {
		listener := listeners[i]
		go func(httpServer *http.Server, listener net.Listener) {
			defer func() { serveDone <- struct{}{} }()
			if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErrors <- fmt.Errorf("serve Pages: %w", err)
			}
		}(httpServer, listener)
	}
	shutdownServers := func() {
		for _, httpServer := range servers {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = httpServer.Shutdown(shutdownCtx)
			shutdownCancel()
		}
	}
	for range servers {
		select {
		case <-ready:
		case err := <-serveErrors:
			shutdownServers()
			for range servers {
				<-serveDone
			}
			return err
		case <-ctx.Done():
			shutdownServers()
			for range servers {
				<-serveDone
			}
			return nil
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		defer func() { serveDone <- struct{}{} }()
		if err := syncer.Run(runCtx); err != nil {
			serveErrors <- fmt.Errorf("synchronize Pages state: %w", err)
		}
	}()

	var runErr error
	select {
	case runErr = <-serveErrors:
	case <-ctx.Done():
		runErr = ctx.Err()
	}
	cancel()
	shutdownServers()
	for range servers {
		<-serveDone
	}
	<-serveDone
	if errors.Is(runErr, context.Canceled) && ctx.Err() != nil {
		return nil
	}
	return runErr
}

func usage(out *os.File) {
	_, _ = fmt.Fprintln(out, "Usage: gitea-pages <standalone|nginx>")
}
