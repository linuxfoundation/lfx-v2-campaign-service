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
	"time"

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
	// create/set-credential payloads carry plaintext provider credentials (OAuth
	// refresh tokens, client secrets), so enabling it would leak those secrets
	// into logs. debug.HTTP() IS still applied below, but in clue v1.2.1 it does
	// not log headers or statuses — it only propagates the runtime /debug toggle
	// into each request's context (so debug-level logs elsewhere activate); it
	// decodes no payload.
	endpoints := svc.NewEndpoints(cont.Service)

	// The container always initializes Connections and Briefs (NewContainer wires
	// them in both the DB and no-DB paths), so construct the endpoints
	// unconditionally. Fail loudly if either is unexpectedly nil rather than
	// silently skipping the mount — a mis-wired container should crash at startup,
	// not serve 404s on the connection/brief routes.
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

	// Track in-flight handlers so shutdown can wait (bounded) for stragglers to
	// return before the DB pool closes. http.Server.Close does not wait for
	// handler goroutines, so without this a straggler could touch the pool as it
	// closes; the tracker is waited on between the forced Close and cont.Close.
	inflight := middleware.NewInflightTracker()

	var handler http.Handler = mux
	handler = inflight.Middleware()(handler)
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

	return runServerWithContext(ctx, srv, cont, inflight)
}

// inflightWaiter is the subset of middleware.InflightTracker the shutdown path
// needs: wait (bounded) for in-flight handlers to return.
type inflightWaiter interface {
	Wait(timeout time.Duration) bool
}

// httpDeadline returns the HTTP-shutdown context's deadline, or now if it has
// none — so the caller's remaining-budget computation yields a non-positive
// duration (an immediate, non-blocking inflight check) rather than an
// unbounded wait when no deadline is set.
func httpDeadline(httpCtx context.Context) time.Time {
	if d, ok := httpCtx.Deadline(); ok {
		return d
	}
	return time.Now()
}

func errorHandler(logCtx context.Context) func(context.Context, http.ResponseWriter, error) {
	return func(ctx context.Context, _ http.ResponseWriter, err error) {
		slog.ErrorContext(ctx, "HTTP error occurred", log.ErrKey, err, "outer_context", logCtx)
	}
}

func runServerWithContext(ctx context.Context, srv *http.Server, cont *container.Container, inflight inflightWaiter) error {
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

	// Shut down in order, not concurrently: first stop the HTTP server (stops
	// accepting new requests and drains in-flight handlers, so no new dispatch
	// can be Started), THEN close the container (which drains in-flight dispatch
	// and only then closes the DB pool). Running these concurrently risks a
	// just-accepted request Starting a dispatch while the pool is being closed.
	//
	// Each phase gets its OWN bounded deadline rather than sharing one context.
	// A shared context would not bound the total: srv.Shutdown could consume the
	// whole budget draining HTTP handlers, leaving Container.Close's drain+grace
	// (whose orchestrator grace timer is otherwise wall-clock, not ctx-bound) to
	// add its full window on top — pushing the real total past
	// DefaultShutdownTimeout and risking a SIGKILL mid-drain. Budgeting the
	// phases separately makes the total a true sum: httpShutdownTimeout +
	// (dispatchDrainTimeout + CancelGracePeriod) <= DefaultShutdownTimeout, which
	// container.go asserts at init.
	// Shutdown ordering is explicit:
	//   1. graceful srv.Shutdown(httpCtx) — stop accepting, drain handlers.
	//   2. on httpCtx expiry, force srv.Close() — http.Server.Shutdown RETURNS on
	//      ctx expiry even if handlers are STILL running; those stragglers could
	//      then touch the DB pool while container/pool close runs (and pgxpool.Close
	//      itself can block on a connection a handler still holds). Close() forcibly
	//      terminates all connections so no handler is still using the pool when it
	//      closes. Force-close only after a timeout — on a clean Shutdown there are
	//      no lingering connections and Close() is a harmless no-op.
	//   3. wait (bounded) for any in-flight handler goroutine to actually RETURN.
	//      srv.Close() terminates connections but does NOT wait for the handler
	//      goroutines themselves, so a straggler could still be executing (and
	//      touching the pool) after Close returns. The inflight tracker lets us
	//      wait for those goroutines to finish before closing the pool; the wait
	//      is bounded by whatever remains of the HTTP-shutdown budget so a hung
	//      handler can never wedge shutdown past its window.
	//   4. THEN cont.Close(closeCtx) — drain in-flight dispatch, then close the pool.
	// A residual remains only if a handler outlasts the bounded wait; that window
	// is tracked under LFXV2-2665.
	httpCtx, cancelHTTP := context.WithTimeout(context.Background(), container.HTTPShutdownTimeout)
	defer cancelHTTP()
	if err := srv.Shutdown(httpCtx); err != nil {
		slog.ErrorContext(ctx, "HTTP server shutdown error; force-closing lingering connections", log.ErrKey, err)
		// Shutdown's ctx expired with handlers still running (or it otherwise
		// failed). Force-close so no straggler can touch the pool as it closes.
		if cerr := srv.Close(); cerr != nil {
			slog.ErrorContext(ctx, "HTTP server force-close error", log.ErrKey, cerr)
		}
	}
	// Wait for in-flight handler goroutines to return before closing the pool. On a
	// clean Shutdown they are already drained and this returns immediately; on the
	// force-close path it bounds the wait by the HTTP budget still remaining so a
	// straggler is awaited but can never push total shutdown past its window.
	if inflight != nil {
		remaining := time.Until(httpDeadline(httpCtx))
		if !inflight.Wait(remaining) {
			slog.ErrorContext(ctx, "in-flight HTTP handlers did not return before pool close; a straggler may touch the pool (LFXV2-2665)")
		}
	}
	closeCtx, cancelClose := context.WithTimeout(context.Background(), container.ContainerCloseTimeout)
	defer cancelClose()
	if err := cont.Close(closeCtx); err != nil {
		slog.ErrorContext(ctx, "container close error", log.ErrKey, err)
	}
	return nil
}
