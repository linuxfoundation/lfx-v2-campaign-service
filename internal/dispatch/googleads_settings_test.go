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

// settingsProvenance is the Result blob recording that the campaign was created under the
// customer activeGoogleAdsConn resolves to. ReadSettings fails CLOSED on absent provenance
// (a row naming no creating customer cannot be read safely under the project's current one),
// so every fixture exercising a DIFFERENT concern has to record it — otherwise the test would
// be asserting its subject through a refusal that never reaches the code under test.
func settingsProvenance() []byte { return []byte(`{"customerId":"1234567890"}`) }

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

// TestGoogleAds_ReadSettings_SubCentBudgetIsNotRoundedIntoAMatch pins that the FORMATTER
// cannot manufacture an agreement.
//
// The row's column is NUMERIC(14,2). An upstream budget of 10.004 (10_004_000 micros) rendered
// at two decimal places becomes "10.00" — identical to a recorded 10.00 — and the field would
// report `match` for two budgets that genuinely differ. That is the same fabricated agreement
// the nil-handling is built to prevent, arriving through rounding instead of through an
// absence, and it is reachable precisely on ADOPTED campaigns, which are what a readback is
// for: this service's own create path rounds to micros from a 2dp amount and can never
// produce one.
func TestGoogleAds_ReadSettings_SubCentBudgetIsNotRoundedIntoAMatch(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED"},"campaignBudget":{"amountMicros":"10004000","period":"DAILY"}}]}`)

	recorded := 10.00
	camp := &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		Result:       settingsProvenance(),
		BudgetAmount: &recorded,
	}

	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	budget := settingsField(t, rb, settingsFieldBudgetAmount)
	if budget.Comparison == model.SettingsMatch {
		t.Errorf("budget reported %q for recorded=10.00 vs upstream=10.004: rounding the upstream "+
			"side to the row's 2dp fabricated an agreement between two budgets that differ", budget.Comparison)
	}
	if budget.Comparison != model.SettingsDiverged {
		t.Errorf("budget comparison = %q, want %q", budget.Comparison, model.SettingsDiverged)
	}
	if budget.Upstream == nil || *budget.Upstream != "10.004" {
		t.Errorf("upstream = %v, want the full-precision 10.004: an operator cannot act on a "+
			"divergence whose reported value has had the differing digits removed", budget.Upstream)
	}
}

// TestGoogleAds_ReadSettings_WholeCentBudgetStillMatches is the other half: the sub-cent
// exception must not make every ordinary budget read as a divergence.
func TestGoogleAds_ReadSettings_WholeCentBudgetStillMatches(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED"},"campaignBudget":{"amountMicros":"500500000","period":"DAILY"}}]}`)

	recorded := 500.50
	camp := &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		Result:       settingsProvenance(),
		BudgetAmount: &recorded,
	}

	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	budget := settingsField(t, rb, settingsFieldBudgetAmount)
	if budget.Comparison != model.SettingsMatch {
		t.Errorf("budget comparison = %q, want %q: 500.50 and 500500000 micros are the SAME budget, "+
			"and reporting a divergence here would make every readback noise", budget.Comparison, model.SettingsMatch)
	}
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
		Result:             settingsProvenance(),
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
		Result:       settingsProvenance(),
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

// TestGoogleAds_ReadSettings_PaddedRecordedNameDoesNotFabricateAMatch pins the sibling of the
// budget-period defect: a normalisation applied to only ONE side of a comparison manufactures
// agreement the data does not support.
//
// The recorded name used to be TrimSpace'd on its way into the comparison while the upstream
// name was carried verbatim, so a row holding `" same name "` compared EQUAL to an upstream
// `"same name"` and was reported as a `match`. The two plainly differ, and Google is showing
// the operator a name this service did not record — which is exactly the finding this readback
// exists to surface.
//
// The padded row is reachable rather than hypothetical: UpdateCampaign assigns
// `p.Campaign.CampaignName` verbatim (internal/service/brief.go), and there is no whitespace
// validation in the service layer, in design/, or on the column.
//
// Every case here uses a PADDED recorded value. A test built from unpadded names passes
// against the bug, because trimming a string with no surrounding whitespace is the identity —
// which is why the existing match/divergence tests above could not have caught this.
func TestGoogleAds_ReadSettings_PaddedRecordedNameDoesNotFabricateAMatch(t *testing.T) {
	for name, recordedName := range map[string]string{
		"leading space":      " same name",
		"trailing space":     "same name ",
		"both ends":          "  same name  ",
		"trailing newline":   "same name\n",
		"leading tab":        "\tsame name",
		"internal is fine":   " same name\t",
		"non-breaking space": "same name\u00a0",
	} {
		t.Run(name, func(t *testing.T) {
			d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"same name","status":"ENABLED"}}]}`)

			camp := &model.Campaign{
				ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
				Result:       settingsProvenance(),
				CampaignName: recordedName,
			}
			rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
			if err != nil {
				t.Fatalf("ReadSettings: %v", err)
			}
			got := settingsField(t, rb, settingsFieldName)
			if got.Comparison == model.SettingsMatch {
				t.Errorf("comparison = match for recorded %q against upstream %q — trimming one "+
					"side of the comparison fabricates an agreement the two values do not have, "+
					"and hides that Google holds a name this service did not record",
					recordedName, "same name")
			}
			if got.Comparison != model.SettingsDiverged {
				t.Errorf("comparison = %q, want diverged: both sides were read and they differ", got.Comparison)
			}
			// The reported recorded value must be the row's OWN bytes. Reporting a trimmed
			// value beside an "diverged" verdict shows an operator two strings that look
			// identical, with no way to see why they were called different.
			if got.Recorded == nil {
				t.Fatal("Recorded = nil; the row holds a name")
			}
			if *got.Recorded != recordedName {
				t.Errorf("Recorded = %q, want the row's verbatim %q — a normalised value in the "+
					"report leaves the divergence unexplainable", *got.Recorded, recordedName)
			}
		})
	}
}

// TestGoogleAds_ReadSettings_BlankRecordedNameIsUnknownNotDiverged is the narrowing half.
// TrimSpace still has a job here: DETECTING an all-blank legacy value, which means the row
// never captured a name. Reporting that as a divergence against a real upstream name would
// invent a finding out of an absence — and a fix that simply deleted the TrimSpace would do
// exactly that, while satisfying every assertion in the test above.
func TestGoogleAds_ReadSettings_BlankRecordedNameIsUnknownNotDiverged(t *testing.T) {
	for name, recordedName := range map[string]string{
		"empty":    "",
		"spaces":   "   ",
		"tab":      "\t",
		"newline":  "\n",
		"mixed ws": " \t\n ",
	} {
		t.Run(name, func(t *testing.T) {
			d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"upstream name","status":"ENABLED"}}]}`)

			camp := &model.Campaign{
				ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
				Result:       settingsProvenance(),
				CampaignName: recordedName,
			}
			rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
			if err != nil {
				t.Fatalf("ReadSettings: %v", err)
			}
			got := settingsField(t, rb, settingsFieldName)
			if got.Recorded != nil {
				t.Errorf("Recorded = %q, want nil: an all-blank column never captured a name, and "+
					"comparing it would report a divergence for a row that recorded nothing", *got.Recorded)
			}
			if got.Comparison != model.SettingsUnknown {
				t.Errorf("comparison = %q, want unknown", got.Comparison)
			}
		})
	}
}

// TestGoogleAds_ReadSettings_IssuesNoMutatingCall proves the whole capability cannot spend
// money, asserted over every request the dispatcher made rather than the one expected.
func TestGoogleAds_ReadSettings_IssuesNoMutatingCall(t *testing.T) {
	d, requests := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED"},"campaignBudget":{"amountMicros":"750000000","period":"DAILY"}}]}`)

	recorded := 500.0
	camp := &model.Campaign{ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		Result: settingsProvenance(), BudgetAmount: &recorded}

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
//
// The fixture pairs amount_micros with DAILY because those two are the CONSISTENT pairing.
// It previously paired amount_micros with CUSTOM_PERIOD, which the client now refuses as a
// self-contradictory row: the amount field is selected by the period upstream, and reading
// an inconsistent pair compares a daily amount against a whole-flight cap and reports a
// budget divergence that is really a field-selection bug. The test asserted the old
// permissive decode by accident — its subject is the write-back property, not budget
// consistency — and a DAILY period exercises that subject just as well, since the upstream
// 750 still diverges from the recorded 500.
func TestGoogleAds_ReadSettings_DoesNotWriteBackOntoTheRow(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"upstream name","status":"PAUSED"},"campaignBudget":{"amountMicros":"750000000","period":"DAILY"}}]}`)

	recorded := 500.0
	bt := model.BudgetDaily
	camp := &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		Result:       settingsProvenance(),
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
		Result:       settingsProvenance(),
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
		Result:       settingsProvenance(),
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

// TestGoogleAds_ReadSettings_PaddedPeriodIsNotMappedToABudgetType is the other half of the
// blankToNil discipline, exercised through the real decode path rather than by calling the
// helper with a literal.
//
// blankToNil deliberately stopped normalising these strings so a malformed field arrives at
// its consumer malformed — its doc-comment names " DAILY " as the exact case. That is only
// worth anything if the consumer then declines to name it: a trim here would reconstruct the
// well-formed DAILY Google never sent and report `match` against a recorded daily budget,
// which is agreement manufactured by normalisation rather than observed on the platform.
//
// The recorded side is deliberately `daily` — the value a trim would make this padded field
// compare EQUAL to. A test whose recorded side differed would read `divergent` either way and
// so would pass against the bug.
func TestGoogleAds_ReadSettings_PaddedPeriodIsNotMappedToABudgetType(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED"},"campaignBudget":{"amountMicros":"500000000","period":" DAILY "}}]}`)

	recorded := 500.0
	bt := model.BudgetDaily
	camp := &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		Result:       settingsProvenance(),
		BudgetAmount: &recorded, BudgetType: &bt,
	}

	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	got := settingsField(t, rb, settingsFieldBudgetType)
	if got.Upstream != nil {
		t.Errorf("budget_type Upstream = %q, want nil: %q is not a spelling this API version "+
			"names, and trimming it into DAILY fabricates a value the platform never sent",
			*got.Upstream, " DAILY ")
	}
	if got.Comparison != model.SettingsUnknown {
		t.Errorf("budget_type comparison = %q, want unknown for a padded period; %q must not "+
			"be normalised into a match against the recorded daily budget",
			got.Comparison, " DAILY ")
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
		Result:       settingsProvenance(),
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
		Result: settingsProvenance(),
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

	camp := &model.Campaign{ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777", Result: settingsProvenance()}
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
	camp := &model.Campaign{ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777", Result: settingsProvenance()}
	if _, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp); err == nil {
		t.Fatal("expected an error when the connection cannot be resolved")
	}
}

// TestGoogleAds_ReadSettings_ReportsUpstreamOnlyObservations pins that settings with NO
// counterpart on the campaign row are still REPORTED.
//
// Nothing in a Google Ads dispatch config can express delivery method, budget sharing or
// bidding strategy, so those three can never diverge — but a budget that reads as expected
// while being ACCELERATED or shared across campaigns is exactly the state that explains a
// spend anomaly the compared fields cannot. Reading them into the struct and then dropping
// them would be dead API surface.
//
// advertising_channel_type is NOT in this set: googleAdsConfig.Channel is recorded in
// ConfigSnapshot, so it has a recorded side and IS compared. It appears below only with a
// campaign carrying no snapshot at all, where `unknown` is the honest verdict for a
// different reason — nothing was recorded on that row. See the three channel-type tests
// following this one.
func TestGoogleAds_ReadSettings_ReportsUpstreamOnlyObservations(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED","advertisingChannelType":"DEMAND_GEN","biddingStrategyType":"MANUAL_CPC"},"campaignBudget":{"amountMicros":"500000000","period":"DAILY","deliveryMethod":"ACCELERATED","explicitlyShared":true}}]}`)

	camp := &model.Campaign{ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777", Result: settingsProvenance()}
	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	for _, tc := range []struct{ field, want string }{
		{settingsFieldBudgetDelivery, "ACCELERATED"},
		{settingsFieldBudgetShared, "true"},
		{settingsFieldBiddingStrategy, "MANUAL_CPC"},
	} {
		got := settingsField(t, rb, tc.field)
		if got.Upstream == nil || *got.Upstream != tc.want {
			t.Errorf("%s Upstream = %v, want %q", tc.field, got.Upstream, tc.want)
		}
		if got.Recorded != nil {
			t.Errorf("%s Recorded = %q, want nil: nothing in a dispatch config expresses this", tc.field, *got.Recorded)
		}
		if got.Comparison != model.SettingsUnknown {
			t.Errorf("%s comparison = %q, want unknown: an upstream-only observation was never compared", tc.field, got.Comparison)
		}
	}
}

// TestGoogleAds_ReadSettings_RecordedChannelTypeDiverges is the point of recording the
// channel at all: a campaign this service dispatched as demand-gen, running upstream as a
// SEARCH campaign, is a real misconfiguration and must be REPORTED as a divergence. Passing
// nil for the recorded side made this permanently `unknown` — the finding could not exist.
func TestGoogleAds_ReadSettings_RecordedChannelTypeDiverges(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED","advertisingChannelType":"SEARCH"}}]}`)

	camp := &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		Result:         settingsProvenance(),
		ConfigSnapshot: []byte(`{"channel":"demand-gen"}`),
	}
	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	got := settingsField(t, rb, settingsFieldChannelType)
	if got.Recorded == nil {
		t.Fatal("channel type Recorded = nil; googleAdsConfig.Channel IS persisted in " +
			"ConfigSnapshot on both the create and the adoption path, so discarding it " +
			"makes an upstream/recorded channel mismatch unreportable")
	}
	if *got.Recorded != "DEMAND_GEN" {
		t.Errorf("Recorded = %q, want DEMAND_GEN (Google's own spelling, the vocabulary this field compares in)", *got.Recorded)
	}
	if got.Comparison != model.SettingsDiverged {
		t.Errorf("comparison = %q, want diverged: recorded demand-gen against an upstream SEARCH", got.Comparison)
	}
}

// TestGoogleAds_ReadSettings_RecordedChannelTypeMatches: the same wiring must also be able
// to report agreement, and an EMPTY channel in a snapshot this adapter wrote means SEARCH —
// the value every caller predating the field meant, and the one the dispatcher hardcoded
// before it existed.
//
// The "absent channel" case previously used `{"budget":50}` — a JSON object with no
// `"channel"` key at all — and asserted it recorded SEARCH. That agreed with the bug rather
// than with the intent. `applyCampaignConfig` marshals the googleAdsConfig STRUCT whole and
// no field carries `omitempty`, so a snapshot this adapter wrote ALWAYS carries the key;
// `{"budget":50}` is therefore not a legacy row, it is a config blob something else supplied.
// The legacy population is represented correctly by `{"budget":50,"channel":""}`, which is
// what such a row genuinely looks like on disk, and that is what this case now asserts.
func TestGoogleAds_ReadSettings_RecordedChannelTypeMatches(t *testing.T) {
	for name, snapshot := range map[string]string{
		"explicit search": `{"channel":"search"}`,
		// What a caller predating the field ACTUALLY leaves on disk: the key present,
		// holding the struct's zero value. This is the row the SEARCH default is for.
		"empty channel written by applyCampaignConfig": `{"budget":50,"channel":""}`,
		// The full shape applyCampaignConfig emits, to pin that the presence test is
		// satisfied by a real snapshot and not only by a hand-trimmed one.
		"full marshalled config with empty channel": `{"budget":50,"channel":"","headlines":null,` +
			`"descriptions":null,"keywords":null,"audienceSegments":null,"geoTargets":null,` +
			`"adoptExisting":false}`,
		"padded channel value": `{"channel":"  SEARCH  "}`,
	} {
		t.Run(name, func(t *testing.T) {
			d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED","advertisingChannelType":"SEARCH"}}]}`)

			camp := &model.Campaign{
				ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
				Result:         settingsProvenance(),
				ConfigSnapshot: []byte(snapshot),
			}
			rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
			if err != nil {
				t.Fatalf("ReadSettings: %v", err)
			}
			got := settingsField(t, rb, settingsFieldChannelType)
			if got.Recorded == nil || *got.Recorded != "SEARCH" {
				t.Fatalf("Recorded = %v, want SEARCH", got.Recorded)
			}
			if got.Comparison != model.SettingsMatch {
				t.Errorf("comparison = %q, want match", got.Comparison)
			}
		})
	}
}

// TestGoogleAds_ReadSettings_UninterpretableChannelIsUnknown: nil is still the answer where
// nothing was genuinely recorded, or where what was recorded cannot be interpreted. Claiming
// `diverged` from a snapshot this adapter cannot read would be a fabricated finding — the
// same absent-is-not-a-value discipline the rest of this readback applies.
//
// The table's second half pins the ABSENCE cases, which a `Channel string` decode cannot
// separate: an omitted key, an explicit null and a wrong-typed value all produce "", and all
// three were being reported as a recorded SEARCH. A default is only sound over the population
// it was reasoned about — callers predating the field, whose rows applyCampaignConfig wrote
// WITH the key — and `UpdateCampaign` accepts arbitrary config JSON, so nothing else may
// borrow it.
func TestGoogleAds_ReadSettings_UninterpretableChannelIsUnknown(t *testing.T) {
	for name, snapshot := range map[string]string{
		"no snapshot at all":                 ``,
		"not a JSON object":                  `"search"`,
		"channel this service cannot create": `{"channel":"performance-max"}`,
		// The three cases the string decode could not tell apart. All three used to
		// decode to "" and be reported as a recorded SEARCH — a channel nobody wrote.
		// UpdateCampaign persists arbitrary caller-supplied config JSON, so each is
		// reachable from an untrusted request, not only from this service's own writes.
		"object with no channel key":  `{"budget":50}`,
		"empty config object":         `{}`,
		"explicit null channel":       `{"budget":50,"channel":null}`,
		"channel is a number":         `{"budget":50,"channel":7}`,
		"channel is an object":        `{"budget":50,"channel":{"name":"search"}}`,
		"channel is an array":         `{"budget":50,"channel":["search"]}`,
		"channel is a bool":           `{"budget":50,"channel":true}`,
		"a foreign platform's config": `{"objective":"OUTCOME_TRAFFIC","pageId":"111"}`,
	} {
		t.Run(name, func(t *testing.T) {
			d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED","advertisingChannelType":"SEARCH"}}]}`)

			camp := &model.Campaign{
				ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
				Result:         settingsProvenance(),
				ConfigSnapshot: []byte(snapshot),
			}
			rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
			if err != nil {
				t.Fatalf("ReadSettings: %v", err)
			}
			got := settingsField(t, rb, settingsFieldChannelType)
			if got.Recorded != nil {
				t.Errorf("Recorded = %q, want nil: nothing interpretable was recorded", *got.Recorded)
			}
			if got.Comparison != model.SettingsUnknown {
				t.Errorf("comparison = %q, want unknown", got.Comparison)
			}
		})
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
		Result:    settingsProvenance(),
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

	camp := &model.Campaign{ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777", Result: settingsProvenance()}
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

	camp := &model.Campaign{ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777", Result: settingsProvenance()}
	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	got := settingsField(t, rb, settingsFieldBudgetShared)
	if got.Upstream != nil {
		t.Errorf("budget_explicitly_shared Upstream = %q, want nil: an unreported bool must not become \"false\"", *got.Upstream)
	}
}

// TestGoogleAds_ReadSettings_DateOnlyUpstreamIsNotReportedAsAMatch is the readback-level
// proof for the defect the old passthrough carried, and it is deliberately at THIS layer
// rather than on the helper: the fabricated `match` was only ever visible in the verdict.
//
// Google documents start_date_time/end_date_time as 'yyyy-MM-dd HH:mm:ss'. A response that
// carries a bare "2026-08-01" — the required time component missing — fails the strict
// parse. While that failure returned the raw string, the value handed to the comparison was
// byte-identical to the recorded side, which this readback formats to exactly YYYY-MM-DD,
// and the field reported `match` for a date the code could not validate at all. The other
// malformed shapes ("2026-08-01 garbage") could not do this because they cannot collide
// with the recorded spelling; the date-only shape is the one that can, and it is also the
// most plausible thing a real API sends.
func TestGoogleAds_ReadSettings_DateOnlyUpstreamIsNotReportedAsAMatch(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED","startDateTime":"2026-08-01","endDateTime":"2026-08-31"}}]}`)

	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	camp := &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		Result:    settingsProvenance(),
		StartDate: &start, EndDate: &end,
	}
	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	for _, field := range []string{settingsFieldStartDate, settingsFieldEndDate} {
		f := settingsField(t, rb, field)
		if f.Comparison == model.SettingsMatch {
			t.Errorf("%s comparison = match for an upstream value that never parsed; a date "+
				"missing its documented time component must not be reportable as agreement", field)
		}
		if f.Comparison != model.SettingsUnknown {
			t.Errorf("%s comparison = %q, want unknown: an unparseable upstream value is not comparable", field, f.Comparison)
		}
		if f.Upstream != nil {
			t.Errorf("%s Upstream = %q, want nil: a value that failed the strict parse must be "+
				"withheld, not carried into the comparison", field, *f.Upstream)
		}
	}
}

// TestGoogleAdsDateOnly covers the helper's branches directly, including the failure path
// that the readback tests reach only through a malformed response.
//
// The failure path is the interesting one: a value in an unexpected shape yields ABSENCE
// rather than the raw string, because the raw string is not inert. It is compared, and the
// recorded side it is compared against is rendered in exactly the YYYY-MM-DD spelling that
// the most likely malformed value — a date with no time component — already has. Returning
// it therefore manufactured a `match`. Withholding it reports `unknown`, which is what a
// value this code could not validate actually warrants.
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
		// THE case. A bare date fails the strict parse, and while the helper returned the
		// raw string on failure this came back as "2026-08-01" — byte-equal to the recorded
		// side and therefore reported as `match` for a value that never parsed. It must be
		// withheld, and it is the one failing value whose raw form can collide with the
		// recorded spelling at all.
		{"a value with no time component is WITHHELD, not returned verbatim", &dateOnly, nil},
		{"an unexpected shape yields absence rather than a comparable string", &weird, nil},
		// The rest are the fabricated-agreement cases. Splitting on the first space would
		// reduce each of these to "2026-08-01" and let it compare EQUAL to a recorded
		// 2026-08-01 — manufacturing an agreement out of a response this code cannot parse.
		// Failing the parse withholds them instead, which cannot compare equal to anything.
		{"garbage after the date is NOT truncated into a match", strPtr("2026-08-01 garbage"), nil},
		{"a malformed time is NOT truncated into a match", strPtr("2026-08-01 25:99:99"), nil},
		{"a trailing space is NOT truncated into a match", strPtr("2026-08-01 "), nil},
		{"a second space-separated field is NOT truncated into a match", strPtr("2026-08-01 00:00:00 EXTRA"), nil},
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

// TestGoogleAds_ReadSettings_AbsentProvenanceIsRefusedBeforeContact is the ABSENCE half of
// the identity invariant, and the arm the mismatch test above cannot reach.
//
// A row that records NO creating customer is not a row that matches the current connection —
// it is a row nothing has established anything about. Waving it through queried the stored
// PlatformCampaignID under whatever account the project currently resolves to, and that id is
// unique only WITHIN a customer: on a collision the search returns ANOTHER account's campaign
// and this endpoint reports a divergence between this campaign's recorded budget and a
// different campaign's actual one. That is the precise false finding the readback exists to
// make impossible, so absence must fail CLOSED exactly as HubSpot's metrics read already does.
//
// The refusal is asserted BEFORE contact for the same reason the mismatch one is: absent
// provenance is a purely LOCAL fact, and no answer Google could give would change it.
func TestGoogleAds_ReadSettings_AbsentProvenanceIsRefusedBeforeContact(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result []byte
	}{
		{"no result blob at all", nil},
		{"empty result blob", []byte(`{}`)},
		{"blob recording an empty customer id", []byte(`{"customerId":""}`)},
		{"unparseable blob", []byte(`not json`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, requests := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED"},"campaignBudget":{"amountMicros":"500000000","period":"DAILY"}}]}`)

			camp := &model.Campaign{
				ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
				Result: tc.result,
			}
			_, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
			if !errors.Is(err, domain.ErrCampaignProvenanceUnknown) {
				t.Fatalf("err = %v, want ErrCampaignProvenanceUnknown — a row naming no creating customer "+
					"must not be read under the project's current one", err)
			}
			// Joined, never returned alone, so existing ErrCampaignAccountMismatch callers keep matching.
			if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
				t.Errorf("err = %v, want it to ALSO wrap ErrCampaignAccountMismatch so existing callers keep matching", err)
			}
			for _, r := range requests() {
				if strings.Contains(r, "googleAds:search") {
					t.Errorf("absent provenance is a purely local fact and must be refused BEFORE the "+
						"campaign is queried, but a search was issued: %v", requests())
				}
			}
		})
	}
}

// TestGoogleAds_ReadSettings_RecordedProvenanceStillReads is the other arm, and it must
// survive the fix above: adding the absence refusal must not turn a row that DOES record
// the current customer into a refusal. Both arms are load-bearing.
func TestGoogleAds_ReadSettings_RecordedProvenanceStillReads(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED"},"campaignBudget":{"amountMicros":"500000000","period":"DAILY"}}]}`)

	camp := &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		Result: []byte(`{"customerId":"1234567890"}`),
	}
	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings with matching recorded provenance: %v", err)
	}
	if rb == nil {
		t.Fatal("readback is nil for a row whose recorded customer matches the connection")
	}
}

// TestGoogleAds_ReadSettings_AbsentProvenanceOutranksAnUnusableConnection pins the
// ERROR PRECEDENCE between two failures that can hold at the same time.
//
// Absent provenance is a purely LOCAL fact about the row: it records no creating customer,
// so its bare-numeric PlatformCampaignID cannot be resolved against any account, and no
// answer the platform could give would change that. A broken connection is an UPSTREAM,
// transient fact. When both hold, the local one must win, because only it names the remedy
// that actually works: re-dispatch the row. Reporting the connection failure instead sends
// the operator to retry a read that will never succeed, hiding the real defect behind an
// unrelated outage — the identical inversion the HubSpot email guard was moved above its
// portal lookup to fix.
//
// The connection here is UNUSABLE, not healthy: with a healthy one, resolution succeeds and
// the guard is reached no matter where it sits, so a healthy fixture cannot discriminate
// between the guard running before or after the resolve. This one can.
func TestGoogleAds_ReadSettings_AbsentProvenanceOutranksAnUnusableConnection(t *testing.T) {
	connErr := errors.New("google ads connection is not usable")
	d := NewGoogleAdsDispatcher(fakeConnReader{err: connErr}, identityEncryptor{})

	// No Result blob at all: the row records no creating customer.
	camp := &model.Campaign{ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777"}

	_, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err == nil {
		t.Fatal("expected an error for an unstamped row")
	}
	// The documented deterministic answer, asserted against the domain sentinels rather than
	// against any string the guard itself builds.
	if !errors.Is(err, domain.ErrCampaignProvenanceUnknown) {
		t.Fatalf("err = %v, want ErrCampaignProvenanceUnknown (the local, deterministic fact must outrank the upstream one)", err)
	}
	// Joined, never returned alone, so existing account-mismatch callers keep matching.
	if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Fatalf("err = %v, want it JOINED with ErrCampaignAccountMismatch", err)
	}
	// The independent bound: the connection failure must NOT be what surfaced. This is the
	// assertion that fails when the guard sits after the resolve.
	if errors.Is(err, connErr) {
		t.Fatalf("err = %v, but the transient connection failure outranked the absent-provenance guard; the operator is sent to retry a read that can never succeed", err)
	}
}

// TestGoogleAdsCreationCustomerIDContractIsSplitByCaller pins the contract
// googleAdsCreationCustomerID's documentation describes: an empty return means
// UNKNOWN, and what unknown PERMITS is decided per caller, not by the helper.
//
// The doc previously said an empty result meant callers "must treat that as
// permission to proceed". The settings readback then began failing closed on
// exactly that input, so the single stated rule became false for one of its own
// callers. This test makes the split executable, so a future caller that changes
// side cannot leave the prose behind.
//
// Both halves are asserted by BEHAVIOUR, through the dispatcher, on the same
// absent-provenance input. An earlier version of this test grepped googleads.go
// for the guard expressions instead; Bugbot correctly identified that as vacuous,
// and a mutation confirmed it: deleting the settings readback's refusal left both
// substrings present elsewhere in the file and the test stayed green. Asserting on
// source text cannot distinguish a live guard from a comment that mentions it.
func TestGoogleAdsCreationCustomerIDContractIsSplitByCaller(t *testing.T) {
	// One campaign, no recorded creating customer: the single input the two sides
	// of the contract answer differently.
	absentProvenance := func() *model.Campaign {
		return &model.Campaign{
			ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
			Result: []byte(`{}`),
		}
	}
	const searchBody = `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED"},"campaignBudget":{"amountMicros":"500000000","period":"DAILY"}}]}`

	t.Run("fail-closed caller refuses absent provenance", func(t *testing.T) {
		d, _ := settingsDispatcher(t, searchBody)
		_, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, absentProvenance())
		if !errors.Is(err, domain.ErrCampaignProvenanceUnknown) {
			t.Fatalf("ReadSettings err = %v, want ErrCampaignProvenanceUnknown: the settings readback "+
				"is the caller whose answer is meaningless without provenance, so absence must fail closed", err)
		}
	})

	t.Run("permissive caller proceeds on absent provenance", func(t *testing.T) {
		d, _ := settingsDispatcher(t, searchBody)
		// The metrics read only ever COMPARES a recorded id, so an unrecorded one
		// cannot prove a mismatch and must not be turned into a refusal. It must get
		// PAST the provenance guard; whatever the stubbed upstream then answers is
		// beside the point, so only the provenance sentinels are excluded.
		_, err := d.ReadMetrics(context.Background(), "proj", model.ProviderGoogleAds,
			absentProvenance(), model.MetricsWindowLast7Days)
		if errors.Is(err, domain.ErrCampaignProvenanceUnknown) {
			t.Fatalf("ReadMetrics err = %v, want it NOT to refuse on absent provenance: a legacy row "+
				"cannot prove a mismatch, and refusing every one would break reads that work today", err)
		}
		if errors.Is(err, domain.ErrCampaignAccountMismatch) {
			t.Fatalf("ReadMetrics err = %v, want no mismatch: absence is not disagreement", err)
		}
	})
}

// TestGoogleAds_ReadSettings_AmountWithoutPeriodIsUnknown pins that the AMOUNT is selected
// only once the period has established its semantics.
//
// The two upstream budget fields mean different things — amount_micros is a DAILY rate,
// total_amount_micros a WHOLE-FLIGHT cap — and googleAdsUpstreamBudgetAmount reads whichever
// one is present without consulting the period. So a campaign recorded as a daily 500 against
// an upstream row carrying only total_amount_micros=500000000 and NO period reported
// `budget_amount: match`, when the two numbers describe different quantities: a 500/day rate
// and a 500 lifetime cap are not the same budget, and the equal digits are a coincidence of
// the units rather than an agreement.
//
// The client cannot catch this pair. Its own contradiction check at campaign_settings.go
// refuses an amount that disagrees with a NAMED period, and deliberately lets an absent one
// through — absence means "Google did not report this field" everywhere on CampaignSettings,
// pinned by TestGetCampaignSettings_UnreadableFieldIsAbsentNotZero, and cannot start
// signalling "inconsistent pair" without breaking that meaning. Its comment states the reason
// the pair is safe to pass on: the dispatcher is supposed to yield `unknown` rather than a
// fabricated verdict. This test is what makes that stated contract true.
//
// The recorded side is deliberately 500 — the value that makes the buggy path report `match`.
// A test recording a different amount would read `divergent` either way and pass against the
// bug.
func TestGoogleAds_ReadSettings_AmountWithoutPeriodIsUnknown(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED"},"campaignBudget":{"totalAmountMicros":"500000000"}}]}`)

	recorded := 500.0
	bt := model.BudgetDaily
	camp := &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		Result:       settingsProvenance(),
		BudgetAmount: &recorded, BudgetType: &bt,
	}

	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	budget := settingsField(t, rb, settingsFieldBudgetAmount)
	if budget.Upstream != nil {
		t.Errorf("budget_amount Upstream = %q, want nil: with no period, an upstream "+
			"total_amount_micros is a whole-flight cap of unestablished meaning, and reporting "+
			"it beside a recorded DAILY amount invites exactly the comparison that is unsound",
			*budget.Upstream)
	}
	if budget.Comparison != model.SettingsUnknown {
		t.Errorf("budget_amount comparison = %q, want %q: a 500/day rate and a 500 lifetime cap "+
			"are different budgets, and reporting `match` tells an operator the platform agrees "+
			"with a configuration it may well contradict",
			budget.Comparison, model.SettingsUnknown)
	}
}

// TestGoogleAds_ReadSettings_AmountWithUnknownPeriodIsUnknown is the same guarantee for the
// period Google DID send but declined to name. UNKNOWN/UNSPECIFIED establish the amount's
// semantics no better than absence does, and the client passes both through for that reason.
func TestGoogleAds_ReadSettings_AmountWithUnknownPeriodIsUnknown(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED"},"campaignBudget":{"totalAmountMicros":"500000000","period":"UNKNOWN"}}]}`)

	recorded := 500.0
	bt := model.BudgetDaily
	camp := &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		Result:       settingsProvenance(),
		BudgetAmount: &recorded, BudgetType: &bt,
	}

	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	budget := settingsField(t, rb, settingsFieldBudgetAmount)
	if budget.Comparison != model.SettingsUnknown {
		t.Errorf("budget_amount comparison = %q, want %q for an UNKNOWN period: a value Google "+
			"explicitly declined to name cannot establish which quantity the amount is",
			budget.Comparison, model.SettingsUnknown)
	}
}

// TestGoogleAds_ReadSettings_DailyAmountWithPeriodStillCompares is the other half: gating the
// amount on the period must not make every ordinary budget read `unknown`. A DAILY period with
// amount_micros is the common case and must still compare.
func TestGoogleAds_ReadSettings_DailyAmountWithPeriodStillCompares(t *testing.T) {
	d, _ := settingsDispatcher(t, `{"results":[{"campaign":{"resourceName":"customers/1234567890/campaigns/777","id":"777","name":"n","status":"ENABLED"},"campaignBudget":{"amountMicros":"500000000","period":"DAILY"}}]}`)

	recorded := 500.0
	bt := model.BudgetDaily
	camp := &model.Campaign{
		ID: "camp-1", Platform: model.ProviderGoogleAds, PlatformCampaignID: "777",
		Result:       settingsProvenance(),
		BudgetAmount: &recorded, BudgetType: &bt,
	}

	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderGoogleAds, camp)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	budget := settingsField(t, rb, settingsFieldBudgetAmount)
	if budget.Comparison != model.SettingsMatch {
		t.Errorf("budget_amount comparison = %q, want %q: a DAILY period with amount_micros is "+
			"the ordinary case and gating on the period must not suppress it",
			budget.Comparison, model.SettingsMatch)
	}
}
