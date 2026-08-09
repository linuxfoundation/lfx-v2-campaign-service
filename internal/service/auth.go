// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"log/slog"
	"sync"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TokenVerifier verifies a bearer token and returns the actor its claims describe.
// Implemented by internal/infrastructure/auth.Verifier; an interface here so this package
// does not depend on the JWKS client, and so a test can supply a "valid" token cheaply.
type TokenVerifier interface {
	VerifyActor(ctx context.Context, token string) (*model.Actor, error)
}

// actorCtxKey is the context key under which the authenticated actor is stored.
type actorCtxKey struct{}

// authGuard holds the token verifier shared by the three authenticated services, embedded
// rather than duplicated so all three admit one answer to "is this token good?".
type authGuard struct {
	// authMu guards v, which the container injects after construction (mirroring
	// SetBackend) while handlers may already be reading it. Named for the guard rather
	// than `mu`: every service embedding this already has its own `mu`, and two
	// same-named fields at different depths resolve silently to the outer one, so a
	// lock taken in the wrong place would compile and protect nothing.
	authMu sync.RWMutex
	v      TokenVerifier
}

// SetTokenVerifier injects the verifier; the container calls it per service.
func (g *authGuard) SetTokenVerifier(v TokenVerifier) {
	g.authMu.Lock()
	g.v = v
	g.authMu.Unlock()
}

// HasTokenVerifier reports whether a verifier was injected. Exported for the container
// test pinning EVERY boot path: a service without one refuses all traffic.
func (g *authGuard) HasTokenVerifier() bool {
	g.authMu.RLock()
	defer g.authMu.RUnlock()
	return g.v != nil
}

// authenticate verifies the token and returns a context carrying the actor. The returned
// string is a client-safe message, empty when authentication SUCCEEDED; each service maps
// it to its own generated error type. A nil verifier REJECTS rather than bypasses: before
// this, a request reaching the pod without passing Heimdall had its claims believed, so
// missing wiring has to be an outage rather than a silent return to that behaviour.
func (g *authGuard) authenticate(ctx context.Context, token string) (context.Context, string) {
	if token == "" {
		return ctx, "missing bearer token"
	}
	g.authMu.RLock()
	v := g.v
	g.authMu.RUnlock()
	if v == nil {
		slog.ErrorContext(ctx, "rejecting request: no token verifier is configured on this service")
		return ctx, "authentication is unavailable"
	}
	actor, err := v.VerifyActor(ctx, token)
	if err != nil {
		// The reason is logged, never returned: see auth.ErrUnauthenticated.
		slog.WarnContext(ctx, "rejecting request: bearer token failed verification", "error", err)
		return ctx, "invalid bearer token"
	}
	if actor == nil {
		// A verifier that accepts must name someone, or the unattributed-write path
		// this replaced is back.
		slog.ErrorContext(ctx, "rejecting request: verifier accepted a token but returned no actor")
		return ctx, "invalid bearer token"
	}
	return context.WithValue(ctx, actorCtxKey{}, actor), ""
}

// actorFromCtx returns the authenticated actor recorded by authenticate, or nil.
func actorFromCtx(ctx context.Context) *model.Actor {
	if a, ok := ctx.Value(actorCtxKey{}).(*model.Actor); ok {
		return a
	}
	return nil
}

// attributedActor returns the authenticated actor for a write that will RECORD it.
//
// Since JWTAuth began verifying, no served route can reach a handler without one, so the
// nil branch is unreachable today. It stays as a tripwire for a future entry point wired
// without the security scheme, which would otherwise present only as NULL attribution and
// nothing else failing. It counts ATTEMPTS, not commits — whether an actor is present is
// decided upstream of anything the repository does.
func attributedActor(ctx context.Context, operation string) *model.Actor {
	a := actorFromCtx(ctx)
	if a == nil {
		slog.WarnContext(ctx, "write attempted with no authenticated actor; attribution will be recorded as NULL if it commits",
			"operation", operation)
	}
	return a
}
