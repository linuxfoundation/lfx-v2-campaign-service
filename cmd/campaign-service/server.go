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

	briefsvcsvr "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_briefs/server"
	connsvcsvr "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_connections/server"
	svcsvr "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_svc/server"
	briefsvc "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
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
	// enabling it would leak those secrets into logs. debug.HTTP() IS still
	// applied below, but in clue v1.2.1 it does not log headers or statuses — it
	// only propagates the runtime /debug toggle into each request's context (so
	// debug-level logs elsewhere activate); it decodes no payload.
	endpoints := svc.NewEndpoints(cont.Service)

	// The container always initializes Connections and Briefs (NewContainer wires
	// them in both the DB and no-DB paths), so construct the endpoints
	// unconditionally. Fail loudly if either is unexpectedly nil rather than
	// silently skipping the mount — a mis-wired container should crash at startup,
	// not serve 404s on the connection/brief routes. debug.LogPayloads is
	// intentionally NOT applied to any endpoint set: connection/brief payloads
	// carry BearerTokens and plaintext provider credentials that would leak.
	if cont.Connections == nil {
		return fmt.Errorf("container misconfigured: Connections service is nil")
	}
	connEndpoints := connsvc.NewEndpoints(cont.Connections)
	if cont.Briefs == nil {
		return fmt.Errorf("container misconfigured: Briefs service is nil")
	}
	briefEndpoints := briefsvc.NewEndpoints(cont.Briefs)

	return handleHTTPServer(ctx, cfg, endpoints, connEndpoints, briefEndpoints, cont)
}

// buildMux constructs the Goa muxer and mounts the campaign, connection, and
// brief services onto it. It is a seam so tests can assert that the routes are
// actually reachable (the bug this fixes — routes that compile but are never
// mounted return 404) without standing up a full server. It returns an error
// only for a programmer-level mis-wiring (nil endpoints).
func buildMux(ctx context.Context, cfg *config.Config, endpoints *svc.Endpoints, connEndpoints *connsvc.Endpoints, briefEndpoints *briefsvc.Endpoints) (goahttp.Muxer, error) {
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

	// Mount the connection and brief routes unconditionally. Both endpoint sets
	// are always non-nil (StartServer constructs them and fails loudly if a
	// service is mis-wired), and both are wired even without a database (nil repo)
	// so their routes return the typed 503 rather than a bare 404. A nil here would
	// be a programmer error, so fail loudly rather than silently skipping the mount
	// and serving 404s.
	if connEndpoints == nil {
		return nil, fmt.Errorf("buildMux: connEndpoints is nil (connection routes would be unmounted)")
	}
	connServer := connsvcsvr.New(connEndpoints, mux, goahttp.RequestDecoder, goahttp.ResponseEncoder, eh, nil)
	connsvcsvr.Mount(mux, connServer)

	if briefEndpoints == nil {
		return nil, fmt.Errorf("buildMux: briefEndpoints is nil (brief routes would be unmounted)")
	}
	briefServer := briefsvcsvr.New(briefEndpoints, mux, goahttp.RequestDecoder, goahttp.ResponseEncoder, eh, nil)
	briefsvcsvr.Mount(mux, briefServer)

	return mux, nil
}

func handleHTTPServer(ctx context.Context, cfg *config.Config, endpoints *svc.Endpoints, connEndpoints *connsvc.Endpoints, briefEndpoints *briefsvc.Endpoints, cont *container.Container) error {
	mux, err := buildMux(ctx, cfg, endpoints, connEndpoints, briefEndpoints)
	if err != nil {
		return err
	}

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
