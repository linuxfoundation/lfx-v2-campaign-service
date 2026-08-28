// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// mockCampaignSearcherDispatcher implements CampaignSearcher and deliberately NOT EmailSearcher:
// the two capabilities are independent, and a mock satisfying both would hide a handler wired to
// the wrong one.
type mockCampaignSearcherDispatcher struct {
	capped      bool
	campaigns   []model.HubSpotCampaign
	created     *model.HubSpotCampaign
	gotPlatform model.Provider
	gotQuery    string
	gotName     string
	searchErr   error
	createErr   error
	// nilCreate forces the (nil, nil) contract violation the handler must refuse.
	nilCreate bool
}

func (m *mockCampaignSearcherDispatcher) Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

// COMPILE-TIME assertion, because the runtime one is silent. campaignSearcherFor resolves this
// capability with a dynamic type assertion, so a mock whose signature drifts from the interface
// does not fail to build — it stops satisfying CampaignSearcher, every test below falls into the
// "platform unsupported" arm, and six tests go on passing while exercising nothing. That is
// exactly what happened when SearchCampaigns began returning a page. This line turns the next
// such drift into a build error.
var _ CampaignSearcher = (*mockCampaignSearcherDispatcher)(nil)

func (m *mockCampaignSearcherDispatcher) SearchCampaigns(_ context.Context, _ string, platform model.Provider, query string) (model.HubSpotCampaignPage, error) {
	m.gotPlatform, m.gotQuery = platform, query
	if m.searchErr != nil {
		return model.HubSpotCampaignPage{}, m.searchErr
	}
	return model.HubSpotCampaignPage{Campaigns: m.campaigns, Capped: m.capped}, nil
}

func (m *mockCampaignSearcherDispatcher) CreateCampaign(_ context.Context, _ string, platform model.Provider, name string) (*model.HubSpotCampaign, error) {
	m.gotPlatform, m.gotName = platform, name
	if m.createErr != nil {
		return nil, m.createErr
	}
	if m.nilCreate {
		return nil, nil
	}
	return m.created, nil
}

func newCampaignSvc(d PlatformDispatcher) *ConnectionService {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderHubSpot: d},
	})
	return svc
}

func TestSearchHubspotCampaigns_ReturnsMatchesInOrder(t *testing.T) {
	d := &mockCampaignSearcherDispatcher{campaigns: []model.HubSpotCampaign{
		{ID: "11", Name: "KubeCon NA 2026", UTM: "kubecon-na-2026", StartDate: "2026-11-01"},
		{ID: "22", Name: "KubeCon EU 2026", UTM: "kubecon-eu-2026", StartDate: "2026-03-01"},
	}}

	res, err := newCampaignSvc(d).SearchHubspotCampaigns(context.Background(),
		&conn.SearchHubspotCampaignsPayload{ProjectID: "cncf", Q: "KubeCon"})
	if err != nil {
		t.Fatalf("SearchHubspotCampaigns: %v", err)
	}
	if d.gotPlatform != model.ProviderHubSpot {
		t.Errorf("platform = %q, want hubspot", d.gotPlatform)
	}
	if d.gotQuery != "KubeCon" {
		t.Errorf("query = %q, want it forwarded verbatim", d.gotQuery)
	}
	if len(res.Campaigns) != 2 {
		t.Fatalf("campaigns = %d, want 2", len(res.Campaigns))
	}
	// Relevance order must survive: the caller shows these to a human picking between similar
	// names, and reordering would put a worse match first.
	if res.Campaigns[0].ID != "11" || res.Campaigns[1].ID != "22" {
		t.Errorf("order not preserved: %+v", res.Campaigns)
	}
	// Distinct values per field so a mapper that crossed two fails here.
	if res.Campaigns[0].Name != "KubeCon NA 2026" {
		t.Errorf("Name = %q", res.Campaigns[0].Name)
	}
	if res.Campaigns[0].Utm == nil || *res.Campaigns[0].Utm != "kubecon-na-2026" {
		t.Errorf("Utm not mapped: %+v", res.Campaigns[0].Utm)
	}
}

// An empty result is the answer the caller acts on by offering a create. It must serialize as
// `[]`, not `null` — this is the one field the caller reads without a null check.
func TestSearchHubspotCampaigns_NoMatchesIsAnEmptySliceNotNil(t *testing.T) {
	d := &mockCampaignSearcherDispatcher{campaigns: []model.HubSpotCampaign{}}

	res, err := newCampaignSvc(d).SearchHubspotCampaigns(context.Background(),
		&conn.SearchHubspotCampaignsPayload{ProjectID: "cncf", Q: "nothing"})
	if err != nil {
		t.Fatalf("an empty search must not error: %v", err)
	}
	if res.Campaigns == nil {
		t.Fatal("Campaigns = nil, want an empty slice so the wire carries [] rather than null")
	}
	if len(res.Campaigns) != 0 {
		t.Errorf("campaigns = %d, want 0", len(res.Campaigns))
	}
}

// A campaign with NO utm token is a real campaign. The wire field is optional, and an empty
// domain value must map to an ABSENT key rather than "" — a consumer cannot otherwise tell "no
// token configured" from "the producer sent an empty string", and treating it as "not found"
// would prompt a duplicate create in a namespace every foundation shares.
func TestSearchHubspotCampaigns_AbsentTokenIsOmittedNotEmptyString(t *testing.T) {
	d := &mockCampaignSearcherDispatcher{campaigns: []model.HubSpotCampaign{
		{ID: "11", Name: "Untokened"},
	}}

	res, err := newCampaignSvc(d).SearchHubspotCampaigns(context.Background(),
		&conn.SearchHubspotCampaignsPayload{ProjectID: "cncf", Q: "untokened"})
	if err != nil {
		t.Fatalf("SearchHubspotCampaigns: %v", err)
	}
	if len(res.Campaigns) != 1 {
		t.Fatalf("a campaign with no token was dropped: %+v", res.Campaigns)
	}
	if res.Campaigns[0].Utm != nil {
		t.Errorf("Utm = %q, want an ABSENT key for a campaign with no token", *res.Campaigns[0].Utm)
	}
	if res.Campaigns[0].StartDate != nil {
		t.Errorf("StartDate = %q, want absent when unset", *res.Campaigns[0].StartDate)
	}
}

// The reserved LF scope is unaddressable here for the same reason it is on every sibling read.
func TestSearchHubspotCampaigns_RejectsSystemScope(t *testing.T) {
	d := &mockCampaignSearcherDispatcher{}

	_, err := newCampaignSvc(d).SearchHubspotCampaigns(context.Background(),
		&conn.SearchHubspotCampaignsPayload{ProjectID: model.SystemProjectID, Q: "x"})
	if err == nil {
		t.Fatal("expected the reserved system scope to be rejected")
	}
	if _, ok := err.(*conn.NotFoundError); !ok {
		t.Errorf("error = %T (%v), want *conn.NotFoundError", err, err)
	}
	if d.gotQuery != "" {
		t.Error("the dispatcher was contacted for the reserved scope")
	}
}

func TestCreateHubspotCampaign_ReturnsTheAssignedToken(t *testing.T) {
	d := &mockCampaignSearcherDispatcher{created: &model.HubSpotCampaign{
		ID: "99", Name: "KubeCon NA 2027", UTM: "assigned-by-hubspot",
	}}

	got, err := newCampaignSvc(d).CreateHubspotCampaign(context.Background(),
		&conn.CreateHubspotCampaignPayload{ProjectID: "cncf", Name: "KubeCon NA 2027"})
	if err != nil {
		t.Fatalf("CreateHubspotCampaign: %v", err)
	}
	if d.gotName != "KubeCon NA 2027" {
		t.Errorf("name = %q, want it forwarded verbatim", d.gotName)
	}
	if got.ID != "99" {
		t.Errorf("ID = %q, want 99", got.ID)
	}
	// The token must be the one the platform assigned, not one this service invented.
	if got.Utm == nil || *got.Utm != "assigned-by-hubspot" {
		t.Errorf("Utm = %v, want the assigned token", got.Utm)
	}
}

// A create that reports success while returning nothing is worse than a clear failure: the
// caller would dereference a campaign reference that does not exist.
func TestCreateHubspotCampaign_NilWithoutErrorIsRefused(t *testing.T) {
	d := &mockCampaignSearcherDispatcher{nilCreate: true}

	_, err := newCampaignSvc(d).CreateHubspotCampaign(context.Background(),
		&conn.CreateHubspotCampaignPayload{ProjectID: "cncf", Name: "ghost"})
	if err == nil {
		t.Fatal("a nil campaign with no error was reported as success")
	}
	if _, ok := err.(*conn.InternalServerError); !ok {
		t.Errorf("error = %T (%v), want *conn.InternalServerError", err, err)
	}
}

func TestCreateHubspotCampaign_RejectsSystemScope(t *testing.T) {
	d := &mockCampaignSearcherDispatcher{}

	_, err := newCampaignSvc(d).CreateHubspotCampaign(context.Background(),
		&conn.CreateHubspotCampaignPayload{ProjectID: model.SystemProjectID, Name: "x"})
	if err == nil {
		t.Fatal("expected the reserved system scope to be rejected")
	}
	// The dispatcher must NOT have been contacted: this is a WRITE into a shared namespace, so
	// refusing after the fact would already have created the campaign.
	if d.gotName != "" {
		t.Error("a campaign was created for the reserved scope")
	}
}

// A platform with no campaign capability is a 400, not a 500 — the caller asked for something
// this platform cannot do, which is a permanent input fault rather than a service failure.
func TestSearchHubspotCampaigns_UnsupportedPlatformIsABadRequest(t *testing.T) {
	// A dispatcher implementing neither capability: the orchestrator's type assertion fails.
	d := &mockEmailSearcherDispatcher{}

	_, err := newCampaignSvc(d).SearchHubspotCampaigns(context.Background(),
		&conn.SearchHubspotCampaignsPayload{ProjectID: "cncf", Q: "x"})
	if err == nil {
		t.Fatal("expected an unsupported-capability refusal")
	}
	if _, ok := err.(*conn.BadRequestError); !ok {
		t.Errorf("error = %T (%v), want *conn.BadRequestError", err, err)
	}
	if !errors.Is(errors.Unwrap(err), nil) && err == nil {
		t.Fatal("unreachable")
	}
}

// A whitespace-only query is a BAD REQUEST, refused before the platform is contacted.
//
// Goa's MinLength(1) counts runes, so "   " passes the generated decoder. The client then
// refuses it with a sentinel-less error that classifyDiscoveryError reports as a retryable
// 503 — telling the caller to retry a request that can never succeed.
func TestSearchHubspotCampaigns_WhitespaceQueryIsABadRequest(t *testing.T) {
	d := &mockCampaignSearcherDispatcher{}

	_, err := newCampaignSvc(d).SearchHubspotCampaigns(context.Background(),
		&conn.SearchHubspotCampaignsPayload{ProjectID: "cncf", Q: "   "})
	if err == nil {
		t.Fatal("a whitespace-only query was accepted")
	}
	if _, ok := err.(*conn.BadRequestError); !ok {
		t.Errorf("error = %T (%v), want *conn.BadRequestError — a 503 would invite a pointless retry", err, err)
	}
	if d.gotQuery != "" {
		t.Error("the platform was contacted for an empty query")
	}
}

// A CREATE failure must not be reported as a retryable search failure.
//
// The read classifier's default arm says "campaign search could not be completed" and invites a
// retry. Both halves are wrong for a create: it names the wrong operation, and — far worse — a
// retried NON-IDEMPOTENT write into an LF-global namespace is how a duplicate campaign gets
// made. HubSpot marks mutating transport/429/3xx/5xx failures unconfirmed precisely because the
// campaign may already exist.
func TestCreateHubspotCampaign_FailureIsUnconfirmedNotRetryableSearch(t *testing.T) {
	d := &mockCampaignSearcherDispatcher{createErr: errors.New("upstream timeout")}

	_, err := newCampaignSvc(d).CreateHubspotCampaign(context.Background(),
		&conn.CreateHubspotCampaignPayload{ProjectID: "cncf", Name: "KubeCon NA 2027"})
	if err == nil {
		t.Fatal("a failed create was reported as success")
	}
	// Read from the Message FIELD, not Error(): the generated error types return "" from
	// Error() by design, so asserting on that would pass against any message at all.
	ue, ok := err.(*conn.ConnServiceUnavailableError)
	if !ok {
		t.Fatalf("error = %T (%v), want *conn.ConnServiceUnavailableError", err, err)
	}
	msg := ue.Message
	// The message must send the operator to HubSpot, not back through the button.
	if !strings.Contains(msg, "check HubSpot") {
		t.Errorf("message does not warn against a blind retry: %q", msg)
	}
	// And it must not describe a SEARCH — that is the read classifier's wording.
	if strings.Contains(strings.ToLower(msg), "campaign search") {
		t.Errorf("a create failure was reported as a search failure: %q", msg)
	}
}

// The orchestrator refuses a (nil, nil) contract violation rather than forwarding it. Forwarded,
// the service layer renders it as an authoritative empty list — and empty is what the caller
// acts on by creating a campaign in a shared namespace.
func TestSearchHubspotCampaigns_NilWithoutErrorIsRefused(t *testing.T) {
	d := &mockCampaignSearcherDispatcher{campaigns: nil}

	_, err := newCampaignSvc(d).SearchHubspotCampaigns(context.Background(),
		&conn.SearchHubspotCampaignsPayload{ProjectID: "cncf", Q: "kubecon"})
	if err == nil {
		t.Fatal("a nil result with no error was rendered as an authoritative empty answer")
	}
}

// Capped must survive the trip to the wire, because it changes what an EMPTY result MEANS. The
// caller offers a create on an empty search; while capped is true, absence is not proof of
// non-existence, and an unqualified create duplicates a campaign in a namespace every foundation
// shares. Dropping the flag anywhere in the chain fails open, silently.
func TestSearchHubspotCampaigns_CappedReachesTheWire(t *testing.T) {
	d := &mockCampaignSearcherDispatcher{campaigns: []model.HubSpotCampaign{}, capped: true}

	res, err := newCampaignSvc(d).SearchHubspotCampaigns(context.Background(),
		&conn.SearchHubspotCampaignsPayload{ProjectID: "cncf", Q: "kubecon"})
	if err != nil {
		t.Fatalf("SearchHubspotCampaigns: %v", err)
	}
	if !res.Capped {
		t.Error("Capped = false, want true — an empty-but-capped search must not read as proof the campaign does not exist")
	}
}

// The other direction, so the flag is not hard-coded true: an uncapped search reports false, and
// the caller may offer the create.
func TestSearchHubspotCampaigns_UncappedSearchIsNotFlagged(t *testing.T) {
	d := &mockCampaignSearcherDispatcher{campaigns: []model.HubSpotCampaign{}, capped: false}

	res, err := newCampaignSvc(d).SearchHubspotCampaigns(context.Background(),
		&conn.SearchHubspotCampaignsPayload{ProjectID: "cncf", Q: "kubecon"})
	if err != nil {
		t.Fatalf("SearchHubspotCampaigns: %v", err)
	}
	if res.Capped {
		t.Error("Capped = true on an uncapped search — the create offer would be suppressed for every operator")
	}
}
