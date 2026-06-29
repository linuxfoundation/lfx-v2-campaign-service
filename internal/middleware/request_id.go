// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package middleware provides HTTP middleware for the service.
package middleware

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/log"
)

// RequestIDMiddleware injects a request ID into the context and response headers.
// It reads an existing X-Request-ID header if present, otherwise generates a new UUID.
func RequestIDMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get(constants.RequestIDHeader)
			if requestID == "" {
				requestID = uuid.New().String()
			}

			w.Header().Set(constants.RequestIDHeader, requestID)

			ctx := context.WithValue(r.Context(), constants.RequestIDHeader, requestID)
			ctx = log.AppendCtx(ctx, slog.String(constants.RequestIDHeader, requestID))

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
