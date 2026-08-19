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
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// settingsDispatcher wires a GoogleAdsDispatcher against a stub Google Ads API that
// returns searchJSON, recording every request path+method so a test can prove no mutating
// call was issued.
func settingsDispatcher(t *testing.T, searchJSON string) (*GoogleAdsDispatcher, func() []string) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []string
	)
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, searchJSON)
	}))
	t.Cleanup(apiSrv.Close)

	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	return d, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(seen))
		copy(out, seen)
		return out
	}
}

// settingsField finds one field in a readback by name.
func settingsField(t *testing.T, rb *model.CampaignSettingsReadback, name string) model.CampaignSettingsField {
	t.Helper()
	for _, f := range rb.Fields {
		if f.Field == name {
			return f
		}
	}
	t.Fatalf("readback has no field %q; fields present: %+v", name, rb.Fields)
	return model.CampaignSettingsField{}
}

// TestGoogleAds_ReadSettings_ReportsBudgetDivergence is THE binding test.
//
// The campaign row records a budget of 500; the platform reports 750. The response must
// report the DIVERGENCE, and — critically — must carry the UPSTREAM value 750.00.
//
// Asserting only that "a value came back" would pass against an implementation that echoes
// the row and never calls upstream. Asserting the upstream number specifically is what
// makes that implementation fail: 750.00 exists nowhere in the row.
func TestGoogleAds_ReadSettings_ReportsBudgetDivergence(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"upstream name","status":"ENABLED"},"campaignBudget":{"amountMicros":"750000000","period":"DAILY","deliveryMethod":"STANDARD"}}]}`)

	recorded := 500.0
	bt := model.BudgetDaily
	camp := &model.Campaign{
		ID:                 "camp-1",
		Platform:           model.ProviderGoogleAds,
		PlatformCampaignID: "777",
		CampaignName:       "recorded name",
		BudgetAmount:       &recorded,
		BudgetType:         &bt,
	}

	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}

	budget := settingsField(t, rb, settingsFieldBudgetAmount)
	if budget.Comparison != model.SettingsDiverged {
		t.Errorf("budget comparison = %q, want %q (row records 500, platform holds 750)", budget.Comparison, model.SettingsDiverged)
	}
	if budget.Upstream == nil {
		t.Fatal("budget Upstream is nil: the platform's value did not reach the response")
	}
	// The load-bearing assertion: 750.00 comes ONLY from the upstream read.
	if *budget.Upstream != "750.00" {
		t.Errorf("budget Upstream = %q, want %q — the value the PLATFORM reported", *budget.Upstream, "750.00")
	}
	if budget.Recorded == nil || *budget.Recorded != "500.00" {
		t.Errorf("budget Recorded = %v, want 500.00 (what the row asked for)", budget.Recorded)
	}

	// The name diverges too, and from the upstream side again.
	name := settingsField(t, rb, settingsFieldName)
	if name.Comparison != model.SettingsDiverged {
		t.Errorf("name comparison = %q, want diverged", name.Comparison)
	}
	if name.Upstream == nil || *name.Upstream != "upstream name" {
		t.Errorf("name Upstream = %v, want %q", name.Upstream, "upstream name")
	}

	if rb.DivergedCount < 2 {
		t.Errorf("DivergedCount = %d, want at least 2 (budget and name)", rb.DivergedCount)
	}
	// The platform's echoed id, not the requested one.
	if rb.PlatformCampaignID != "777" {
		t.Errorf("PlatformCampaignID = %q, want 777", rb.PlatformCampaignID)
	}
}

// TestGoogleAds_ReadSettings_MatchWhenBothAgree is the counterpart to the divergence test:
// the same code path must report `match` when the two sides genuinely agree, or a
// "diverged" verdict would be meaningless.
func TestGoogleAds_ReadSettings_MatchWhenBothAgree(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"same name","status":"ENABLED"},"campaignBudget":{"amountMicros":"500000000","period":"DAILY"}}]}`)

	recorded := 500.0
	bt := model.BudgetDaily
	camp := &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		CampaignName: "same name", BudgetAmount: &recorded, BudgetType: &bt,
	}

	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	if got := settingsField(t, rb, settingsFieldBudgetAmount); got.Comparison != model.SettingsMatch {
		t.Errorf("budget comparison = %q, want match (both sides are 500)", got.Comparison)
	}
	if got := settingsField(t, rb, settingsFieldBudgetType); got.Comparison != model.SettingsMatch {
		t.Errorf("budget_type comparison = %q, want match (daily vs DAILY)", got.Comparison)
	}
	if got := settingsField(t, rb, settingsFieldName); got.Comparison != model.SettingsMatch {
		t.Errorf("name comparison = %q, want match", got.Comparison)
	}
	if rb.DivergedCount != 0 {
		t.Errorf("DivergedCount = %d, want 0", rb.DivergedCount)
	}
}

// TestGoogleAds_ReadSettings_IssuesNoMutatingCall proves the whole capability cannot spend
// money, asserted over every request the dispatcher made rather than the one expected.
func TestGoogleAds_ReadSettings_IssuesNoMutatingCall(t *testing.T) {
	d, requests := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED"},"campaignBudget":{"amountMicros":"750000000","period":"DAILY"}}]}`)

	recorded := 500.0
	camp := &model.Campaign{ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777", BudgetAmount: &recorded}

	if _, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp); err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	got := requests()
	if len(got) == 0 {
		t.Fatal("no requests recorded; the test cannot prove anything about what was sent")
	}
	for _, r := range got {
		if strings.Contains(r, ":mutate") {
			t.Errorf("settings readback issued a MUTATING call (%s) — this capability must never write upstream. all requests: %v", r, got)
		}
	}
}

// TestGoogleAds_ReadSettings_DoesNotWriteBackOntoTheRow pins design decision 1: the
// readback is READ-ONLY with respect to the campaign row.
//
// The row means "what this dispatch asked for". Writing the observation onto it would
// change the column's meaning from request to observation, break shape-consistency with
// every sibling adapter, and let one transient bad read destroy the only record of the
// request. The row must come back byte-identical in every compared field.
func TestGoogleAds_ReadSettings_DoesNotWriteBackOntoTheRow(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"upstream name","status":"PAUSED"},"campaignBudget":{"amountMicros":"750000000","period":"CUSTOM_PERIOD"}}]}`)

	recorded := 500.0
	bt := model.BudgetDaily
	camp := &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		CampaignName: "recorded name", BudgetAmount: &recorded, BudgetType: &bt,
		Status: model.CampaignStatusCreated,
	}

	if _, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp); err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}

	if *camp.BudgetAmount != 500.0 {
		t.Errorf("BudgetAmount was overwritten to %v; the row must keep recording what was REQUESTED (500)", *camp.BudgetAmount)
	}
	if *camp.BudgetType != model.BudgetDaily {
		t.Errorf("BudgetType was overwritten to %q; the row must keep recording the request", *camp.BudgetType)
	}
	if camp.CampaignName != "recorded name" {
		t.Errorf("CampaignName was overwritten to %q; the row must keep recording the request", camp.CampaignName)
	}
	if camp.Status != model.CampaignStatusCreated {
		t.Errorf("Status was overwritten to %q; the platform's run state must never be written into this service's lifecycle column", camp.Status)
	}
}

// TestGoogleAds_ReadSettings_UnreadUpstreamIsUnknownNotMatch is the absent-vs-equal
// guarantee at the layer that decides verdicts.
//
// The row records a budget; the platform reports none. That is NOT agreement — nothing was
// observed to agree with — and reporting `match` here would be exactly the fabricated
// "they match" this capability exists to make impossible.
func TestGoogleAds_ReadSettings_UnreadUpstreamIsUnknownNotMatch(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED"}}]}`)

	recorded := 500.0
	bt := model.BudgetDaily
	camp := &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		BudgetAmount: &recorded, BudgetType: &bt,
	}

	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	budget := settingsField(t, rb, settingsFieldBudgetAmount)
	if budget.Comparison != model.SettingsUnknown {
		t.Errorf("budget comparison = %q, want %q: an unread upstream side is NOT a match", budget.Comparison, model.SettingsUnknown)
	}
	if budget.Upstream != nil {
		t.Errorf("budget Upstream = %q, want nil (absent, never defaulted)", *budget.Upstream)
	}
	// The recorded side is still reported, so the operator can see what was asked for even
	// though it could not be compared.
	if budget.Recorded == nil || *budget.Recorded != "500.00" {
		t.Errorf("budget Recorded = %v, want 500.00", budget.Recorded)
	}
	if rb.UnknownCount == 0 {
		t.Error("UnknownCount = 0, want > 0: unreadable fields must be counted, not silently dropped")
	}
	if rb.DivergedCount != 0 {
		t.Errorf("DivergedCount = %d, want 0: an unknown must never be counted as a divergence", rb.DivergedCount)
	}
}

// TestGoogleAds_ReadSettings_UnknownPeriodIsNotMappedToABudgetType pins the fail-closed
// enum mapping. Google's UNKNOWN literally means "a value this API version cannot name";
// mapping it to daily or lifetime would manufacture a verdict out of a value Google
// explicitly declined to state.
func TestGoogleAds_ReadSettings_UnknownPeriodIsNotMappedToABudgetType(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED"},"campaignBudget":{"amountMicros":"500000000","period":"UNKNOWN"}}]}`)

	recorded := 500.0
	bt := model.BudgetDaily
	camp := &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		BudgetAmount: &recorded, BudgetType: &bt,
	}

	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	got := settingsField(t, rb, settingsFieldBudgetType)
	if got.Comparison != model.SettingsUnknown {
		t.Errorf("budget_type comparison = %q, want unknown for an UNKNOWN period", got.Comparison)
	}
	if got.Upstream != nil {
		t.Errorf("budget_type Upstream = %q, want nil: UNKNOWN must not be mapped to a budget type", *got.Upstream)
	}
}

// TestGoogleAds_ReadSettings_CustomPeriodMapsToLifetime pins the one enum mapping that is
// easy to get wrong: Google's v23 BudgetPeriodEnum has NO `LIFETIME` value — CUSTOM_PERIOD
// is what corresponds to this service's `lifetime`.
func TestGoogleAds_ReadSettings_CustomPeriodMapsToLifetime(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED"},"campaignBudget":{"totalAmountMicros":"9000000000","period":"CUSTOM_PERIOD"}}]}`)

	bt := model.BudgetLifetime
	recorded := 9000.0
	camp := &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		BudgetAmount: &recorded, BudgetType: &bt,
	}

	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	btField := settingsField(t, rb, settingsFieldBudgetType)
	if btField.Upstream == nil || *btField.Upstream != string(model.BudgetLifetime) {
		t.Errorf("budget_type Upstream = %v, want %q (CUSTOM_PERIOD is Google's spelling of lifetime)", btField.Upstream, model.BudgetLifetime)
	}
	if btField.Comparison != model.SettingsMatch {
		t.Errorf("budget_type comparison = %q, want match", btField.Comparison)
	}
	// A CUSTOM_PERIOD budget's amount lives in total_amount_micros; reading only
	// amount_micros would report this campaign as having no budget.
	amt := settingsField(t, rb, settingsFieldBudgetAmount)
	if amt.Upstream == nil || *amt.Upstream != "9000.00" {
		t.Errorf("budget_amount Upstream = %v, want 9000.00 from total_amount_micros", amt.Upstream)
	}
}

// TestGoogleAds_ReadSettings_StatusIsReportedButNotCompared pins the deliberate
// non-comparison: the row's Status is this service's lifecycle vocabulary and Google's is
// ENABLED/PAUSED/REMOVED — different axes. Comparing them would report a permanent,
// meaningless divergence on every campaign ever created, while still reporting the
// upstream value lets an operator SEE that a campaign is paused upstream.
func TestGoogleAds_ReadSettings_StatusIsReportedButNotCompared(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"PAUSED"},"campaignBudget":{"amountMicros":"500000000","period":"DAILY"}}]}`)

	camp := &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		Status: model.CampaignStatusCreated,
	}
	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	st := settingsField(t, rb, settingsFieldStatus)
	if st.Upstream == nil || *st.Upstream != "PAUSED" {
		t.Errorf("status Upstream = %v, want PAUSED — the operator must be able to SEE the platform's run state", st.Upstream)
	}
	if st.Recorded != nil {
		t.Errorf("status Recorded = %q, want nil: this service's lifecycle status is a different axis and must not be presented as the same field", *st.Recorded)
	}
	if st.Comparison != model.SettingsUnknown {
		t.Errorf("status comparison = %q, want unknown — the two vocabularies are not comparable", st.Comparison)
	}
}

// TestGoogleAds_ReadSettings_AbsentUpstreamCampaignIsReportedAsAbsent: the platform
// answered and holds no such campaign. That must be its own signal, not an all-unknown
// readback, which would say "we could not read these" when the truth is far more specific
// and far more urgent.
func TestGoogleAds_ReadSettings_AbsentUpstreamCampaignIsReportedAsAbsent(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[]}`)

	camp := &model.Campaign{ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777"}
	_, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if !errors.Is(err, domain.ErrPlatformCampaignAbsent) {
		t.Fatalf("err = %v, want ErrPlatformCampaignAbsent", err)
	}
}

// TestGoogleAds_ReadSettings_AccountMismatchIsRefusedBeforeContact pins the identity
// invariant. The stored PlatformCampaignID is unique only within the customer it was
// created under, so reading it under a re-pointed connection could return ANOTHER
// campaign's configuration — and this endpoint would then report a divergence between
// this campaign's recorded budget and a different campaign's actual one.
func TestGoogleAds_ReadSettings_AccountMismatchIsRefusedBeforeContact(t *testing.T) {
	d, requests := settingsDispatcher(t, `{"results":[]}`)

	camp := &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		Result: []byte(`{"customerId":"9999999999"}`),
	}
	_, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Fatalf("err = %v, want ErrCampaignAccountMismatch", err)
	}
	for _, r := range requests() {
		if strings.Contains(r, "googleAds:search") {
			t.Errorf("a mismatched account must be refused BEFORE the campaign is queried, but a search was issued: %v", requests())
		}
	}
}

// TestGoogleAds_ReadSettings_ConnectionUnresolvedPropagates: a broken connection surfaces
// as a plain error, not wrapped with notCreated — a read has no create-claim to protect.
func TestGoogleAds_ReadSettings_ConnectionUnresolvedPropagates(t *testing.T) {
	d := NewGoogleAdsDispatcher(fakeConnReader{err: errors.New("no connection")}, identityEncryptor{})
	camp := &model.Campaign{ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777"}
	if _, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp); err == nil {
		t.Fatal("expected an error when the connection cannot be resolved")
	}
}

// TestGoogleAds_ReadSettings_ReportsUpstreamOnlyObservations pins that settings with NO
// counterpart on the campaign row are still REPORTED.
//
// Nothing in a Google Ads dispatch config can express delivery method, budget sharing,
// channel type or bidding strategy, so they can never diverge — but a budget that reads as
// expected while being ACCELERATED or shared across campaigns is exactly the state that
// explains a spend anomaly the compared fields cannot. Reading them into the struct and
// then dropping them would be dead API surface.
func TestGoogleAds_ReadSettings_ReportsUpstreamOnlyObservations(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED","advertisingChannelType":"DEMAND_GEN","biddingStrategyType":"MANUAL_CPC"},"campaignBudget":{"amountMicros":"500000000","period":"DAILY","deliveryMethod":"ACCELERATED","explicitlyShared":true}}]}`)

	camp := &model.Campaign{ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777"}
	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	for _, tc := range []struct{ field, want string }{
		{settingsFieldBudgetDelivery, "ACCELERATED"},
		{settingsFieldBudgetShared, "true"},
		{settingsFieldChannelType, "DEMAND_GEN"},
		{settingsFieldBiddingStrategy, "MANUAL_CPC"},
	} {
		got := settingsField(t, rb, tc.field)
		if got.Upstream == nil || *got.Upstream != tc.want {
			t.Errorf("%s Upstream = %v, want %q", tc.field, got.Upstream, tc.want)
		}
		if got.Recorded != nil {
			t.Errorf("%s Recorded = %q, want nil: no row column expresses this", tc.field, *got.Recorded)
		}
		if got.Comparison != model.SettingsUnknown {
			t.Errorf("%s comparison = %q, want unknown: an upstream-only observation was never compared", tc.field, got.Comparison)
		}
	}
}

// TestGoogleAds_ReadSettings_FlightDatesCompareAsDatesNotTimestamps pins the date
// normalisation. Google returns 'yyyy-MM-dd HH:mm:ss' in the ad account's timezone while
// the row stores a DATE; comparing the raw strings would report a divergence for every
// campaign whose dates actually agree.
func TestGoogleAds_ReadSettings_FlightDatesCompareAsDatesNotTimestamps(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED","startDateTime":"2026-08-01 00:00:00","endDateTime":"2026-08-31 23:59:59"}}]}`)

	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	camp := &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		StartDate: &start, EndDate: &end,
	}
	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	sf := settingsField(t, rb, settingsFieldStartDate)
	if sf.Upstream == nil || *sf.Upstream != "2026-08-01" {
		t.Errorf("start_date Upstream = %v, want 2026-08-01 (the time component must be dropped)", sf.Upstream)
	}
	if sf.Comparison != model.SettingsMatch {
		t.Errorf("start_date comparison = %q, want match: the same date must not read as a divergence", sf.Comparison)
	}
	ef := settingsField(t, rb, settingsFieldEndDate)
	if ef.Upstream == nil || *ef.Upstream != "2026-08-31" {
		t.Errorf("end_date Upstream = %v, want 2026-08-31", ef.Upstream)
	}
	if ef.Comparison != model.SettingsMatch {
		t.Errorf("end_date comparison = %q, want match", ef.Comparison)
	}
}

// TestGoogleAds_ReadSettings_AbsentFlightDateIsUnknownNotFalselyDiverged: Google Ads
// campaigns carry no dates in this service's config, so the recorded side is normally NULL.
// That must read as `unknown`, not as a divergence against the platform's real dates —
// which would flag every Google campaign in the system.
func TestGoogleAds_ReadSettings_AbsentFlightDateIsUnknownNotFalselyDiverged(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED","startDateTime":"2026-08-01 00:00:00"}}]}`)

	camp := &model.Campaign{ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777"}
	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	sf := settingsField(t, rb, settingsFieldStartDate)
	if sf.Comparison != model.SettingsUnknown {
		t.Errorf("start_date comparison = %q, want unknown when the row records no date", sf.Comparison)
	}
	if rb.DivergedCount != 0 {
		t.Errorf("DivergedCount = %d, want 0: an unrecorded date must never be reported as a divergence", rb.DivergedCount)
	}
	// The upstream date is still shown, so the operator learns what the platform holds.
	if sf.Upstream == nil || *sf.Upstream != "2026-08-01" {
		t.Errorf("start_date Upstream = %v, want 2026-08-01", sf.Upstream)
	}
}

// TestGoogleAds_ReadSettings_AbsentSharedFlagIsNotFalse pins that an absent bool stays
// absent. Rendering nil as "false" would be a claim the platform never made — and
// "not shared" is the reassuring answer, so defaulting to it hides the anomaly.
func TestGoogleAds_ReadSettings_AbsentSharedFlagIsNotFalse(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED"},"campaignBudget":{"amountMicros":"500000000","period":"DAILY"}}]}`)

	camp := &model.Campaign{ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777"}
	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	got := settingsField(t, rb, settingsFieldBudgetShared)
	if got.Upstream != nil {
		t.Errorf("budget_explicitly_shared Upstream = %q, want nil: an unreported bool must not become \"false\"", *got.Upstream)
	}
}

// TestGoogleAdsDateOnly covers the helper's branches directly, including the passthrough
// that the readback tests cannot reach through a well-formed response.
//
// The passthrough is the interesting one: a value in an unexpected shape is returned
// UNCHANGED rather than discarded, because it is still what the platform said, and showing
// it beside a differing recorded date tells an operator more than an unexplained absence.
// The cost is that such a value compares raw against the row's YYYY-MM-DD and so reads as a
// divergence — which is the honest outcome, since the two genuinely do not agree in any
// form this code can establish.
func TestGoogleAdsDateOnly(t *testing.T) {
	dt := "2026-08-01 00:00:00"
	dateOnly := "2026-08-01"
	weird := "not-a-date"
	for _, tc := range []struct {
		name string
		in   *string
		want *string
	}{
		{"nil stays nil", nil, nil},
		{"timestamp is reduced to its date", &dt, &dateOnly},
		{"a value with no time is unchanged", &dateOnly, &dateOnly},
		{"an unexpected shape passes through rather than vanishing", &weird, &weird},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := googleAdsDateOnly(tc.in)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("got %q, want nil", *got)
			case tc.want == nil:
				return
			case got == nil:
				t.Fatalf("got nil, want %q", *tc.want)
			case *got != *tc.want:
				t.Fatalf("got %q, want %q", *got, *tc.want)
			}
		})
	}
}

// TestBoolToStrPtr pins that absence survives. Rendering a nil as "false" would be a claim
// the platform never made — and since "not shared" is the reassuring answer, defaulting to
// it would hide exactly the anomaly the field exists to surface.
func TestBoolToStrPtr(t *testing.T) {
	if got := boolToStrPtr(nil); got != nil {
		t.Errorf("nil rendered as %q, want nil", *got)
	}
	tr, fa := true, false
	if got := boolToStrPtr(&tr); got == nil || *got != "true" {
		t.Errorf("true rendered as %v, want \"true\"", got)
	}
	if got := boolToStrPtr(&fa); got == nil || *got != "false" {
		t.Errorf("false rendered as %v, want \"false\"", got)
	}
}
