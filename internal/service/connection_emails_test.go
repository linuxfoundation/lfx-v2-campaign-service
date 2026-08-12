// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// mockEmailSearcherDispatcher implements EmailSearcher, and deliberately does NOT implement
// AccountLister — the two capabilities are independent, and a mock that satisfied both would
// hide a handler wired to the wrong one.
type mockEmailSearcherDispatcher struct {
	emails      []model.MarketingEmail
	gotPlatform model.Provider
	gotQuery    string
	err         error
	// nilResult forces the (nil, nil) contract violation the orchestrator must reject.
	nilResult bool
}

func (m *mockEmailSearcherDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (m *mockEmailSearcherDispatcher) SearchEmails(ctx context.Context, projectID string, platform model.Provider, query string) ([]model.MarketingEmail, error) {
	m.gotPlatform = platform
	m.gotQuery = query
	if m.err != nil {
		return nil, m.err
	}
	if m.nilResult {
		return nil, nil
	}
	return m.emails, nil
}

func newEmailSvc(d PlatformDispatcher) *ConnectionService {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderHubSpot: d},
	})
	return svc
}

func TestListHubspotEmails_ReturnsThePortalsEmails(t *testing.T) {
	searcher := &mockEmailSearcherDispatcher{
		emails: []model.MarketingEmail{
			{ID: "112233", Name: "KubeCon EU 2026 — announce", Subject: "Registration is open", State: "PUBLISHED", UpdatedAt: "2026-08-01T17:04:00Z"},
			{ID: "445566", Name: "KubeCon EU 2026 — reminder", Subject: "Two weeks left", State: "DRAFT", UpdatedAt: "2026-07-30T09:00:00Z"},
		},
	}
	q := "KubeCon"
	result, err := newEmailSvc(searcher).ListHubspotEmails(context.Background(),
		&conn.ListHubspotEmailsPayload{ProjectID: "p", Q: &q})
	if err != nil {
		t.Fatalf("ListHubspotEmails failed: %v", err)
	}
	if searcher.gotPlatform != model.ProviderHubSpot {
		t.Fatalf("dispatcher asked for provider %q, want %q", searcher.gotPlatform, model.ProviderHubSpot)
	}
	if searcher.gotQuery != "KubeCon" {
		t.Fatalf("query = %q, want it forwarded verbatim", searcher.gotQuery)
	}
	if len(result.Emails) != 2 {
		t.Fatalf("expected 2 emails, got %d", len(result.Emails))
	}
	// State travels: a picker shows that a template is still a DRAFT before someone clones
	// it. NOT archived — HubSpot tracks archival as a separate flag and the search does not
	// request archived rows, so they never appear at all. The client must REQUEST `state` in
	// includedProperties for this to be non-empty; it is not returned by default.
	if result.Emails[1].State == nil || *result.Emails[1].State != "DRAFT" {
		t.Errorf("second email state = %v, want DRAFT — the picker cannot warn about a state it never receives", result.Emails[1].State)
	}
}

// An omitted q is not an error: it lists the most recently updated emails, which is the useful
// first screen for a picker that has not been typed into yet.
func TestListHubspotEmails_AbsentQueryListsRatherThanFails(t *testing.T) {
	searcher := &mockEmailSearcherDispatcher{emails: []model.MarketingEmail{}}
	_, err := newEmailSvc(searcher).ListHubspotEmails(context.Background(),
		&conn.ListHubspotEmailsPayload{ProjectID: "p"})
	if err != nil {
		t.Fatalf("absent q should list, not fail: %v", err)
	}
	if searcher.gotQuery != "" {
		t.Errorf("query = %q, want the empty string", searcher.gotQuery)
	}
}

// An empty result must serialize as [] rather than null, so a client need not special-case a
// portal that has no matching email.
func TestListHubspotEmails_EmptyResultIsNotNil(t *testing.T) {
	result, err := newEmailSvc(&mockEmailSearcherDispatcher{emails: []model.MarketingEmail{}}).
		ListHubspotEmails(context.Background(), &conn.ListHubspotEmailsPayload{ProjectID: "p"})
	if err != nil {
		t.Fatalf("ListHubspotEmails failed: %v", err)
	}
	if result.Emails == nil {
		t.Fatal("Emails is nil; an empty answer must marshal as [], not null")
	}
}

// A searcher returning (nil, nil) is a contract violation, not an empty portal. Reporting it as
// success would render an empty picker as fact and send someone hunting for a permissions
// problem in HubSpot that does not exist.
func TestListHubspotEmails_NilResultIsAnError(t *testing.T) {
	_, err := newEmailSvc(&mockEmailSearcherDispatcher{nilResult: true}).
		ListHubspotEmails(context.Background(), &conn.ListHubspotEmailsPayload{ProjectID: "p"})
	if err == nil {
		t.Fatal("expected an error for a nil result; an empty picker must not be reported as fact")
	}
}

// The handler reuses account discovery's status mapping, so the arms have to be exercised HERE
// too: a mapping shared by reference is still a mapping this endpoint can be wired away from.
func TestListHubspotEmails_ClassifiesFailures(t *testing.T) {
	t.Run("no connection is 404 naming hubspot", func(t *testing.T) {
		_, err := newEmailSvc(&mockEmailSearcherDispatcher{err: domain.ErrNotFound}).
			ListHubspotEmails(context.Background(), &conn.ListHubspotEmailsPayload{ProjectID: "p"})
		notFound, ok := err.(*conn.NotFoundError)
		if !ok {
			t.Fatalf("expected NotFoundError, got %T: %v", err, err)
		}
		if !strings.Contains(notFound.Message, "hubspot") {
			t.Errorf("message = %q, want it to name hubspot", notFound.Message)
		}
	})

	// The defect is permanent until a human edits the connection, so 503 would be a false
	// promise that waiting helps.
	t.Run("unusable connection is 400 naming the published field", func(t *testing.T) {
		wrapped := fmt.Errorf("%w: %w: hubspot credentials need privateAppToken",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsIncomplete)
		_, err := newEmailSvc(&mockEmailSearcherDispatcher{err: wrapped}).
			ListHubspotEmails(context.Background(), &conn.ListHubspotEmailsPayload{ProjectID: "p"})
		badReq, ok := err.(*conn.BadRequestError)
		if !ok {
			t.Fatalf("expected BadRequestError, got %T: %v", err, err)
		}
		// private_app_token is the name on the WIRE (design/connection.go); the persisted Go
		// shape is privateAppToken, and a caller cannot send that one.
		if !strings.Contains(badReq.Message, "private_app_token") {
			t.Errorf("message = %q, want it to name private_app_token, the field a caller can send", badReq.Message)
		}
	})

	// The message must name what was attempted. "account discovery could not be completed" is
	// wrong here twice over: HubSpot has no ad account, and the caller searched for a template.
	t.Run("upstream failure is 503 naming email search", func(t *testing.T) {
		_, err := newEmailSvc(&mockEmailSearcherDispatcher{err: errors.New("hubspot 500")}).
			ListHubspotEmails(context.Background(), &conn.ListHubspotEmailsPayload{ProjectID: "p"})
		unavailable, ok := err.(*conn.ConnServiceUnavailableError)
		if !ok {
			t.Fatalf("expected ConnServiceUnavailableError, got %T: %v", err, err)
		}
		if !strings.Contains(unavailable.Message, "email search") {
			t.Errorf("message = %q, want it to name email search", unavailable.Message)
		}
		if strings.Contains(unavailable.Message, "account discovery") {
			t.Errorf("message = %q describes an operation the caller did not perform", unavailable.Message)
		}
	})
}

// During cold start the orchestrator is not yet wired, and the 503 must name what the CALLER
// attempted. Before `resolveBackendWithOrch` took an operation, an email search in that window
// was told "account discovery service is unavailable" — an operation this endpoint does not
// perform, sending whoever read it to check the wrong subsystem.
func TestListHubspotEmails_ColdStartNamesEmailSearch(t *testing.T) {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	// No SetOrchestrator: this is exactly the pre-wiring state.

	_, err := svc.ListHubspotEmails(context.Background(), &conn.ListHubspotEmailsPayload{ProjectID: "p"})

	unavailable, ok := err.(*conn.ConnServiceUnavailableError)
	if !ok {
		t.Fatalf("expected ConnServiceUnavailableError, got %T: %v", err, err)
	}
	if !strings.Contains(unavailable.Message, "email search") {
		t.Errorf("message = %q, want it to name email search", unavailable.Message)
	}
	if strings.Contains(unavailable.Message, "account discovery") {
		t.Errorf("message = %q names an operation this endpoint does not perform", unavailable.Message)
	}
}

// A dispatcher with no EmailSearcher is a caller error (400), not a transient outage: asking a
// platform this service cannot search will never start working on its own.
func TestListHubspotEmails_UnsupportedPlatformIs400(t *testing.T) {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderHubSpot: &mockDispatcher{}},
	})
	_, err := svc.ListHubspotEmails(context.Background(), &conn.ListHubspotEmailsPayload{ProjectID: "p"})
	if _, ok := err.(*conn.BadRequestError); !ok {
		t.Fatalf("expected BadRequestError for a dispatcher without EmailSearcher, got %T: %v", err, err)
	}
}
