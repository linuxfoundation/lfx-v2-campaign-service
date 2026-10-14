// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

// linkedInUnauthorizedBody is the 401 payload LinkedIn returns for an expired token —
// the body the client's expiry classifier actually matches, so these tests exercise the
// production arm rather than a synthetic one.
const linkedInUnauthorizedBody = `{"serviceErrorCode":65602,"message":"The token used in the request has expired","status":401}`

// linkedInFutureClock keeps future-dated campaign schedules valid.
func linkedInFutureClock() func() time.Time {
	return func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
}

// TestLinkedIn_ToggleMidCascade401IsUnconfirmedNotJustExpiry pins FINDING 7.
//
// On PAUSE the cascade flips the CAMPAIGN first (delivery stops immediately at the gate)
// and pauses the creatives second. A token revoked between those two steps produces a
// partialCascadeError whose Unwrap exposes the inner expiry — so an expiry-first check
// matched, and the caller was told only "reconnect the connection" for a tree that was
// ALREADY half-applied. The verify-before-retry signal, which is the only one describing
// a change that actually took effect, was silently dropped.
//
// Both facts are true here. The assertion is that the PERISHABLE one wins: "verify the
// platform state" describes a partial effect that persists whether or not the credential
// is ever repaired, while "reconnect" is a precondition the very next call rediscovers.
// The expiry is not lost either — unconfirmedToggleError wraps the cause, so the
// reconnect signal stays reachable through errors.Is.
func TestLinkedIn_ToggleMidCascade401IsUnconfirmedNotJustExpiry(t *testing.T) {
	var campaignPatched, creativePatched bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// Step 1 — the CAMPAIGN pause. Succeeds: delivery has now stopped upstream.
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "adCampaigns/"):
			campaignPatched = true
			w.WriteHeader(http.StatusOK)
		// Creative discovery returns one creative to pause.
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "creatives"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"elements":[{"id":"urn:li:sponsoredCreative:900"}],"metadata":{}}`)
		// Step 2 — the CREATIVE pause. The token is revoked between the two steps.
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "creatives/"):
			creativePatched = true
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, linkedInUnauthorizedBody)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	d := NewLinkedInDispatcher(
		fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(linkedInFutureClock()),
	)
	campaign := &model.Campaign{Platform: model.ProviderLinkedInAds, PlatformCampaignID: "555"}

	err := d.ToggleStatus(context.Background(), "p1", model.ProviderLinkedInAds, campaign, model.CampaignRunPaused)
	if err == nil {
		t.Fatal("expected an error when the creative pause is answered 401")
	}

	// The premise: the campaign gate really was flipped before the 401, so a partial
	// effect exists. Without this the test could pass for the wrong reason.
	if !campaignPatched || !creativePatched {
		t.Fatalf("test premise broken: campaignPatched=%v creativePatched=%v — the 401 must "+
			"land AFTER the campaign flip for this to be a partial cascade", campaignPatched, creativePatched)
	}

	// THE FINDING: the unconfirmed signal must reach the caller, not be preempted.
	var unconfirmed interface{ Unconfirmed() bool }
	if !errors.As(err, &unconfirmed) || !unconfirmed.Unconfirmed() {
		t.Fatalf("err = %v; want an Unconfirmed() error — the campaign pause already took "+
			"effect, so the caller must be told to verify platform state before retrying, "+
			"not merely to reconnect", err)
	}

	// The expiry must NOT be the classification the service layer keys on. linkedinExpiry
	// tags an expiry with domain.ErrConnectionNotUsable, and the service's toggle switch
	// matches that sentinel ABOVE its unconfirmed arm — so if the expiry arm had won here,
	// this partial cascade would answer a non-retryable 409 "repair your connection" and
	// the platform-state ambiguity would never be reported or logged at all.
	if errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Errorf("err = %v; must NOT carry ErrConnectionNotUsable: the service layer matches "+
			"that above its unconfirmed arm, so tagging it here re-masks the partial cascade", err)
	}

	// ...but the reconnect fact is not DESTROYED, only de-prioritised: it stays reachable
	// through the wrap so the caller can still report both.
	if !errors.Is(err, linkedin.ErrCredentialsExpired) {
		t.Errorf("err = %v; want ErrCredentialsExpired to remain reachable — the reorder must "+
			"re-rank the two signals, not discard one", err)
	}
	if !strings.Contains(err.Error(), "unconfirmed") {
		t.Errorf("err = %q; want the unconfirmed wording in the surfaced message", err)
	}
}

// TestLinkedIn_TogglePreSend401StaysAPlainExpiry is the negative half of FINDING 7's
// reorder, and it is what stops "unconfirmed first" from swallowing the expiry case the
// reconnect path exists for.
//
// A credential that fails closed BEFORE any request (here: a refresh token already past
// its deadline) applied nothing upstream, so it is not unconfirmed. It must still answer
// with the connection-defect pair that maps to an actionable non-retryable status.
func TestLinkedIn_TogglePreSend401StaysAPlainExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no upstream call should happen: the credential fails closed first (%s %s)", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	d := NewLinkedInDispatcher(
		fakeConnReader{conn: activeLinkedInConn(expiredLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(linkedInFutureClock()),
	)
	campaign := &model.Campaign{Platform: model.ProviderLinkedInAds, PlatformCampaignID: "555"}

	err := d.ToggleStatus(context.Background(), "p1", model.ProviderLinkedInAds, campaign, model.CampaignRunPaused)
	if err == nil {
		t.Fatal("expected an error for an expired, unrefreshable credential")
	}
	var unconfirmed interface{ Unconfirmed() bool }
	if errors.As(err, &unconfirmed) && unconfirmed.Unconfirmed() {
		t.Errorf("err = %v; a PRE-SEND expiry applied nothing upstream and must NOT be "+
			"reported as unconfirmed", err)
	}
	if !errors.Is(err, domain.ErrConnectionNotUsable) || !errors.Is(err, domain.ErrCredentialsExpired) {
		t.Errorf("err = %v; want the connection-defect pair so the caller is told to reconnect", err)
	}
}

// TestLinkedIn_DispatchGroupPOST401RetainsClaim pins the dispatcher half of FINDINGS
// 9+10. A 401 answering the campaign-group create POST may follow a group LinkedIn
// already committed, so the dispatch claim must be RETAINED — a released claim lets a
// retry create a second billable group that nothing will reconcile.
//
// The claim rule keys on `result == nil` ALONE, never on whether the campaign id is
// populated, so this asserts a non-nil campaign comes back even though no campaign id
// exists yet. Previously the 401 was a definite failure: the client returned (nil, err),
// the dispatcher took its `result == nil` arm, and the claim was released.
func TestLinkedIn_DispatchGroupPOST401RetainsClaim(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The find-existing lookups are GETs and must succeed so the flow reaches the
		// create POST; a GET failure is correctly NOT an ambiguous create, and letting
		// one happen here would pass the test for the wrong reason.
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, linkedInUnauthorizedBody)
	}))
	defer srv.Close()

	d := NewLinkedInDispatcher(
		fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(srv.URL), linkedin.WithClock(linkedInFutureClock()),
	)
	cfg := json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]
	}}`)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	if err == nil {
		t.Fatal("expected an error when the group create POST is answered 401")
	}
	if camp == nil {
		t.Fatal("campaign = nil; want a NON-NIL partial campaign so the orchestrator RETAINS " +
			"the dispatch claim — a 401 can follow a committed group create, and releasing " +
			"the claim lets a retry orphan a second billable campaign group")
	}
	// The claim rule keys on result == nil alone: an empty campaign id must NOT be read
	// as "nothing was created".
	if camp.PlatformCampaignID != "" {
		t.Errorf("PlatformCampaignID = %q, want empty — no campaign was created; the retention "+
			"decision must rest on the non-nil result, not on a populated id", camp.PlatformCampaignID)
	}
	if !strings.Contains(err.Error(), "may exist") {
		t.Errorf("err = %q; want the dispatcher's 'may exist' wording that marks a retained claim", err)
	}
	// The reconnect signal survives the reclassification.
	if !errors.Is(err, linkedin.ErrCredentialsExpired) {
		t.Errorf("err = %v; want ErrCredentialsExpired to remain reachable", err)
	}
}
