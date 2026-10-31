// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

package runtime

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

type readyListener struct {
	net.Listener
	ready chan<- struct{}
	once  sync.Once
}

func (l *readyListener) Accept() (net.Conn, error) {
	l.once.Do(func() { l.ready <- struct{}{} })
	return l.Listener.Accept()
}

// Run starts the built-in HTTP publisher and synchronization service.
func Run(ctx context.Context, cfg Config, options ...Option) error {
	cfg.applyDefaults()
	if err := cfg.validateBuiltInServer(); err != nil {
		return err
	}
	publisher, err := newHTTPPublisher(cfg)
	if err != nil {
		return err
	}
	service, err := NewService(cfg, publisher, options...)
	if err != nil {
		return err
	}
	defer service.Close()

	httpListener, err := net.Listen("tcp", cfg.HTTPListen)
	if err != nil {
		return fmt.Errorf("listen Pages HTTP: %w", err)
	}
	servers := []*http.Server{{Handler: publisher.Handler(false), ReadHeaderTimeout: 10 * time.Second}}
	ready := make(chan struct{}, 2)
	listeners := []net.Listener{&readyListener{Listener: httpListener, ready: ready}}
	if cfg.HTTPSMode == HTTPSPages {
		tlsConfig := service.TLSConfig()
		if tlsConfig == nil {
			_ = httpListener.Close()
			return errors.New("pages HTTPS requires certificate services")
		}
		httpsListener, listenErr := net.Listen("tcp", cfg.HTTPSListen)
		if listenErr != nil {
			_ = httpListener.Close()
			return fmt.Errorf("listen Pages HTTPS: %w", listenErr)
		}
		listeners = append(listeners, tls.NewListener(&readyListener{Listener: httpsListener, ready: ready}, tlsConfig))
		servers = append(servers, &http.Server{Handler: publisher.Handler(true), ReadHeaderTimeout: 10 * time.Second})
	}

	errorsCh := make(chan error, len(servers)+1)
	done := make(chan struct{}, len(servers)+1)
	for i, server := range servers {
		listener := listeners[i]
		go func() {
			defer func() { done <- struct{}{} }()
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errorsCh <- fmt.Errorf("serve Pages: %w", err)
			}
		}()
	}
	shutdown := func() {
		for _, server := range servers {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_ = server.Shutdown(shutdownCtx)
			cancel()
		}
	}
	for range servers {
		select {
		case <-ready:
		case err := <-errorsCh:
			shutdown()
			for range servers {
				<-done
			}
			return err
		case <-ctx.Done():
			shutdown()
			for range servers {
				<-done
			}
			return nil
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer func() { done <- struct{}{} }()
		if err := service.Run(runCtx); err != nil {
			errorsCh <- fmt.Errorf("synchronize Pages state: %w", err)
		}
	}()
	var runErr error
	select {
	case runErr = <-errorsCh:
	case <-ctx.Done():
		runErr = ctx.Err()
	}
	cancel()
	shutdown()
	for range servers {
		<-done
	}
	<-done
	if errors.Is(runErr, context.Canceled) && ctx.Err() != nil {
		return nil
	}
	return runErr
}
