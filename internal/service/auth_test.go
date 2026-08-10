// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	audiences "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_audiences"
	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// stubVerifier maps a token to the actor it names — the whole contract this package
// depends on. Cryptography is tested in internal/infrastructure/auth.
type stubVerifier struct {
	actors map[string]*model.Actor
	err    error
}

func (s *stubVerifier) VerifyActor(_ context.Context, token string) (*model.Actor, error) {
	if s.err != nil {
		return nil, s.err
	}
	a, ok := s.actors[token]
	if !ok {
		return nil, errors.New("stub: unknown token")
	}
	return a, nil
}

func verifierFor(token string, actor *model.Actor) *stubVerifier {
	return &stubVerifier{actors: map[string]*model.Actor{token: actor}}
}

// TestAuthenticate_NoVerifierRejects is the load-bearing one: no verifier is the
// missing-wiring state, and it MUST refuse rather than quietly believe the claims.
func TestAuthenticate_NoVerifierRejects(t *testing.T) {
	var g authGuard
	ctx, msg, unavailable := g.authenticate(context.Background(), "any-token")
	if !unavailable {
		t.Error("a guard with no verifier is THIS service failing, not the caller: want the 503 disposition")
	}
	if msg == "" {
		t.Fatal("a guard with no verifier accepted a token; it must fail closed")
	}
	if actorFromCtx(ctx) != nil {
		t.Fatal("a rejected request must not carry an actor")
	}
}

// TestAuthenticate_RejectionMessagesAreOpaque pins that the client-facing message never
// varies with the REASON: a specific one only tells an attacker what to fix next.
func TestAuthenticate_RejectionMessagesAreOpaque(t *testing.T) {
	failing := authGuard{}
	failing.SetTokenVerifier(&stubVerifier{err: errors.New("signature is invalid")})

	nilActor := authGuard{}
	nilActor.SetTokenVerifier(&stubVerifier{actors: map[string]*model.Actor{"t": nil}})
	_, msgFail, failUnavail := failing.authenticate(context.Background(), "t")
	_, msgNil, nilUnavail := nilActor.authenticate(context.Background(), "t")
	if failUnavail || nilUnavail {
		t.Errorf("both are token-side refusals and must map to 400: %v / %v", failUnavail, nilUnavail)
	}
	if msgFail == "" || msgNil == "" {
		t.Fatalf("both must be rejections: %q / %q", msgFail, msgNil)
	}
	if msgFail != msgNil {
		t.Errorf("rejection messages differ by reason: %q vs %q", msgFail, msgNil)
	}
	if msgFail == "signature is invalid" {
		t.Error("the verifier's reason leaked into the client-facing message")
	}
}

// TestAuthenticate_GoodAndEmptyTokens covers the happy path (a handler reads back the
// actor the verifier named) and the empty header Goa passes through unchanged.
func TestAuthenticate_GoodAndEmptyTokens(t *testing.T) {
	g := authGuard{}
	want := &model.Actor{Username: "ada", Email: "ada@lf.dev", Name: "Ada"}
	g.SetTokenVerifier(verifierFor("good", want))

	ctx, msg, _ := g.authenticate(context.Background(), "good")
	if msg != "" {
		t.Fatalf("authenticate rejected a good token: %s", msg)
	}
	if got := actorFromCtx(ctx); got == nil || *got != *want {
		t.Fatalf("actor = %+v, want %+v", got, want)
	}
	if _, msg, _ := g.authenticate(context.Background(), ""); msg == "" {
		t.Fatal("an empty token must be rejected")
	}
}

// TestAuthenticate_EmptyTokenIsTheVerifiersCallNotTheGuards pins the disposition of an
// absent Authorization header: Goa passes "" for one, and the guard must ASK rather than
// decide. The production verifier refuses it either way (the half above), so the only
// behaviour this changes is the local mock principal's — whose contract is that every
// request is that principal, header or not. A guard that answered first made the mock
// usable only by callers who invented a dummy token.
func TestAuthenticate_EmptyTokenIsTheVerifiersCallNotTheGuards(t *testing.T) {
	mock := &model.Actor{Username: "local-dev", Name: "local-dev"}
	g := authGuard{}
	g.SetTokenVerifier(&stubVerifier{actors: map[string]*model.Actor{"": mock}})

	ctx, msg, _ := g.authenticate(context.Background(), "")
	if msg != "" {
		t.Fatalf("a verifier that accepts the empty token was overruled by the guard: %s", msg)
	}
	if got := actorFromCtx(ctx); got == nil || *got != *mock {
		t.Fatalf("actor = %+v, want the principal the verifier named (%+v)", got, mock)
	}
}

// TestAuthenticate_KeyUnavailableIsOurOutageNotTheCallersFault pins the disposition split.
// Every reason a TOKEN is refused collapses to one opaque 400; a JWKS fetch that failed is
// not such a reason — nothing was learned about the token, because it was never checked.
// Answering 400 there tells a caller holding a perfectly good credential that theirs is
// bad, and tells them not to retry a condition that clears when Heimdall recovers. It is
// reachable on a cold cache and at every TTL expiry, not only at startup.
func TestAuthenticate_KeyUnavailableIsOurOutageNotTheCallersFault(t *testing.T) {
	g := authGuard{}
	g.SetTokenVerifier(&stubVerifier{err: fmt.Errorf("fetch jwks: %w", domain.ErrKeyUnavailable)})

	ctx, msg, unavailable := g.authenticate(context.Background(), "a-perfectly-good-token")
	if !unavailable {
		t.Fatal("a JWKS outage was reported as a bad token; it must take the 503 disposition")
	}
	if msg == "" {
		t.Fatal("the request must still be rejected")
	}
	if strings.Contains(msg, "jwks") || strings.Contains(msg, "invalid bearer token") {
		t.Errorf("msg = %q, want a client-safe message that does not blame the token", msg)
	}
	if actorFromCtx(ctx) != nil {
		t.Fatal("a rejected request must not carry an actor")
	}
}

// TestJWTAuth_UnverifiableIsUnavailableOnEveryService pins the disposition at all three
// boundaries. The mapping is written out once per service (Goa gives each its own error
// types), so a split that only the brief service honours is a split two thirds of the API
// does not have — and the failure mode is silent: a 400 for a JWKS outage looks like an
// ordinary rejection in every log and dashboard.
func TestJWTAuth_UnverifiableIsUnavailableOnEveryService(t *testing.T) {
	keysDown := &stubVerifier{err: fmt.Errorf("fetch jwks: %w", domain.ErrKeyUnavailable)}

	t.Run("connections", func(t *testing.T) {
		s := NewConnectionService(nil, nil)
		s.SetTokenVerifier(keysDown)
		_, err := s.JWTAuth(context.Background(), "a-perfectly-good-token", nil)
		if _, ok := err.(*conn.ConnServiceUnavailableError); !ok {
			t.Fatalf("err = %T (%v), want *conn.ConnServiceUnavailableError", err, err)
		}
	})
	t.Run("audiences", func(t *testing.T) {
		s := NewAudienceService(nil)
		s.SetTokenVerifier(keysDown)
		_, err := s.JWTAuth(context.Background(), "a-perfectly-good-token", nil)
		if _, ok := err.(*audiences.ConnServiceUnavailableError); !ok {
			t.Fatalf("err = %T (%v), want *audiences.ConnServiceUnavailableError", err, err)
		}
	})
}
