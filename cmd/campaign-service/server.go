// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main wires up the HTTP server.
//
// NOTE: this file imports from gen/, which is produced by running `make apigen`.
// The project will not compile until you run `make apigen` at least once.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"

	connsvcsvr "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_connections/server"
	svcsvr "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_svc/server"
	connsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	svc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_svc"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/container"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/middleware"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/log"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"goa.design/clue/debug"
	goahttp "goa.design/goa/v3/http"
)

// StartServer initializes and starts the HTTP server.
func StartServer(ctx context.Context, cfg *config.Config) error {
	cont, err := container.NewContainer(cfg)
	if err != nil {
		return err
	}

	// NOTE: debug.LogPayloads() is intentionally NOT applied. Every authenticated
	// payload carries a BearerToken (a valid JWT), and the connection service's
	// create/set-credential payloads carry plaintext provider credentials, so
	// enabling DEBUG would leak those secrets into logs. Debug HTTP logging
	// (headers/status, no decoded payload) is still applied below via debug.HTTP().
	endpoints := svc.NewEndpoints(cont.Service)

	// The container always initializes Connections (NewContainer wires it in both
	// the DB and no-DB paths), so construct the endpoints unconditionally. Fail
	// loudly if it's unexpectedly nil rather than silently skipping the mount —
	// a mis-wired container should crash at startup, not serve 404s on the
	// connection routes.
	if cont.Connections == nil {
		return fmt.Errorf("container misconfigured: Connections service is nil")
	}
	connEndpoints := connsvc.NewEndpoints(cont.Connections)

	return handleHTTPServer(ctx, cfg, endpoints, connEndpoints, cont)
}

func handleHTTPServer(ctx context.Context, cfg *config.Config, endpoints *svc.Endpoints, connEndpoints *connsvc.Endpoints, cont *container.Container) error {
	mux := goahttp.NewMuxer()
	if cfg.Debug {
		debug.MountPprofHandlers(debug.Adapt(mux))
		debug.MountDebugLogEnabler(debug.Adapt(mux))
	}

	koDataPath := os.Getenv("KO_DATA_PATH")
	if koDataPath == "" {
		koDataPath = "."
	}
	koHTTPDir := http.Dir(koDataPath)

	eh := errorHandler(ctx)
	server := svcsvr.New(
		endpoints,
		mux,
		goahttp.RequestDecoder,
		goahttp.ResponseEncoder,
		eh,
		nil,
		koHTTPDir,
		koHTTPDir,
		koHTTPDir,
		koHTTPDir,
	)
	svcsvr.Mount(mux, server)

	// Mount the connection routes unconditionally. connEndpoints is always
	// non-nil (StartServer constructs it and fails loudly if the service is
	// mis-wired), and the connection service is wired even without a database
	// (with a nil repo) so its routes return the typed 503 rather than a bare
	// 404. A nil here would be a programmer error, so fail loudly rather than
	// silently skipping the mount and serving 404s.
	if connEndpoints == nil {
		return fmt.Errorf("handleHTTPServer: connEndpoints is nil (connection routes would be unmounted)")
	}
	connServer := connsvcsvr.New(connEndpoints, mux, goahttp.RequestDecoder, goahttp.ResponseEncoder, eh, nil)
	connsvcsvr.Mount(mux, connServer)

	var handler http.Handler = mux
	handler = middleware.RequestIDMiddleware()(handler)
	if cfg.Debug {
		handler = debug.HTTP()(handler)
	}
	handler = otelhttp.NewHandler(handler, "lfx-v2-campaign-service",
		otelhttp.WithFilter(func(r *http.Request) bool {
			// Exclude health probes from tracing to avoid steady span
			// volume from frequent liveness/readiness checks.
			p := r.URL.Path
			return p != "/healthz" && p != "/livez" && p != "/readyz"
		}),
	)

	srv := &http.Server{
		Addr:              cfg.ServerAddress(),
		Handler:           handler,
		ReadHeaderTimeout: constants.DefaultReadHeaderTimeout,
		WriteTimeout:      constants.DefaultWriteTimeout,
		IdleTimeout:       constants.DefaultIdleTimeout,
	}

	return runServerWithContext(ctx, srv, cont)
}

func errorHandler(logCtx context.Context) func(context.Context, http.ResponseWriter, error) {
	return func(ctx context.Context, _ http.ResponseWriter, err error) {
		slog.ErrorContext(ctx, "HTTP error occurred", log.ErrKey, err, "outer_context", logCtx)
	}
}

func runServerWithContext(ctx context.Context, srv *http.Server, cont *container.Container) error {
	serverErr := make(chan error, 1)

	go func() {
		slog.InfoContext(ctx, "LFX V2 Campaign Service listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
		slog.InfoContext(ctx, "shutdown initiated")
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), constants.DefaultShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.ErrorContext(ctx, "HTTP server shutdown error", log.ErrKey, err)
		}
	}()

	go func() {
		defer wg.Done()
		if err := cont.Close(); err != nil {
			slog.ErrorContext(ctx, "container close error", log.ErrKey, err)
		}
	}()

	wg.Wait()
	return nil
}
