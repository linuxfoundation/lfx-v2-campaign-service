// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"

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
	ctx, msg := g.authenticate(context.Background(), "any-token")
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
	_, msgFail := failing.authenticate(context.Background(), "t")
	_, msgNil := nilActor.authenticate(context.Background(), "t")
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

	ctx, msg := g.authenticate(context.Background(), "good")
	if msg != "" {
		t.Fatalf("authenticate rejected a good token: %s", msg)
	}
	if got := actorFromCtx(ctx); got == nil || *got != *want {
		t.Fatalf("actor = %+v, want %+v", got, want)
	}
	if _, msg := g.authenticate(context.Background(), ""); msg == "" {
		t.Fatal("an empty token must be rejected")
	}
}
