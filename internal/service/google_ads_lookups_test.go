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

// mockLookupDispatcher implements the four create-form lookup capabilities.
type mockLookupDispatcher struct {
	geo        []model.GeoTargetSuggestion
	notCalled  bool // SuggestGeoTargets and ListConversionActions report Google was not reached
	lists      *model.AudienceUserLists
	actions    *model.ConversionActions
	audiences  *model.AudienceResources
	err        error
	gotKind    string
	gotQuery   model.GeoTargetQuery
	gotAccount string
	calls      int
}

func (m *mockLookupDispatcher) Dispatch(context.Context, *model.CampaignBrief, model.Provider, json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (m *mockLookupDispatcher) SuggestGeoTargets(_ context.Context, _ string, _ model.Provider, q model.GeoTargetQuery) ([]model.GeoTargetSuggestion, bool, error) {
	m.calls++
	m.gotQuery = q
	return m.geo, !m.notCalled, m.err
}

func (m *mockLookupDispatcher) ListUserLists(_ context.Context, _ string, _ model.Provider, accountID string) (*model.AudienceUserLists, error) {
	m.calls++
	m.gotAccount = accountID
	return m.lists, m.err
}

func (m *mockLookupDispatcher) ListConversionActions(context.Context, string, model.Provider) (*model.ConversionActions, bool, error) {
	m.calls++
	return m.actions, !m.notCalled, m.err
}

func (m *mockLookupDispatcher) ListAudiences(_ context.Context, _ string, _ model.Provider, kind string) (*model.AudienceResources, bool, error) {
	m.calls++
	m.gotKind = kind
	return m.audiences, !m.notCalled, m.err
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

// Every lookup refuses the reserved LF scope with the same 404 every project_id-taking
// connection endpoint uses, before any backend is consulted.
func TestGoogleAdsLookups_RejectTheReservedSystemScope(t *testing.T) {
	svc := NewConnectionService(nil, nil) // no backend: the guard must run first
	_, gerr := svc.SuggestGoogleAdsGeoTargets(context.Background(), &conn.SuggestGoogleAdsGeoTargetsPayload{ProjectID: model.SystemProjectID, Q: "Austin"})
	_, lerr := svc.ListGoogleAdsUserLists(context.Background(), &conn.ListGoogleAdsUserListsPayload{ProjectID: model.SystemProjectID})
	_, cerr := svc.ListGoogleAdsConversionActions(context.Background(), &conn.ListGoogleAdsConversionActionsPayload{ProjectID: model.SystemProjectID})
	_, aerr := svc.ListGoogleAdsAudiences(context.Background(), &conn.ListGoogleAdsAudiencesPayload{ProjectID: model.SystemProjectID, Type: "audience"})
	for name, err := range map[string]error{"geo": gerr, "user lists": lerr, "conversion actions": cerr, "audiences": aerr} {
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
		{"stored account id not digits is 400", fmt.Errorf("%w: %w: x", domain.ErrConnectionNotUsable, domain.ErrProviderConfigInvalid), "400"},
		{"unusable LF system connection is 500", fmt.Errorf("%w: %w", domain.ErrSystemConnectionNotUsable, domain.ErrConnectionNotUsable), "500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &mockLookupDispatcher{err: tc.err}
			_, gerr := lookupService(d).SuggestGoogleAdsGeoTargets(context.Background(), &conn.SuggestGoogleAdsGeoTargetsPayload{ProjectID: "cncf", Q: "Austin"})
			_, lerr := lookupService(d).ListGoogleAdsUserLists(context.Background(), &conn.ListGoogleAdsUserListsPayload{ProjectID: "cncf"})
			_, cerr := lookupService(d).ListGoogleAdsConversionActions(context.Background(), &conn.ListGoogleAdsConversionActionsPayload{ProjectID: "cncf"})
			_, aerr := lookupService(d).ListGoogleAdsAudiences(context.Background(), &conn.ListGoogleAdsAudiencesPayload{ProjectID: "cncf", Type: "custom_audience"})
			for name, err := range map[string]error{"geo": gerr, "user lists": lerr, "conversion actions": cerr, "audiences": aerr} {
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
	_, cerr := svc.ListGoogleAdsConversionActions(context.Background(), &conn.ListGoogleAdsConversionActionsPayload{ProjectID: "cncf"})
	_, aerr := svc.ListGoogleAdsAudiences(context.Background(), &conn.ListGoogleAdsAudiencesPayload{ProjectID: "cncf", Type: "audience"})
	if errCode(gerr) != "400" || errCode(lerr) != "400" || errCode(cerr) != "400" || errCode(aerr) != "400" {
		t.Errorf("geo=%v lists=%v conversion actions=%v audiences=%v, want 400 for all four", gerr, lerr, cerr, aerr)
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
		var c *conn.UserListsConflictError
		if !errors.As(err, &c) || c.Code != "409" {
			t.Fatalf("err = %T %v, want 409", err, err)
		}
		if d.calls != 0 {
			t.Error("the dispatcher was reached on a shared-tenancy platform")
		}
	}
}

// Only a request that reached Google is recorded: a cache hit and a local refusal (no or an
// unusable connection) record nothing; a miss, successful or failed upstream, records one.
func TestReadGeoTargetSuggestions_RecordsOnlyCallsThatReachedGoogle(t *testing.T) {
	cases := []struct {
		name string
		d    *mockLookupDispatcher
		want int
	}{
		{"cache hit", &mockLookupDispatcher{geo: []model.GeoTargetSuggestion{}, notCalled: true}, 0},
		{"no connection", &mockLookupDispatcher{err: domain.ErrNotFound, notCalled: true}, 0},
		{"unusable connection", &mockLookupDispatcher{err: fmt.Errorf("%w: x", domain.ErrConnectionNotUsable), notCalled: true}, 0},
		{"miss", &mockLookupDispatcher{geo: []model.GeoTargetSuggestion{}}, 1},
		{"upstream failure", &mockLookupDispatcher{err: errors.New("google 500")}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingMetrics{}
			o := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: tc.d})
			o.SetMetrics(rec)
			_, _ = o.ReadGeoTargetSuggestions(context.Background(), "cncf", model.ProviderGoogleAds, model.GeoTargetQuery{Query: "a"})
			if got := len(rec.upstreamCalls()); got != tc.want {
				t.Errorf("%d upstream calls recorded, want %d", got, tc.want)
			}
		})
	}
}

func TestListGoogleAdsConversionActions_MapsEveryField(t *testing.T) {
	d := &mockLookupDispatcher{actions: &model.ConversionActions{AccountID: "1234567890", Actions: []model.ConversionAction{
		{ID: "111", ResourceName: "customers/1234567890/conversionActions/111", Name: "Event Registrations",
			Status: "ENABLED", Type: "WEBPAGE", Category: "SIGNUP", PrimaryForGoal: true},
		{ID: "222", ResourceName: "customers/1234567890/conversionActions/222", Name: "Newsletter",
			Status: "ENABLED", Type: "UPLOAD_CLICKS", Category: "LEAD"},
	}}}
	res, err := lookupService(d).ListGoogleAdsConversionActions(context.Background(), &conn.ListGoogleAdsConversionActionsPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("ListGoogleAdsConversionActions: %v", err)
	}
	if res.AccountID != "1234567890" || res.ConversionActionCount != 2 || len(res.ConversionActions) != 2 {
		t.Fatalf("result = %+v", res)
	}
	want := []conn.GoogleAdsConversionAction{
		{ID: "111", ResourceName: "customers/1234567890/conversionActions/111", Name: "Event Registrations",
			Status: "ENABLED", Type: "WEBPAGE", Category: "SIGNUP", PrimaryForGoal: true},
		{ID: "222", ResourceName: "customers/1234567890/conversionActions/222", Name: "Newsletter",
			Status: "ENABLED", Type: "UPLOAD_CLICKS", Category: "LEAD"},
	}
	for i := range want {
		if *res.ConversionActions[i] != want[i] {
			t.Errorf("action %d = %+v, want %+v", i, *res.ConversionActions[i], want[i])
		}
	}
}

func TestListGoogleAdsConversionActions_EmptyIsAnEmptyArray(t *testing.T) {
	d := &mockLookupDispatcher{actions: &model.ConversionActions{AccountID: "1234567890", Actions: []model.ConversionAction{}}}
	res, err := lookupService(d).ListGoogleAdsConversionActions(context.Background(), &conn.ListGoogleAdsConversionActionsPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("ListGoogleAdsConversionActions: %v", err)
	}
	b, _ := json.Marshal(res.ConversionActions)
	if string(b) != "[]" || res.ConversionActionCount != 0 {
		t.Errorf("conversion_actions = %s count=%d, want [] and 0", b, res.ConversionActionCount)
	}
}

// Unlike the user-list read, the conversion-action read is NOT refused on a shared-tenancy
// platform (#458): it reaches the dispatcher and answers. The gateway admits only
// global_marketing_ops of the project (charts parity_test.go, #486).
func TestListGoogleAdsConversionActions_SharedTenancyIsNotRefused(t *testing.T) {
	d := &sharedTenancyLookupDispatcher{mockLookupDispatcher{actions: &model.ConversionActions{
		AccountID: "1234567890",
		Actions:   []model.ConversionAction{{ID: "111", ResourceName: "customers/1234567890/conversionActions/111", Status: "ENABLED"}},
	}}}
	res, err := lookupService(d).ListGoogleAdsConversionActions(context.Background(), &conn.ListGoogleAdsConversionActionsPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("err = %T %v, want the shared account's actions", err, err)
	}
	if d.calls != 1 || res.ConversionActionCount != 1 {
		t.Errorf("calls=%d count=%d, want the dispatcher reached once and one action", d.calls, res.ConversionActionCount)
	}
}

func TestReadConversionActions_NilResultIsAnError(t *testing.T) {
	for name, res := range map[string]*model.ConversionActions{"nil result": nil, "nil actions": {AccountID: "1234567890"}} {
		o := &Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: &mockLookupDispatcher{actions: res}}}
		if _, err := o.ReadConversionActions(context.Background(), "cncf", model.ProviderGoogleAds); err == nil {
			t.Errorf("%s: must not be reported as an empty answer", name)
		}
	}
}

// Only a conversion-action read that reached Google is recorded: a local refusal (no or an
// unusable connection) records nothing; a call that went out, successful or not, records one.
func TestReadConversionActions_RecordsOnlyCallsThatReachedGoogle(t *testing.T) {
	empty := &model.ConversionActions{AccountID: "1234567890", Actions: []model.ConversionAction{}}
	cases := []struct {
		name string
		d    *mockLookupDispatcher
		want int
	}{
		{"no connection", &mockLookupDispatcher{err: domain.ErrNotFound, notCalled: true}, 0},
		{"unusable connection", &mockLookupDispatcher{err: fmt.Errorf("%w: x", domain.ErrConnectionNotUsable), notCalled: true}, 0},
		{"success", &mockLookupDispatcher{actions: empty}, 1},
		{"upstream failure", &mockLookupDispatcher{err: errors.New("google 500")}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingMetrics{}
			o := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: tc.d})
			o.SetMetrics(rec)
			_, _ = o.ReadConversionActions(context.Background(), "cncf", model.ProviderGoogleAds)
			if got := len(rec.upstreamCalls()); got != tc.want {
				t.Errorf("%d upstream calls recorded, want %d", got, tc.want)
			}
		})
	}
}

// Every field maps through, the kind is passed down and echoed on the envelope and each row,
// and an empty subtype, status or description is omitted rather than sent as "".
func TestListGoogleAdsAudiences_MapsEveryField(t *testing.T) {
	d := &mockLookupDispatcher{audiences: &model.AudienceResources{AccountID: "8666746580", Kind: "user_interest", Audiences: []model.AudienceResource{
		{ResourceName: "customers/8666746580/userInterests/80276", ID: "80276", Name: "Software", Subtype: "IN_MARKET"},
	}}}
	res, err := lookupService(d).ListGoogleAdsAudiences(context.Background(), &conn.ListGoogleAdsAudiencesPayload{ProjectID: "cncf", Type: "user_interest"})
	if err != nil {
		t.Fatalf("ListGoogleAdsAudiences: %v", err)
	}
	if d.gotKind != "user_interest" {
		t.Errorf("kind passed down = %q, want user_interest", d.gotKind)
	}
	if res.AccountID != "8666746580" || res.Type != "user_interest" || res.AudienceCount != 1 || res.Truncated {
		t.Fatalf("res = %+v", res)
	}
	b, _ := json.Marshal(res.Audiences[0])
	const want = `{"ResourceName":"customers/8666746580/userInterests/80276","ID":"80276","Name":"Software","Type":"user_interest","Subtype":"IN_MARKET","Status":null,"Description":null}`
	if string(b) != want {
		t.Errorf("row = %s\nwant  %s", b, want)
	}

	d = &mockLookupDispatcher{audiences: &model.AudienceResources{AccountID: "8666746580", Kind: "custom_audience", Audiences: []model.AudienceResource{
		{ResourceName: "customers/8666746580/customAudiences/1", ID: "1", Name: "KubeCon", Subtype: "SEARCH", Status: "ENABLED", Description: "d"},
	}}}
	res, err = lookupService(d).ListGoogleAdsAudiences(context.Background(), &conn.ListGoogleAdsAudiencesPayload{ProjectID: "cncf", Type: "custom_audience"})
	if err != nil {
		t.Fatalf("ListGoogleAdsAudiences: %v", err)
	}
	a := res.Audiences[0]
	if a.Status == nil || *a.Status != "ENABLED" || a.Description == nil || *a.Description != "d" || a.Subtype == nil || *a.Subtype != "SEARCH" || a.Type != "custom_audience" {
		t.Errorf("row = %+v, want status, description and subtype mapped", a)
	}
}

// q keeps names containing it, ignoring case and surrounding space, and the 500 cap applies
// after the filter, with truncated reporting what was cut.
func TestListGoogleAdsAudiences_FiltersByNameThenCaps(t *testing.T) {
	rows := make([]model.AudienceResource, 0, googleAdsAudienceMax+3)
	for i := 0; i < googleAdsAudienceMax+1; i++ {
		rows = append(rows, model.AudienceResource{ResourceName: fmt.Sprintf("customers/1/audiences/%d", i+1), ID: fmt.Sprint(i + 1), Name: fmt.Sprintf("CNCF KubeCon %04d", i)})
	}
	rows = append(rows, model.AudienceResource{ResourceName: "customers/1/audiences/9001", ID: "9001", Name: "Open Source Summit"})
	d := &mockLookupDispatcher{audiences: &model.AudienceResources{AccountID: "1", Kind: "audience", Audiences: rows}}

	res, err := lookupService(d).ListGoogleAdsAudiences(context.Background(), &conn.ListGoogleAdsAudiencesPayload{ProjectID: "cncf", Type: "audience", Q: strp("  open SOURCE ")})
	if err != nil {
		t.Fatalf("ListGoogleAdsAudiences: %v", err)
	}
	if res.AudienceCount != 1 || res.Audiences[0].ID != "9001" || res.Truncated {
		t.Errorf("q filter: count=%d truncated=%v first=%+v, want only the Open Source Summit row", res.AudienceCount, res.Truncated, res.Audiences)
	}

	res, err = lookupService(d).ListGoogleAdsAudiences(context.Background(), &conn.ListGoogleAdsAudiencesPayload{ProjectID: "cncf", Type: "audience", Q: strp("kubecon")})
	if err != nil {
		t.Fatalf("ListGoogleAdsAudiences: %v", err)
	}
	if res.AudienceCount != googleAdsAudienceMax || len(res.Audiences) != googleAdsAudienceMax || !res.Truncated {
		t.Errorf("cap: count=%d truncated=%v, want %d and truncated", res.AudienceCount, res.Truncated, googleAdsAudienceMax)
	}

	res, err = lookupService(d).ListGoogleAdsAudiences(context.Background(), &conn.ListGoogleAdsAudiencesPayload{ProjectID: "cncf", Type: "audience", Q: strp("summit")})
	if err != nil || res.AudienceCount != 1 || res.Truncated {
		t.Errorf("a match past the cap position must still be found: err=%v res=%+v", err, res)
	}
}

// An account with no entry of the kind is an empty array, not null.
func TestListGoogleAdsAudiences_EmptyIsAnEmptyArray(t *testing.T) {
	d := &mockLookupDispatcher{audiences: &model.AudienceResources{AccountID: "1", Kind: "combined_audience", Audiences: []model.AudienceResource{}}}
	res, err := lookupService(d).ListGoogleAdsAudiences(context.Background(), &conn.ListGoogleAdsAudiencesPayload{ProjectID: "cncf", Type: "combined_audience"})
	if err != nil {
		t.Fatalf("ListGoogleAdsAudiences: %v", err)
	}
	b, _ := json.Marshal(res.Audiences)
	if string(b) != "[]" || res.AudienceCount != 0 || res.Truncated {
		t.Errorf("audiences = %s count=%d truncated=%v, want [] 0 false", b, res.AudienceCount, res.Truncated)
	}
}

// A bad type or q is a 400 before any dispatcher call.
func TestListGoogleAdsAudiences_RejectsBadInputBeforeAnyUpstreamCall(t *testing.T) {
	cases := map[string]*conn.ListGoogleAdsAudiencesPayload{
		"empty type":       {ProjectID: "cncf", Type: ""},
		"unknown type":     {ProjectID: "cncf", Type: "user_list"},
		"case-folded type": {ProjectID: "cncf", Type: "Custom_Audience"},
		"blank q":          {ProjectID: "cncf", Type: "audience", Q: strp("   ")},
		"empty q":          {ProjectID: "cncf", Type: "audience", Q: strp("")},
		"q over 100 chars": {ProjectID: "cncf", Type: "audience", Q: strp(strings.Repeat("a", 101))},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			d := &mockLookupDispatcher{audiences: &model.AudienceResources{Audiences: []model.AudienceResource{}}}
			_, err := lookupService(d).ListGoogleAdsAudiences(context.Background(), p)
			if errCode(err) != "400" {
				t.Fatalf("err = %T %v, want 400", err, err)
			}
			if d.calls != 0 {
				t.Error("the dispatcher was called for a request refused locally")
			}
		})
	}
	// 100 multi-byte characters are within the limit.
	d := &mockLookupDispatcher{audiences: &model.AudienceResources{Audiences: []model.AudienceResource{}}}
	if _, err := lookupService(d).ListGoogleAdsAudiences(context.Background(), &conn.ListGoogleAdsAudiencesPayload{ProjectID: "cncf", Type: "audience", Q: strp(strings.Repeat("東", 100))}); err != nil || d.calls != 1 {
		t.Errorf("err=%v calls=%d; a 100-character q must reach the dispatcher", err, d.calls)
	}
}

// Like the conversion-action read, the audience read is NOT refused on a shared-tenancy
// platform (#496); the gateway admits only global_marketing_ops (charts parity_test.go).
func TestListGoogleAdsAudiences_SharedTenancyIsNotRefused(t *testing.T) {
	d := &sharedTenancyLookupDispatcher{mockLookupDispatcher{audiences: &model.AudienceResources{
		AccountID: "1", Kind: "audience", Audiences: []model.AudienceResource{{ResourceName: "customers/1/audiences/2", ID: "2", Name: "x"}},
	}}}
	res, err := lookupService(d).ListGoogleAdsAudiences(context.Background(), &conn.ListGoogleAdsAudiencesPayload{ProjectID: "cncf", Type: "audience"})
	if err != nil || d.calls != 1 || res.AudienceCount != 1 {
		t.Errorf("err=%v calls=%d, want the shared account's audiences", err, d.calls)
	}
}

// The platform's more-than-10000-rows refusal is an unclassified upstream error, published as
// 503 in the design description; pinned so a new classifier arm cannot change it silently.
func TestListGoogleAdsAudiences_ScanOverflowIs503(t *testing.T) {
	d := &mockLookupDispatcher{err: errors.New("list user_interest: account 1 returned more than the 10000 rows this read scans")}
	_, err := lookupService(d).ListGoogleAdsAudiences(context.Background(), &conn.ListGoogleAdsAudiencesPayload{ProjectID: "cncf", Type: "user_interest"})
	if errCode(err) != "503" {
		t.Errorf("err = %T %v, want 503", err, err)
	}
}

func TestReadAudiences_NilResultIsAnError(t *testing.T) {
	for name, res := range map[string]*model.AudienceResources{"nil result": nil, "nil audiences": {AccountID: "1"}} {
		o := &Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: &mockLookupDispatcher{audiences: res}}}
		if _, err := o.ReadAudiences(context.Background(), "cncf", model.ProviderGoogleAds, "audience"); err == nil {
			t.Errorf("%s: must not be reported as an empty answer", name)
		}
	}
}

// Only an audience read that reached Google is recorded, as for the conversion-action read.
func TestReadAudiences_RecordsOnlyCallsThatReachedGoogle(t *testing.T) {
	empty := &model.AudienceResources{AccountID: "1234567890", Kind: "audience", Audiences: []model.AudienceResource{}}
	cases := []struct {
		name string
		d    *mockLookupDispatcher
		want int
	}{
		{"no connection", &mockLookupDispatcher{err: domain.ErrNotFound, notCalled: true}, 0},
		{"unusable connection", &mockLookupDispatcher{err: fmt.Errorf("%w: x", domain.ErrConnectionNotUsable), notCalled: true}, 0},
		{"success", &mockLookupDispatcher{audiences: empty}, 1},
		{"upstream failure", &mockLookupDispatcher{err: errors.New("google 500")}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingMetrics{}
			o := NewOrchestrator(&fakeCampaignRepo{}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: tc.d})
			o.SetMetrics(rec)
			_, _ = o.ReadAudiences(context.Background(), "cncf", model.ProviderGoogleAds, "audience")
			if got := len(rec.upstreamCalls()); got != tc.want {
				t.Errorf("%d upstream calls recorded, want %d", got, tc.want)
			}
		})
	}
}
