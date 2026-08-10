// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

const goodHubSpotCreds = `{"PrivateAppToken":"pat-123"}`

func activeHubSpotConn(creds string) *model.Connection {
	return &model.Connection{
		Provider:             model.ProviderHubSpot,
		AccountID:            "8112310",
		EncryptedCredentials: []byte(creds),
		ProviderConfig:       map[string]string{"portal_id": "8112310"},
		Status:               model.StatusActive,
	}
}

// fakeAudienceReader is an in-memory audienceReader for the dispatcher tests.
type fakeAudienceReader struct {
	auds []*model.CampaignAudience
	err  error
}

func (f fakeAudienceReader) ListAudiences(context.Context, string, string) ([]*model.CampaignAudience, error) {
	return f.auds, f.err
}

// builtHubSpotAudience returns a newest-first list with one BUILT HubSpot audience.
func builtHubSpotAudience(masterList string, suppression []string) []*model.CampaignAudience {
	raw, _ := json.Marshal(suppression)
	return []*model.CampaignAudience{{
		ID: "aud-1", Platform: model.ProviderHubSpot, Status: model.AudienceBuilt,
		PlatformMasterListID: masterList, SuppressionListIDs: raw,
	}}
}

// hubspotServer fakes the HubSpot API for the clone + set-send-list flow. It records the
// send-list payload so a test can assert the master/suppression ids reached the wire.
// hubspotRec captures what the fake server saw. Every field is written by the HANDLER goroutine
// and read by the TEST goroutine, so all access is mutex-guarded: httptest.Server.Close only
// synchronizes at the deferred Close, which runs AFTER the assertions (same guard as
// meta_test.go).
type hubspotRec struct {
	mu           sync.Mutex
	sendListBody map[string]any
	sawClone     bool
	sawSendList  bool
	taggedHTML   string
}

func (r *hubspotRec) markClone() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sawClone = true
}

func (r *hubspotRec) markSendList(body map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sawSendList = true
	r.sendListBody = body
}

func (r *hubspotRec) markTagged(raw string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.taggedHTML = raw
}

// SawClone / SawSendList / SendListBody / TaggedHTML read the captures under the lock.
func (r *hubspotRec) SawClone() bool    { r.mu.Lock(); defer r.mu.Unlock(); return r.sawClone }
func (r *hubspotRec) SawSendList() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.sawSendList }
func (r *hubspotRec) SendListBody() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sendListBody
}
func (r *hubspotRec) TaggedHTML() string { r.mu.Lock(); defer r.mu.Unlock(); return r.taggedHTML }

func hubspotServer(t *testing.T) (*httptest.Server, *hubspotRec) {
	t.Helper()
	rec := &hubspotRec{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/oauth/v2/private-apps/get/access-token-info":
			_, _ = io.WriteString(w, `{"hubId":"8112310"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/clone"):
			rec.markClone()
			_, _ = io.WriteString(w, `{"id":"cloned-1","name":"test-clone","state":"DRAFT"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/setsendlist"):
			body := make(map[string]any)
			_ = json.NewDecoder(r.Body).Decode(&body)
			rec.markSendList(body)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/draft"):
			_, _ = io.WriteString(w, `{"content":{"widgets":{}}}`)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/draft"):
			_, _ = io.WriteString(w, `{"id":"cloned-1"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	return srv, rec
}


func TestDispatch_PortalLookupTimeoutIsApplied(t *testing.T) {
	srv, _ := hubspotServer(t)
	defer srv.Close()

	// Captures the DEADLINE the outgoing http.Request's context carried, per path — the
	// server-side r.Context() (an httptest connection-lifetime context) can't reveal this;
	// only the client-side request the RoundTripper sees has the ctx this code actually set.
	deadlines := map[string]time.Time{}
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if dl, ok := req.Context().Deadline(); ok {
			deadlines[req.URL.Path] = dl
		}
		return http.DefaultTransport.RoundTrip(req)
	})

	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud,
		hubspot.WithBaseURL(srv.URL), hubspot.WithHTTPClient(&http.Client{Transport: rt}))

	before := time.Now()
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot,
		json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555"}}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	portalDeadline, ok := deadlines["/oauth/v2/private-apps/get/access-token-info"]
	if !ok {
		t.Fatal("the token-info request must carry a deadline, not the caller's un-timeboxed context")
	}
	if d := portalDeadline.Sub(before); d <= 0 || d > portalLookupTimeout+time.Second {
		t.Errorf("token-info deadline was %v out from Dispatch start, want within (0, portalLookupTimeout=%v]", d, portalLookupTimeout)
	}
	// The clone call, by contrast, is a MUTATING step and must NOT be truncated to the short
	// provenance-lookup budget. It still carries A deadline — every attempt gets its own
	// context.WithTimeout(ctx, c.requestTimeout) inside doRequest, unconditionally, regardless
	// of what the caller's context looked like (see client.go) — so presence/absence of a
	// deadline isn't the right signal. What must hold is that it is bounded by the CLIENT's
	// own per-attempt requestTimeout, not truncated down to the much shorter portalLookupTimeout.
	cloneDeadline, ok := deadlines["/marketing/v3/emails/clone"]
	if !ok {
		t.Fatal("the clone request must carry a deadline (doRequest's per-attempt timeout)")
	}
	if d := cloneDeadline.Sub(before); d <= portalLookupTimeout {
		t.Errorf("clone deadline was only %v out from Dispatch start, want materially more than portalLookupTimeout=%v — it must not have inherited the short portal-lookup budget", d, portalLookupTimeout)
	}
	// providerCallTimeout lives in internal/service (orchestrator.go) and cannot be imported
	// here without a cycle; its value (2m) is duplicated in this assertion so the two stay
	// honest with each other. If that constant changes, this must change too.
	const providerCallTimeoutForTest = 2 * time.Minute
	if portalLookupTimeout >= providerCallTimeoutForTest {
		t.Fatalf("portalLookupTimeout (%v) must stay well under providerCallTimeout (%v) or the guard is pointless", portalLookupTimeout, providerCallTimeoutForTest)
	}
}

func testCampaign() *model.Campaign {
	return &model.Campaign{
		ID:                  "camp-1",
		PlatformCampaignID:  "999",
		BriefID:             "brief-1",
		ProjectID:           "proj-1",
		Platform:            model.ProviderHubSpot,
		Status:              model.CampaignStatusPending,
		Result:              json.RawMessage(`{"portalId":"8112310"}`),
	}
}

func TestReadMetrics_PortalLookupTimeoutIsApplied(t *testing.T) {
	// Create a server that blocks on the token-info endpoint for longer than portalLookupTimeout
	// but shorter than the ambient context deadline (which is 20s from metricsCallTimeout).
	blockedReqs := make(chan struct{})
	portalsObserved := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/oauth/v2/private-apps/get/access-token-info" {
			portalsObserved++
			// Block longer than portalLookupTimeout (10s) but less than metricsCallTimeout (20s).
			// If the fix works, this blocks for 15s and ReadMetrics times out after 10s.
			// If the fix doesn't work, ReadMetrics waits the full 15s and burns the budget.
			blockedReqs <- struct{}{}
			time.Sleep(15 * time.Second)
			_, _ = io.WriteString(w, `{"hubId":"8112310"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	// Captures the DEADLINE the outgoing http.Request's context carried.
	deadlines := map[string]time.Time{}
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if dl, ok := req.Context().Deadline(); ok {
			deadlines[req.URL.Path] = dl
		}
		return http.DefaultTransport.RoundTrip(req)
	})

	d := NewHubSpotDispatcher(
		fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)},
		identityEncryptor{},
		fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)},
		hubspot.WithBaseURL(srv.URL),
		hubspot.WithHTTPClient(&http.Client{Transport: rt}),
	)

	campaign := testCampaign()
	// Give ReadMetrics an ambient context with metricsCallTimeout (20s).
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	before := time.Now()
	_, err := d.ReadMetrics(ctx, campaign.ProjectID, model.ProviderHubSpot, campaign, model.MetricsWindowLast30Days)
	elapsed := time.Since(before)

	// Should fail quickly due to portal lookup timeout, not wait the full 15s.
	// Allowing up to 2s of jitter/GC overhead.
	if elapsed > 12*time.Second {
		t.Errorf("ReadMetrics took %v, should have timed out after portalLookupTimeout (10s) + buffer, got %v", elapsed, err)
	}

	// Verify we did attempt the portal lookup.
	if portalsObserved == 0 {
		t.Fatal("portal lookup was never attempted")
	}

	// ReadMetrics should fail because the portal lookup timed out.
	if err == nil {
		t.Fatal("expected error due to portal lookup timeout, got nil")
	}

	// Verify the token-info request carried the short deadline.
	portalDeadline, ok := deadlines["/oauth/v2/private-apps/get/access-token-info"]
	if !ok {
		t.Fatal("the token-info request must carry a deadline")
	}
	if d := portalDeadline.Sub(before); d > portalLookupTimeout+2*time.Second {
		t.Errorf("token-info deadline was %v out from ReadMetrics start, want roughly portalLookupTimeout=%v", d, portalLookupTimeout)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
