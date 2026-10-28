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

// mockLookupDispatcher implements both targeting-lookup capabilities.
type mockLookupDispatcher struct {
	geo        []model.GeoTargetSuggestion
	lists      *model.AudienceUserLists
	err        error
	gotQuery   model.GeoTargetQuery
	gotAccount string
	calls      int
}

func (m *mockLookupDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (m *mockLookupDispatcher) SuggestGeoTargets(_ context.Context, _ string, _ model.Provider, q model.GeoTargetQuery) ([]model.GeoTargetSuggestion, error) {
	m.calls++
	m.gotQuery = q
	return m.geo, m.err
}

func (m *mockLookupDispatcher) ListUserLists(_ context.Context, _ string, _ model.Provider, accountID string) (*model.AudienceUserLists, error) {
	m.calls++
	m.gotAccount = accountID
	return m.lists, m.err
}

func lookupService(d PlatformDispatcher) *ConnectionService {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: d}})
	return svc
}

func TestSuggestGoogleAdsGeoTargets_FiltersTypesAndCapsAtLimit(t *testing.T) {
	d := &mockLookupDispatcher{geo: []model.GeoTargetSuggestion{
		{ID: "9031000", Name: "78701", TargetType: "Postal Code", CountryCode: "US", Status: "ENABLED"},
		{ID: "1026201", Name: "Austin", TargetType: "City", CountryCode: "US", Status: "ENABLED"},
		{ID: "9031001", Name: "78702", TargetType: "Postal Code", CountryCode: "US", Status: "ENABLED"},
		{ID: "9031002", Name: "78703", TargetType: "Postal Code", CountryCode: "US", Status: "REMOVAL_PLANNED"},
	}}
	res, err := lookupService(d).SuggestGoogleAdsGeoTargets(context.Background(), &conn.SuggestGoogleAdsGeoTargetsPayload{
		ProjectID: "cncf", Q: " 787 ", CountryCode: strp("us"), TargetTypes: []string{"postal_code"}, Limit: intp(2),
	})
	if err != nil {
		t.Fatalf("SuggestGoogleAdsGeoTargets: %v", err)
	}
	if d.gotQuery.Query != "787" || d.gotQuery.CountryCode != "US" {
		t.Errorf("query passed down = %+v, want trimmed query and upper-cased country", d.gotQuery)
	}
	if res.SuggestionCount != 2 || len(res.Suggestions) != 2 || !res.Truncated {
		t.Fatalf("got %d suggestions truncated=%v, want 2 and truncated", res.SuggestionCount, res.Truncated)
	}
	for _, s := range res.Suggestions {
		if s.TargetType != "Postal Code" {
			t.Errorf("suggestion %s has type %q; the City row must be filtered out", s.ID, s.TargetType)
		}
	}
	if res.Suggestions[0].ID != "9031000" || res.Suggestions[1].ID != "9031001" {
		t.Errorf("order not preserved: %s, %s", res.Suggestions[0].ID, res.Suggestions[1].ID)
	}
}

func TestSuggestGoogleAdsGeoTargets_EmptyIsAnEmptyArray(t *testing.T) {
	res, err := lookupService(&mockLookupDispatcher{geo: []model.GeoTargetSuggestion{}}).SuggestGoogleAdsGeoTargets(
		context.Background(), &conn.SuggestGoogleAdsGeoTargetsPayload{ProjectID: "cncf", Q: "nowhere"})
	if err != nil {
		t.Fatalf("SuggestGoogleAdsGeoTargets: %v", err)
	}
	b, _ := json.Marshal(res.Suggestions)
	if string(b) != "[]" || res.Truncated {
		t.Errorf("suggestions = %s truncated=%v, want [] and not truncated", b, res.Truncated)
	}
}

// Lengths are counted in characters, as the Goa decoder counts them: a 50-character
// non-ASCII place name (150 bytes) is within the published 100-character limit.
func TestSuggestGoogleAdsGeoTargets_CountsCharactersNotBytes(t *testing.T) {
	d := &mockLookupDispatcher{geo: []model.GeoTargetSuggestion{}}
	_, err := lookupService(d).SuggestGoogleAdsGeoTargets(context.Background(), &conn.SuggestGoogleAdsGeoTargetsPayload{
		ProjectID: "cncf", Q: strings.Repeat("東", 50), TargetTypes: []string{strings.Repeat("市", 39) + "1"},
	})
	if err != nil || d.calls != 1 {
		t.Fatalf("err=%v calls=%d; a within-limit multi-byte query must reach the dispatcher", err, d.calls)
	}
}

func TestSuggestGoogleAdsGeoTargets_RejectsBadInputBeforeAnyUpstreamCall(t *testing.T) {
	cases := map[string]*conn.SuggestGoogleAdsGeoTargetsPayload{
		"blank query":     {ProjectID: "cncf", Q: "   "},
		"bad country":     {ProjectID: "cncf", Q: "Austin", CountryCode: strp("USA")},
		"limit too large": {ProjectID: "cncf", Q: "Austin", Limit: intp(51)},
		"blank type":      {ProjectID: "cncf", Q: "Austin", TargetTypes: []string{" _ "}},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			d := &mockLookupDispatcher{geo: []model.GeoTargetSuggestion{}}
			_, err := lookupService(d).SuggestGoogleAdsGeoTargets(context.Background(), p)
			var br *conn.BadRequestError
			if !errors.As(err, &br) {
				t.Fatalf("err = %T %v, want 400", err, err)
			}
			if d.calls != 0 {
				t.Error("the dispatcher was called for a request refused locally")
			}
		})
	}
}

// Both lookups refuse the reserved LF scope with the same 404 every project_id-taking
// connection endpoint uses, before any backend is consulted.
func TestGoogleAdsLookups_RejectTheReservedSystemScope(t *testing.T) {
	svc := NewConnectionService(nil, nil) // no backend: the guard must run first
	_, gerr := svc.SuggestGoogleAdsGeoTargets(context.Background(), &conn.SuggestGoogleAdsGeoTargetsPayload{ProjectID: model.SystemProjectID, Q: "Austin"})
	_, lerr := svc.ListGoogleAdsUserLists(context.Background(), &conn.ListGoogleAdsUserListsPayload{ProjectID: model.SystemProjectID})
	for name, err := range map[string]error{"geo": gerr, "user lists": lerr} {
		var nf *conn.NotFoundError
		if !errors.As(err, &nf) {
			t.Errorf("%s: err = %T %v, want 404", name, err, err)
		}
	}
}

func TestGoogleAdsLookups_ErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"no connection is 404", fmt.Errorf("wrapped: %w", domain.ErrNotFound), "404"},
		{"unusable connection is 400", fmt.Errorf("%w: %w", domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected), "400"},
		{"malformed account is 400", fmt.Errorf("%w: x", domain.ErrAccountIDMalformed), "400"},
		{"upstream failure is 503", errors.New("google said no"), "503"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &mockLookupDispatcher{err: tc.err}
			_, gerr := lookupService(d).SuggestGoogleAdsGeoTargets(context.Background(), &conn.SuggestGoogleAdsGeoTargetsPayload{ProjectID: "cncf", Q: "Austin"})
			_, lerr := lookupService(d).ListGoogleAdsUserLists(context.Background(), &conn.ListGoogleAdsUserListsPayload{ProjectID: "cncf"})
			for name, err := range map[string]error{"geo": gerr, "user lists": lerr} {
				if got := errCode(err); got != tc.want {
					t.Errorf("%s: code = %q (%T %v), want %s", name, got, err, err, tc.want)
				}
			}
		})
	}
}

func errCode(err error) string {
	var (
		nf *conn.NotFoundError
		br *conn.BadRequestError
		su *conn.ConnServiceUnavailableError
		ie *conn.InternalServerError
	)
	switch {
	case errors.As(err, &nf):
		return nf.Code
	case errors.As(err, &br):
		return br.Code
	case errors.As(err, &su):
		return su.Code
	case errors.As(err, &ie):
		return ie.Code
	}
	return ""
}

func TestGoogleAdsLookups_UnsupportedDispatcherIs400(t *testing.T) {
	svc := lookupService(&mockDispatcher{})
	_, gerr := svc.SuggestGoogleAdsGeoTargets(context.Background(), &conn.SuggestGoogleAdsGeoTargetsPayload{ProjectID: "cncf", Q: "Austin"})
	_, lerr := svc.ListGoogleAdsUserLists(context.Background(), &conn.ListGoogleAdsUserListsPayload{ProjectID: "cncf"})
	if errCode(gerr) != "400" || errCode(lerr) != "400" {
		t.Errorf("geo=%v lists=%v, want 400 for both", gerr, lerr)
	}
}

func TestListGoogleAdsUserLists_MapsAndPassesTheAccount(t *testing.T) {
	size := int64(12000)
	d := &mockLookupDispatcher{lists: &model.AudienceUserLists{AccountID: "8666746580", Lists: []model.AudienceUserList{{
		ResourceName: "customers/8666746580/userLists/7012345678", ID: "7012345678", Name: "Registrants",
		Type: "CRM_BASED", MembershipStatus: "OPEN", SizeForSearch: &size, EligibleForSearch: true,
	}}}}
	res, err := lookupService(d).ListGoogleAdsUserLists(context.Background(), &conn.ListGoogleAdsUserListsPayload{ProjectID: "cncf", AccountID: strp("8666746580")})
	if err != nil {
		t.Fatalf("ListGoogleAdsUserLists: %v", err)
	}
	if d.gotAccount != "8666746580" {
		t.Errorf("account passed down = %q", d.gotAccount)
	}
	if res.AccountID != "8666746580" || res.UserListCount != 1 || res.Truncated {
		t.Fatalf("result = %+v", res)
	}
	l := res.UserLists[0]
	if l.ResourceName != "customers/8666746580/userLists/7012345678" || !l.EligibleForSearch || l.SizeForSearch == nil || *l.SizeForSearch != 12000 {
		t.Errorf("list = %+v", l)
	}
	if l.AccountStatus != nil || l.SizeRangeForSearch != nil {
		t.Error("fields Google did not send must be absent, not empty strings")
	}
}

func TestListGoogleAdsUserLists_CapsAndReportsTruncation(t *testing.T) {
	lists := make([]model.AudienceUserList, googleAdsUserListMax+1)
	for i := range lists {
		lists[i] = model.AudienceUserList{ResourceName: fmt.Sprintf("customers/1/userLists/%d", i+1), ID: fmt.Sprint(i + 1)}
	}
	d := &mockLookupDispatcher{lists: &model.AudienceUserLists{AccountID: "1", Lists: lists}}
	res, err := lookupService(d).ListGoogleAdsUserLists(context.Background(), &conn.ListGoogleAdsUserListsPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("ListGoogleAdsUserLists: %v", err)
	}
	if res.UserListCount != googleAdsUserListMax || !res.Truncated {
		t.Errorf("count=%d truncated=%v, want %d and truncated", res.UserListCount, res.Truncated, googleAdsUserListMax)
	}
	if d.gotAccount != "" {
		t.Errorf("an omitted account_id must reach the dispatcher as empty, got %q", d.gotAccount)
	}
}

func TestListGoogleAdsUserLists_RejectsAMalformedAccountLocally(t *testing.T) {
	d := &mockLookupDispatcher{}
	_, err := lookupService(d).ListGoogleAdsUserLists(context.Background(), &conn.ListGoogleAdsUserListsPayload{ProjectID: "cncf", AccountID: strp("866-674-6580")})
	if errCode(err) != "400" || d.calls != 0 {
		t.Errorf("err=%v calls=%d, want a local 400", err, d.calls)
	}
}

func TestReadUserLists_NilResultIsAnError(t *testing.T) {
	o := &Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: &mockLookupDispatcher{lists: &model.AudienceUserLists{}}}}
	if _, err := o.ReadUserLists(context.Background(), "cncf", model.ProviderGoogleAds, ""); err == nil {
		t.Error("a nil Lists slice must not be reported as an empty answer")
	}
	o = &Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: &mockLookupDispatcher{}}}
	if _, err := o.ReadGeoTargetSuggestions(context.Background(), "cncf", model.ProviderGoogleAds, model.GeoTargetQuery{Query: "x"}); err == nil {
		t.Error("a nil suggestion slice must not be reported as an empty answer")
	}
}

func intp(v int) *int { return &v }

// An omitted limit is the documented default of 20, not zero.
func TestSuggestGoogleAdsGeoTargets_OmittedLimitDefaultsTo20(t *testing.T) {
	rows := make([]model.GeoTargetSuggestion, 25)
	for i := range rows {
		rows[i] = model.GeoTargetSuggestion{ID: fmt.Sprint(i + 1)}
	}
	res, err := lookupService(&mockLookupDispatcher{geo: rows}).SuggestGoogleAdsGeoTargets(context.Background(),
		&conn.SuggestGoogleAdsGeoTargetsPayload{ProjectID: "cncf", Q: "a"})
	if err != nil {
		t.Fatalf("SuggestGoogleAdsGeoTargets: %v", err)
	}
	if res.SuggestionCount != 20 || !res.Truncated {
		t.Errorf("count=%d truncated=%v, want 20 and truncated", res.SuggestionCount, res.Truncated)
	}
}

// sharedTenancyLookupDispatcher is mockLookupDispatcher on a shared-tenancy platform: it
// carries the AccountMonitorScoper marker the shared platforms' dispatchers implement.
type sharedTenancyLookupDispatcher struct{ mockLookupDispatcher }

func (*sharedTenancyLookupDispatcher) MonitorScopePrecheck(string) error { return nil }
func (*sharedTenancyLookupDispatcher) MonitorScopeForAccount([]model.ProjectCampaignScope, string) ([]string, error) {
	return nil, nil
}

// On a shared-tenancy platform the account-wide user-list read is refused with 409 before the
// dispatcher is reached, whatever account the caller names.
func TestListGoogleAdsUserLists_SharedTenancyIsRefusedWith409(t *testing.T) {
	for _, acct := range []*string{nil, strp("8666746580")} {
		d := &sharedTenancyLookupDispatcher{mockLookupDispatcher{lists: &model.AudienceUserLists{Lists: []model.AudienceUserList{}}}}
		_, err := lookupService(d).ListGoogleAdsUserLists(context.Background(), &conn.ListGoogleAdsUserListsPayload{ProjectID: "cncf", AccountID: acct})
		var c *conn.ConflictError
		if !errors.As(err, &c) || c.Code != "409" {
			t.Fatalf("err = %T %v, want 409", err, err)
		}
		if d.calls != 0 {
			t.Error("the dispatcher was reached on a shared-tenancy platform")
		}
	}
}
