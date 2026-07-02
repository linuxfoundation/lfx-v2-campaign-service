// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package main wires up the HTTP server.
//
// NOTE: this file imports from gen/, which is produced by running `make apigen`.
// The project will not compile until you run `make apigen` at least once.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"sync"

	svcsvr "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_svc/server"
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

	endpoints := svc.NewEndpoints(cont.Service)
	if cfg.Debug {
		endpoints.Use(debug.LogPayloads())
	}

	return handleHTTPServer(ctx, cfg, endpoints, cont)
}

func handleHTTPServer(ctx context.Context, cfg *config.Config, endpoints *svc.Endpoints, cont *container.Container) error {
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

	var handler http.Handler = mux
	handler = middleware.RequestIDMiddleware()(handler)
	if cfg.Debug {
		handler = debug.HTTP()(handler)
	}
	handler = otelhttp.NewHandler(handler, "lfx-v2-campaign-service")

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
