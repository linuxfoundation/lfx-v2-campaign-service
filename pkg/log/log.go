// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package log provides structured logging utilities for context-aware logging.
package log

import (
	"context"
	"log"
	"log/slog"
	"os"

	slogotel "github.com/remychantenay/slog-otel"
)

// ErrKey is the standard key for error attributes in structured logs.
const ErrKey = "error"

type ctxKey string

const slogFields ctxKey = "slog_fields"

type contextHandler struct {
	slog.Handler
}

// Handle adds contextual attributes to the Record before calling the underlying handler.
func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs, ok := ctx.Value(slogFields).([]slog.Attr); ok {
		for _, v := range attrs {
			r.AddAttrs(v)
		}
	}
	return h.Handler.Handle(ctx, r)
}

// AppendCtx adds an slog attribute to the provided context so that it will be
// included in any Record created with such context.
func AppendCtx(parent context.Context, attr slog.Attr) context.Context {
	if parent == nil {
		parent = context.Background()
	}
	if v, ok := parent.Value(slogFields).([]slog.Attr); ok {
		v = append(v, attr)
		return context.WithValue(parent, slogFields, v)
	}
	return context.WithValue(parent, slogFields, []slog.Attr{attr})
}

// InitStructureLogConfig sets up JSON structured logging, level from LOG_LEVEL env var,
// and wraps the handler with the slog-otel bridge so trace/span IDs appear in every log line.
func InitStructureLogConfig() {
	opts := &slog.HandlerOptions{}

	switch os.Getenv("LOG_LEVEL") {
	case "debug":
		opts.Level = slog.LevelDebug
	case "warn":
		opts.Level = slog.LevelWarn
	case "info":
		opts.Level = slog.LevelInfo
	default:
		opts.Level = slog.LevelDebug
	}

	opts.AddSource = os.Getenv("LOG_ADD_SOURCE") == "true"

	log.SetFlags(log.Llongfile)
	h := slog.NewJSONHandler(os.Stdout, opts)
	slog.SetDefault(slog.New(contextHandler{slogotel.OtelHandler{Next: h}}))
}
