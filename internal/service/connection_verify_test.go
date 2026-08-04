// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// stubVerifier is a CredentialVerifier returning a canned result, recording whether it was
// called and with what.
type stubVerifier struct {
	result     domain.VerificationResult
	calls      int
	gotProject string
	gotCtxDone bool
}

func (s *stubVerifier) VerifyCredential(ctx context.Context, projectID string, _ model.Provider) domain.VerificationResult {
	s.calls++
	s.gotProject = projectID
	s.gotCtxDone = ctx.Err() != nil
	return s.result
}

// newTestConnService builds a ConnectionService backed by a fake repo holding one connection
// for the given provider, optionally with a verifier wired.
func newTestConnService(t *testing.T, p model.Provider, withCreds bool, v domain.CredentialVerifier) *ConnectionService {
	t.Helper()
	repo := newFakeRepo()
	c := &model.Connection{ProjectID: "cncf", Provider: p, AccountID: "123", Status: model.StatusActive, Version: 1}
	if withCreds {
		c.EncryptedCredentials = []byte("ciphertext")
	}
	repo.store[repoKey("cncf", p)] = c
	svc := NewConnectionService(repo, nil)
	if v != nil {
		svc.SetVerifiers(map[model.Provider]domain.CredentialVerifier{p: v})
	}
	return svc
}

// TestTestConn_VerifiedStateSetsOK pins the ONLY state for which ok is true.
func TestTestConn_VerifiedStateSetsOK(t *testing.T) {
	v := &stubVerifier{result: domain.VerificationResult{State: domain.VerificationVerified}}
	svc := newTestConnService(t, model.ProviderGoogleAds, true, v)

	res, err := svc.testConn(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("testConn: %v", err)
	}
	if res.State != string(domain.VerificationVerified) {
		t.Errorf("state = %q, want %q", res.State, domain.VerificationVerified)
	}
	if !res.OK {
		t.Error("ok = false for a verified state; ok must be derived as true only here")
	}
	if v.calls != 1 {
		t.Errorf("verifier called %d times, want 1", v.calls)
	}
	if v.gotProject != "cncf" {
		t.Errorf("verifier got project %q, want cncf", v.gotProject)
	}
}

// TestTestConn_InvalidIsNotOK pins that a provider REJECTION is reported as invalid with
// ok=false — the actionable "re-authenticate" verdict.
func TestTestConn_InvalidIsNotOK(t *testing.T) {
	v := &stubVerifier{result: domain.VerificationResult{
		State:  domain.VerificationInvalid,
		Reason: "Google Ads rejected the stored credential",
	}}
	svc := newTestConnService(t, model.ProviderGoogleAds, true, v)

	res, err := svc.testConn(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("testConn: %v", err)
	}
	if res.State != string(domain.VerificationInvalid) {
		t.Errorf("state = %q, want invalid", res.State)
	}
	if res.OK {
		t.Error("ok = true for an invalid credential")
	}
	if res.Message == nil || *res.Message == "" {
		t.Fatal("invalid state carried no reason; an operator would not know which system failed")
	}
}

// TestTestConn_UnverifiableIsDistinctFromInvalid is the CENTRAL test of this change: the two
// non-verified states must be DISTINGUISHABLE, because they imply opposite operator actions
// (re-authenticate vs. do not touch the credential). If a future refactor collapses them back
// into a boolean, this fails.
func TestTestConn_UnverifiableIsDistinctFromInvalid(t *testing.T) {
	invalid := &stubVerifier{result: domain.VerificationResult{State: domain.VerificationInvalid, Reason: "rejected"}}
	unver := &stubVerifier{result: domain.VerificationResult{State: domain.VerificationUnverifiable, Reason: "unreachable"}}

	svcInvalid := newTestConnService(t, model.ProviderGoogleAds, true, invalid)
	svcUnver := newTestConnService(t, model.ProviderGoogleAds, true, unver)

	resInvalid, err := svcInvalid.testConn(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("testConn(invalid): %v", err)
	}
	resUnver, err := svcUnver.testConn(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("testConn(unverifiable): %v", err)
	}

	// Both are non-ok — which is exactly why ok alone is insufficient.
	if resInvalid.OK || resUnver.OK {
		t.Fatal("expected both non-verified states to have ok=false")
	}
	// ...and they MUST still be tellable apart.
	if resInvalid.State == resUnver.State {
		t.Fatalf("invalid and unverifiable collapsed to the same state %q; they imply opposite operator actions", resInvalid.State)
	}
	if resUnver.State != string(domain.VerificationUnverifiable) {
		t.Errorf("state = %q, want unverifiable", resUnver.State)
	}
}

// TestTestConn_NoVerifierWiredIsUnverifiable pins that a provider with no verifier reports
// "unknown" rather than implying the credential is good (the old ok=HasCredentials behaviour).
func TestTestConn_NoVerifierWiredIsUnverifiable(t *testing.T) {
	// Credentials ARE stored — under the old implementation this returned ok=true.
	svc := newTestConnService(t, model.ProviderRedditAds, true, nil)

	res, err := svc.testConn(context.Background(), "cncf", model.ProviderRedditAds)
	if err != nil {
		t.Fatalf("testConn: %v", err)
	}
	if res.OK {
		t.Error("ok = true with no verifier wired: this reports credential PRESENCE as success, the exact defect being fixed")
	}
	if res.State != string(domain.VerificationUnverifiable) {
		t.Errorf("state = %q, want unverifiable", res.State)
	}
	if res.Message == nil || !strings.Contains(*res.Message, "not yet wired") {
		t.Errorf("reason should name that verification is not wired for this provider, got %v", res.Message)
	}
}

// TestTestConn_NoStoredCredentialIsUnverifiableNotInvalid pins that "nothing supplied" is not
// reported as "rejected" — different problem, different fix.
func TestTestConn_NoStoredCredentialIsUnverifiableNotInvalid(t *testing.T) {
	v := &stubVerifier{result: domain.VerificationResult{State: domain.VerificationVerified}}
	svc := newTestConnService(t, model.ProviderGoogleAds, false /* no creds */, v)

	res, err := svc.testConn(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("testConn: %v", err)
	}
	if res.State != string(domain.VerificationUnverifiable) {
		t.Errorf("state = %q, want unverifiable for a missing credential", res.State)
	}
	if res.OK {
		t.Error("ok = true with no credential stored")
	}
	// The provider must NOT be contacted when there is nothing to verify.
	if v.calls != 0 {
		t.Errorf("verifier called %d times with no stored credential; want 0", v.calls)
	}
}

// TestTestConn_UnknownStateFailsClosed pins the fail-closed path: a verifier returning a state
// this build does not recognize must be reported as unverifiable, never as a verdict.
func TestTestConn_UnknownStateFailsClosed(t *testing.T) {
	v := &stubVerifier{result: domain.VerificationResult{State: domain.VerificationState("bogus"), Reason: "should be discarded"}}
	svc := newTestConnService(t, model.ProviderGoogleAds, true, v)

	res, err := svc.testConn(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("testConn: %v", err)
	}
	if res.State != string(domain.VerificationUnverifiable) {
		t.Errorf("state = %q, want unverifiable for an unrecognized state", res.State)
	}
	if res.OK {
		t.Error("ok = true for an unrecognized state")
	}
	if res.Message == nil || !strings.Contains(*res.Message, "unrecognized") {
		t.Errorf("reason should say the outcome was unrecognized, got %v", res.Message)
	}
}

// TestTestConn_DoesNotMutateConnectionStatus pins that verification NEVER writes to the
// connection row. If an unverifiable outcome marked the connection 'error', a provider outage
// would durably brand every project's working connection as broken.
func TestTestConn_DoesNotMutateConnectionStatus(t *testing.T) {
	v := &stubVerifier{result: domain.VerificationResult{State: domain.VerificationInvalid, Reason: "rejected"}}
	repo := newFakeRepo()
	stored := &model.Connection{
		ProjectID: "cncf", Provider: model.ProviderGoogleAds, AccountID: "123",
		Status: model.StatusActive, Version: 1, EncryptedCredentials: []byte("ciphertext"),
	}
	repo.store[repoKey("cncf", model.ProviderGoogleAds)] = stored
	svc := NewConnectionService(repo, nil)
	svc.SetVerifiers(map[model.Provider]domain.CredentialVerifier{model.ProviderGoogleAds: v})

	if _, err := svc.testConn(context.Background(), "cncf", model.ProviderGoogleAds); err != nil {
		t.Fatalf("testConn: %v", err)
	}
	if stored.Status != model.StatusActive {
		t.Errorf("connection status mutated to %q by a verification call; test must be read-only", stored.Status)
	}
	if stored.Version != 1 {
		t.Errorf("connection version bumped to %d by a verification call", stored.Version)
	}
}

// TestTestConn_BoundsTheProviderCall pins that the probe runs under a deadline, so a hung
// provider cannot outlive the HTTP response.
func TestTestConn_BoundsTheProviderCall(t *testing.T) {
	var gotDeadline bool
	v := &deadlineProbe{onCall: func(ctx context.Context) { _, gotDeadline = ctx.Deadline() }}
	svc := newTestConnService(t, model.ProviderGoogleAds, true, v)

	if _, err := svc.testConn(context.Background(), "cncf", model.ProviderGoogleAds); err != nil {
		t.Fatalf("testConn: %v", err)
	}
	if !gotDeadline {
		t.Error("verifier ran without a deadline; a hung provider could outlive the HTTP write timeout")
	}
}

type deadlineProbe struct{ onCall func(context.Context) }

func (d *deadlineProbe) VerifyCredential(ctx context.Context, _ string, _ model.Provider) domain.VerificationResult {
	d.onCall(ctx)
	return domain.VerificationResult{State: domain.VerificationVerified}
}
