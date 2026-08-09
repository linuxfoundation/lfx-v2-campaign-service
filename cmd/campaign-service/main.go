// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// The LFX V2 Campaign Service.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/config"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/log"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/utils"
)

// Build-time variables set via ldflags in the Makefile.
var (
	version   = "dev"
	buildTime = "unknown"
	gitCommit = "unknown"
)

const gracefulShutdownSeconds = 25

func init() {
	log.InitStructureLogConfig()
}

func main() {
	// The installer runs as a subcommand of this binary (see sysacct.go) and must be
	// dispatched before any serving setup: it needs no OTel exporter and no signal handler.
	//
	// An unrecognised command is REFUSED rather than ignored. Matching only the exact name and
	// falling through meant a typo started the HTTP server: flag.Parse stops at the first
	// positional argument, so `bootstrap-system-acount` parsed cleanly and the deployment Job
	// that was meant to install credentials became a second, idle server replica — running,
	// healthy, exit code pending forever, with nothing installed and no error anywhere.
	if handled, code := runCommand(os.Args[1:], os.Stderr); handled {
		os.Exit(code)
	}

	cfg := config.LoadConfig()

	ctx := context.Background()
	otelConfig := utils.OTelConfigFromEnv()
	if otelConfig.ServiceVersion == "" {
		otelConfig.ServiceVersion = version
	}
	otelShutdown, err := utils.SetupOTelSDKWithConfig(ctx, otelConfig)
	if err != nil {
		slog.With(log.ErrKey, err).Error("error setting up OpenTelemetry SDK")
		os.Exit(1)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), gracefulShutdownSeconds*time.Second)
		defer cancel()
		if shutdownErr := otelShutdown(ctx); shutdownErr != nil {
			slog.With(log.ErrKey, shutdownErr).Error("error shutting down OpenTelemetry SDK")
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		<-sigChan
		slog.InfoContext(ctx, "received shutdown signal, shutting down gracefully...")
		cancel()
	}()

	slog.InfoContext(ctx, "starting LFX V2 Campaign Service",
		"version", version,
		"buildTime", buildTime,
		"gitCommit", gitCommit,
		"host", cfg.Host,
		"port", cfg.Port,
	)

	if err := StartServer(ctx, cfg); err != nil {
		slog.ErrorContext(ctx, "server exited with error", log.ErrKey, err)
		os.Exit(1)
	}
}
