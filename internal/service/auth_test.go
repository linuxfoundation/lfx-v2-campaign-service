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
	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
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
	// The third boundary, and the one the comment above singles out. It was missing while
	// the comment claimed "all three" — which is the failure this whole test exists to
	// prevent, one level up: a guarantee asserted in prose and checked in two thirds of the
	// places it is asserted about reads, to anyone auditing coverage by reading, exactly
	// like a guarantee that holds.
	t.Run("briefs", func(t *testing.T) {
		s := NewBriefService(nil, nil, nil, nil)
		s.SetTokenVerifier(keysDown)
		_, err := s.JWTAuth(context.Background(), "a-perfectly-good-token", nil)
		if _, ok := err.(*briefs.ConnServiceUnavailableError); !ok {
			t.Fatalf("err = %T (%v), want *briefs.ConnServiceUnavailableError", err, err)
		}
	})
}

// TestJWTAuth_RefusedTokenIsUnauthorizedOnEveryService pins the token-side status at all
// three boundaries, and is the sibling of the 503 test above. The mapping is written out
// once per service — Goa gives each its own error types — so a service left on
// `BadRequestError` would answer 400 while the other two answer 401, and nothing about
// that is visible from the Go types: each service compiles, returns a typed error, and
// only the wire status disagrees.
//
// It asserts the CHALLENGE as well as the type. RFC 9110 §15.5.2 requires a 401 to carry
// one, and it reaches the wire solely because Goa maps this field onto the
// WWW-Authenticate header — a service that constructed the right type with the field left
// empty would emit a bare 401 that every status-only assertion here would still pass.
func TestJWTAuth_RefusedTokenIsUnauthorizedOnEveryService(t *testing.T) {
	// A verifier that is WIRED and refuses: the token-side branch, not the 503 one. A
	// bare authGuard would take the no-verifier branch and pass this for the wrong reason.
	refusing := &stubVerifier{err: errors.New("signature is invalid")}

	t.Run("connections", func(t *testing.T) {
		s := NewConnectionService(nil, nil)
		s.SetTokenVerifier(refusing)
		_, err := s.JWTAuth(context.Background(), "bad-token", nil)
		ue, ok := err.(*conn.UnauthorizedError)
		if !ok {
			t.Fatalf("err = %T (%v), want *conn.UnauthorizedError", err, err)
		}
		if ue.WwwAuthenticate != "Bearer" || ue.Code != "401" {
			t.Errorf("challenge/code = %q/%q, want \"Bearer\"/\"401\"", ue.WwwAuthenticate, ue.Code)
		}
	})
	t.Run("audiences", func(t *testing.T) {
		s := NewAudienceService(nil)
		s.SetTokenVerifier(refusing)
		_, err := s.JWTAuth(context.Background(), "bad-token", nil)
		ue, ok := err.(*audiences.UnauthorizedError)
		if !ok {
			t.Fatalf("err = %T (%v), want *audiences.UnauthorizedError", err, err)
		}
		if ue.WwwAuthenticate != "Bearer" || ue.Code != "401" {
			t.Errorf("challenge/code = %q/%q, want \"Bearer\"/\"401\"", ue.WwwAuthenticate, ue.Code)
		}
	})
	t.Run("briefs", func(t *testing.T) {
		s := NewBriefService(nil, nil, nil, nil)
		s.SetTokenVerifier(refusing)
		_, err := s.JWTAuth(context.Background(), "bad-token", nil)
		ue, ok := err.(*briefs.UnauthorizedError)
		if !ok {
			t.Fatalf("err = %T (%v), want *briefs.UnauthorizedError", err, err)
		}
		if ue.WwwAuthenticate != "Bearer" || ue.Code != "401" {
			t.Errorf("challenge/code = %q/%q, want \"Bearer\"/\"401\"", ue.WwwAuthenticate, ue.Code)
		}
	})
}

// TestJWTAuth_UnauthorizedMessageIsOpaqueAcrossServices is the wire-level companion to
// TestAuthenticate_RejectionMessagesAreOpaque, which pins opacity one layer down at the
// guard. Moving the status to 401 is exactly the kind of change that invites a more
// helpful body ("token expired", "bad signature"), and the guard-level test would not
// catch a service that enriched the message on its way out — it never calls JWTAuth.
//
// The two token-side reasons here are the same pair that test uses: a verifier that
// refuses, and one that accepts while naming nobody. Both must be indistinguishable to a
// caller in status, challenge AND message.
func TestJWTAuth_UnauthorizedMessageIsOpaqueAcrossServices(t *testing.T) {
	refused := &stubVerifier{err: errors.New("token is expired by 3h2m")}
	noActor := &stubVerifier{actors: map[string]*model.Actor{"t": nil}}

	// One accessor per service so the three concrete Unauthorized types — which share no
	// interface beyond `error` — can be compared in one loop.
	services := map[string]func(TokenVerifier) (code, msg, challenge string, err error){
		"connections": func(v TokenVerifier) (string, string, string, error) {
			s := NewConnectionService(nil, nil)
			s.SetTokenVerifier(v)
			_, err := s.JWTAuth(context.Background(), "t", nil)
			ue, ok := err.(*conn.UnauthorizedError)
			if !ok {
				return "", "", "", err
			}
			return ue.Code, ue.Message, ue.WwwAuthenticate, nil
		},
		"audiences": func(v TokenVerifier) (string, string, string, error) {
			s := NewAudienceService(nil)
			s.SetTokenVerifier(v)
			_, err := s.JWTAuth(context.Background(), "t", nil)
			ue, ok := err.(*audiences.UnauthorizedError)
			if !ok {
				return "", "", "", err
			}
			return ue.Code, ue.Message, ue.WwwAuthenticate, nil
		},
		"briefs": func(v TokenVerifier) (string, string, string, error) {
			s := NewBriefService(nil, nil, nil, nil)
			s.SetTokenVerifier(v)
			_, err := s.JWTAuth(context.Background(), "t", nil)
			ue, ok := err.(*briefs.UnauthorizedError)
			if !ok {
				return "", "", "", err
			}
			return ue.Code, ue.Message, ue.WwwAuthenticate, nil
		},
	}

	for name, call := range services {
		t.Run(name, func(t *testing.T) {
			_, msgRefused, challengeRefused, errA := call(refused)
			_, msgNoActor, challengeNoActor, errB := call(noActor)
			if errA != nil || errB != nil {
				t.Fatalf("both must be *UnauthorizedError: %v / %v", errA, errB)
			}
			if msgRefused != msgNoActor {
				t.Errorf("rejection messages differ by reason: %q vs %q", msgRefused, msgNoActor)
			}
			if challengeRefused != challengeNoActor {
				t.Errorf("challenges differ by reason: %q vs %q", challengeRefused, challengeNoActor)
			}
			// The verifier's own words must not survive into anything the client sees.
			// Checked on the message AND the challenge, because the challenge is a new
			// wire-visible field and `error="..."` is the RFC-blessed place a reason
			// would naturally be added.
			for field, v := range map[string]string{"message": msgRefused, "challenge": challengeRefused} {
				if strings.Contains(v, "expired") || strings.Contains(v, "signature") {
					t.Errorf("the verifier's reason leaked into the client-facing %s: %q", field, v)
				}
			}
		})
	}
}
