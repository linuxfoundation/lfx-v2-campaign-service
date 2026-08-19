// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
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

// bearerChallenge is the WWW-Authenticate value sent with every 401. RFC 9110 §15.5.2
// requires a 401 to carry at least one challenge, and Goa emits this one by mapping the
// generated error's WwwAuthenticate field onto the header (see design/connection.go's
// UnauthorizedError) — so an empty string here would produce a spec-violating bare 401
// rather than no header at all.
//
// It is the bare scheme, with no `realm` and no `error="invalid_token"` parameter. Both
// are optional, and both would reintroduce exactly what the opaque message avoids: an
// `error` code names WHICH check failed, which is the reason-specific signal
// TestAuthenticate_RejectionMessagesAreOpaque exists to keep off the wire. The three
// services share the one constant so a challenge cannot drift between them.
const bearerChallenge = "Bearer"

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
// it to its own generated error type.
//
// The bool says whose fault the failure is: true when THIS service could not perform the
// check (no verifier wired, Heimdall's JWKS unreachable), false when the token itself was
// refused. Callers map the first to 503 and the second to 401. The SECOND was 400 before,
// which classified an absent or refused credential as a MALFORMED REQUEST, and told the
// caller not to retry a token a refresh would fix. The first has always answered 503 and
// is untouched here. The verdict is not derivable from the message — "invalid bearer
// token" is deliberately the same string for every token-side refusal — so it is returned
// separately rather than sniffed.
//
// The token-side refusal is 401 rather than 400 because the request is WELL-FORMED and the
// credential is what must be replaced: 400 conflated an expired token with an invalid
// payload, which want opposite client handling (refresh and retry vs. do not retry), and
// made auth failures indistinguishable from payload errors in status-based alerting.
//
// A nil verifier REJECTS rather than bypasses: before
// this, a request reaching the pod without passing Heimdall had its claims believed, so
// missing wiring has to be an outage rather than a silent return to that behaviour.
//
// An EMPTY token is not short-circuited here, deliberately. Goa passes "" when the
// Authorization header is absent, and the production verifier rejects that explicitly
// (auth.VerifyActor: "token is empty"), so pre-empting it changes nothing for a deployed
// pod — it only overrides the ONE verifier that is supposed to accept it. The local mock
// principal's whole contract is "treat every request as this principal", and a developer
// running without Auth0 has no header to send: rejecting them before the verifier is
// consulted made the mock work only for callers who invented a dummy token, i.e. not for
// the workflow it exists for. Who may authenticate is the verifier's question.
func (g *authGuard) authenticate(ctx context.Context, token string) (_ context.Context, msg string, unavailable bool) {
	g.authMu.RLock()
	v := g.v
	g.authMu.RUnlock()
	if v == nil {
		slog.ErrorContext(ctx, "rejecting request: no token verifier is configured on this service")
		return ctx, "authentication is unavailable", true
	}
	actor, err := v.VerifyActor(ctx, token)
	if err != nil {
		if errors.Is(err, domain.ErrKeyUnavailable) {
			// Nothing was learned about the token: the keys to check it against never
			// arrived. Logged at error, not warn — this one is ours.
			slog.ErrorContext(ctx, "rejecting request: token signing keys are unavailable", "error", err)
			return ctx, "authentication is unavailable", true
		}
		// The reason is logged, never returned: see auth.ErrUnauthenticated.
		slog.WarnContext(ctx, "rejecting request: bearer token failed verification", "error", err)
		return ctx, "invalid bearer token", false
	}
	if actor == nil {
		// A verifier that accepts must name someone, or the unattributed-write path
		// this replaced is back. This one stays on the token-side branch deliberately: it
		// is indistinguishable from a refusal from outside, and keeping it there is what
		// TestAuthenticate_RejectionMessagesAreOpaque pins — a caller must not learn from
		// the response which of the two happened.
		slog.ErrorContext(ctx, "rejecting request: verifier accepted a token but returned no actor")
		return ctx, "invalid bearer token", false
	}
	return context.WithValue(ctx, actorCtxKey{}, actor), "", false
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
//
// The message says "unattributed" rather than naming a column state, because this helper serves
// every table and the tables disagree. `campaign_repo.go` COALESCEs on update (the upsert
// conflict arm, the replace, and the soft delete), so a nil actor there PRESERVES the last actor
// known — deliberately, since forgetting who last touched a row is worse than not learning who
// touched it this time. `brief_repo.go` (replace, approve, archive) and `connection_repo.go`
// assign `updated_by` outright, so the same nil actor CLEARS it.
//
// Promising either one here would send an operator looking for a column state half the tables do
// not have, which is why the warning names neither. The divergence itself is worth settling, but
// it is a repository decision and not this helper's to make.
func attributedActor(ctx context.Context, operation string) *model.Actor {
	a := actorFromCtx(ctx)
	if a == nil {
		slog.WarnContext(ctx, "write attempted with no authenticated actor; this write is unattributed (the stored actor is left NULL, preserved, or cleared depending on the table)",
			"operation", operation)
	}
	return a
}
