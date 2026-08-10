// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

const statsResponse = `{"emails":[4242],"campaignAggregations":{},"aggregate":{"counters":` +
	`{"sent":1000,"delivered":950,"open":400,"click":80,"bounce":50,"unsubscribed":7}}}`

// statsRec captures what the fake statistics server saw. Written by the HANDLER goroutine
// and read by the TEST goroutine, so guarded — same reason as hubspotRec.
type statsRec struct {
	mu     sync.Mutex
	auth   string
	called bool
}

func (r *statsRec) mark(auth string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.auth, r.called = auth, true
}
func (r *statsRec) Auth() string { r.mu.Lock(); defer r.mu.Unlock(); return r.auth }
func (r *statsRec) Called() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.called }

func statsServer(t *testing.T) (*httptest.Server, *statsRec) {
	t.Helper()
	rec := &statsRec{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mark(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, statsResponse)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func stagedEmail() *model.Campaign {
	return &model.Campaign{ID: "camp-uuid-1", Platform: model.ProviderHubSpot, PlatformCampaignID: "4242"}
}

func TestHubSpot_ReadMetricsReturnsEmailCounters(t *testing.T) {
	srv, rec := statsServer(t)
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{},
		fakeAudienceReader{}, hubspot.WithBaseURL(srv.URL))

	m, err := d.ReadMetrics(context.Background(), "proj-1", model.ProviderHubSpot, stagedEmail(), model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("ReadMetrics: %v", err)
	}
	// The SERVICE's campaign UUID, not the HubSpot email id the platform client keyed its
	// result by — the API contract for campaign_id is the former, and the client has no way
	// to know it.
	if m.CampaignID != "camp-uuid-1" {
		t.Errorf("campaign_id = %q, want camp-uuid-1", m.CampaignID)
	}
	if m.Email == nil || m.Email.Delivered != 950 || m.Email.Unsubscribes != 7 {
		t.Errorf("email counters = %+v", m.Email)
	}
	if m.Impressions != 400 || m.Clicks != 80 {
		t.Errorf("impressions/clicks = %d/%d, want 400/80", m.Impressions, m.Clicks)
	}
	if !rec.Called() {
		t.Error("the statistics endpoint was never called")
	}
}

// A campaign with no platform id was never staged upstream, so there is nothing to read.
// The orchestrator maps this to 409, not a platform failure.
func TestHubSpot_ReadMetricsRejectsAnUnprovisionedCampaign(t *testing.T) {
	srv, rec := statsServer(t)
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{},
		fakeAudienceReader{}, hubspot.WithBaseURL(srv.URL))
	c := stagedEmail()
	c.PlatformCampaignID = ""
	if _, err := d.ReadMetrics(context.Background(), "proj-1", model.ProviderHubSpot, c, model.MetricsWindowToday); err == nil {
		t.Fatal("an unprovisioned campaign was accepted")
	}
	if rec.Called() {
		t.Error("a request was sent for an unprovisioned campaign")
	}
}

// The load-bearing ordering assertion. An unsupported window is a permanent 400 whatever
// the connection looks like, so it must be detected BEFORE credentials are resolved — the
// connection here is missing entirely, and resolving first would surface that instead and
// tell the caller to retry a request that can never succeed.
func TestHubSpot_ReadMetricsValidatesTheWindowBeforeResolvingCredentials(t *testing.T) {
	d := NewHubSpotDispatcher(fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{}, fakeAudienceReader{})
	_, err := d.ReadMetrics(context.Background(), "proj-1", model.ProviderHubSpot, stagedEmail(), model.MetricsWindow("last_quarter"))
	if err == nil {
		t.Fatal("an unsupported window was accepted")
	}
	if !errors.Is(err, domain.ErrMetricsWindowUnsupported) {
		t.Fatalf("err = %v, want it to wrap ErrMetricsWindowUnsupported", err)
	}
	if errors.Is(err, domain.ErrNotFound) {
		t.Error("the missing connection was reported instead of the unsupported window")
	}
}

func TestHubSpot_ReadMetricsRejectsAnInactiveConnection(t *testing.T) {
	srv, rec := statsServer(t)
	conn := activeHubSpotConn(goodHubSpotCreds)
	conn.Status = model.StatusInactive
	d := NewHubSpotDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, fakeAudienceReader{},
		hubspot.WithBaseURL(srv.URL))
	if _, err := d.ReadMetrics(context.Background(), "proj-1", model.ProviderHubSpot, stagedEmail(), model.MetricsWindowToday); err == nil {
		t.Fatal("an inactive connection was accepted")
	}
	if rec.Called() {
		t.Error("a request was sent on an inactive connection")
	}
}

// A whitespace-only stored token is an INCOMPLETE credential, not a token. The dispatcher
// must say so — hubspot.NewClient trims too (client.go:189), so nothing malformed could
// reach the wire regardless, but without the trim here the emptiness check passes and the
// failure resurfaces later from inside the client as a generic missing-token error that
// does not point at the stored connection.
func TestHubSpot_ReadMetricsRejectsAWhitespaceOnlyToken(t *testing.T) {
	srv, rec := statsServer(t)
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(`{"PrivateAppToken":"   "}`)},
		identityEncryptor{}, fakeAudienceReader{}, hubspot.WithBaseURL(srv.URL))
	_, err := d.ReadMetrics(context.Background(), "proj-1", model.ProviderHubSpot, stagedEmail(), model.MetricsWindowToday)
	if err == nil {
		t.Fatal("a whitespace-only token was accepted")
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("err = %v, want it to name the credential as incomplete", err)
	}
	if rec.Called() {
		t.Error("a request was sent with no usable token")
	}
}

// Padding around a real token never reaches the Authorization header. This pins the
// END-TO-END property, not the dispatcher's trim specifically: removing the trim here still
// passes, because hubspot.NewClient trims again (client.go:189). What the dispatcher's trim
// is actually load-bearing for is the emptiness check above — see the revert-diagnostic in
// TestHubSpot_ReadMetricsRejectsAWhitespaceOnlyToken, which is the binding one.
func TestHubSpot_ReadMetricsTrimsPaddingOffTheStoredToken(t *testing.T) {
	srv, rec := statsServer(t)
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn("{\"PrivateAppToken\":\"  pat-123 \"}")},
		identityEncryptor{}, fakeAudienceReader{}, hubspot.WithBaseURL(srv.URL))
	if _, err := d.ReadMetrics(context.Background(), "proj-1", model.ProviderHubSpot, stagedEmail(), model.MetricsWindowToday); err != nil {
		t.Fatalf("ReadMetrics: %v", err)
	}
	if got := rec.Auth(); got != "Bearer pat-123" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer pat-123")
	}
}

// The dispatcher satisfies the orchestrator's optional metrics capability. Asserted at
// compile time here rather than left to the type assertion in ReadCampaignMetrics, where a
// signature drift would surface as a 400 "not supported for this platform" instead of a
// build failure.
var _ interface {
	ReadMetrics(context.Context, string, model.Provider, *model.Campaign, model.MetricsWindow) (*model.CampaignMetrics, error)
} = (*HubSpotDispatcher)(nil)

// An empty statistics match is a successful read of nothing. The dispatcher must mark it so
// the service can answer 409; unmarked it takes the 503 default, which reports an outage for
// the ordinary case — the staged draft nobody has sent yet.
func TestHubSpot_ReadMetricsMarksAnEmptyWindowAsNoData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"emails":[],"campaignAggregations":{},"aggregate":{"counters":{}}}`)
	}))
	t.Cleanup(srv.Close)
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{},
		fakeAudienceReader{}, hubspot.WithBaseURL(srv.URL))

	_, err := d.ReadMetrics(context.Background(), "proj-1", model.ProviderHubSpot, stagedEmail(), model.MetricsWindowLast30Days)
	if err == nil {
		t.Fatal("an empty window was reported as a successful read")
	}
	if !errors.Is(err, domain.ErrNoMetricsInWindow) {
		t.Fatalf("err = %v, want it to wrap ErrNoMetricsInWindow", err)
	}
	// The platform cause survives alongside the domain marker, so the log still says which
	// upstream condition produced it.
	if !errors.Is(err, hubspot.ErrNoSentEmailInWindow) {
		t.Error("the hubspot cause was dropped from the error chain")
	}
}
