// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// The dispatcher satisfies the orchestrator's optional email-search capability. Asserted at
// compile time rather than left to the type assertion in Orchestrator.SearchEmails, where a
// signature drift would surface as a 400 "not supported for this platform" instead of a build
// failure — the same reasoning as the ReadMetrics assertion in hubspot_metrics_test.go.
var _ interface {
	SearchEmails(context.Context, string, model.Provider, string) ([]model.MarketingEmail, error)
} = (*HubSpotDispatcher)(nil)

// TestHubSpot_SearchEmailsPreservesEveryField covers the ADAPTER SEAM, which neither
// neighbouring suite reaches.
//
// The platform test proves `state` is requested and decodes into `hubspot.Email`; the service
// test injects a `model.MarketingEmail` directly through a mock searcher. Between them sits the
// per-field copy in HubSpotDispatcher.SearchEmails, and deleting any line of it — `State:
// e.State` most consequentially, since an empty state was the bug LFXV2-3197 shipped — leaves
// both suites green. Only a test that starts at an HTTP response and ends at the domain model
// binds that mapping.
func TestHubSpot_SearchEmailsPreservesEveryField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Distinctive values per field: a swapped Name/Subject mapping would still populate
		// both, and identical fixtures would let it pass.
		_, _ = io.WriteString(w, `{"results":[`+
			`{"id":"112233","name":"KubeCon EU 2026 — announce","subject":"Registration is open",`+
			`"state":"DRAFT","updatedAt":"2026-08-01T17:04:00Z"}`+
			`]}`)
	}))
	t.Cleanup(srv.Close)

	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{},
		fakeAudienceReader{}, hubspot.WithBaseURL(srv.URL))

	got, err := d.SearchEmails(context.Background(), "proj-1", model.ProviderHubSpot, "kubecon")
	if err != nil {
		t.Fatalf("SearchEmails: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d emails, want 1", len(got))
	}

	want := model.MarketingEmail{
		ID:        "112233",
		Name:      "KubeCon EU 2026 — announce",
		Subject:   "Registration is open",
		State:     "DRAFT",
		UpdatedAt: "2026-08-01T17:04:00Z",
	}
	// Compared whole rather than field by field: a NEW field added to model.MarketingEmail and
	// forgotten in the mapping fails here, which a per-field assertion would not catch.
	if got[0] != want {
		t.Errorf("SearchEmails mapped %+v, want %+v", got[0], want)
	}
}

// A portal with no matching email must produce an empty NON-NIL slice. service.EmailSearcher
// requires it, and Orchestrator.SearchEmails rejects (nil, nil) as a contract violation — so a
// dispatcher that returned nil here would turn "no match" into a 503 rather than an empty picker.
func TestHubSpot_SearchEmailsReturnsEmptyNotNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	t.Cleanup(srv.Close)

	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{},
		fakeAudienceReader{}, hubspot.WithBaseURL(srv.URL))

	got, err := d.SearchEmails(context.Background(), "proj-1", model.ProviderHubSpot, "nothing-matches")
	if err != nil {
		t.Fatalf("SearchEmails: %v", err)
	}
	if got == nil {
		t.Fatal("returned nil; the orchestrator treats (nil, nil) as a contract violation, so an empty portal would answer 503")
	}
	if len(got) != 0 {
		t.Errorf("got %d emails, want 0", len(got))
	}
}
