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

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// metaTargetingAdSetBody is the ad set with the targeting and schedule the readback asks for.
const metaTargetingAdSetBody = `{"id":"888","campaign_id":"555","lifetime_budget":"500000","start_time":"2026-08-01T00:00:00+0000","end_time":"2026-08-31T16:59:59-0700","bid_strategy":"LOWEST_COST_WITHOUT_CAP",` +
	`"targeting":{"geo_locations":{"countries":["US"],"regions":[{"key":"3847","name":"California","country":"US"},{"key":3843}]}},` +
	`"adset_schedule":[{"start_minute":480,"end_minute":1020,"days":[1,5],"timezone_type":"ADVERTISER"}]}`

// geoRow is metaSettingsRow with a lifetime budget and a config snapshot that recorded
// two regions, no cities and a schedule.
func geoRow() *model.Campaign {
	row := metaSettingsRow()
	row.BudgetAmount = floatPtr(5000)
	row.BudgetType = budgetTypePtr(model.BudgetLifetime)
	row.ConfigSnapshot = json.RawMessage(`{"targeting":{"regions":["3843"," 3847"],"cities":null},"adSchedule":{"days":[5,1,5],"startMinute":480,"endMinute":1020}}`)
	return row
}

// metaTargetingReadback runs ReadSettings against a fake Graph API answering the ad set with adSet,
// and returns the readback and the ad set request's fields parameter.
func metaTargetingReadback(t *testing.T, row *model.Campaign, adSet string) (*model.CampaignSettingsReadback, string) {
	t.Helper()
	var (
		mu     sync.Mutex
		fields string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/555":
			_, _ = io.WriteString(w, metaSettingsCampaignBody)
		case "/888":
			mu.Lock()
			fields = r.URL.Query().Get("fields")
			mu.Unlock()
			_, _ = io.WriteString(w, adSet)
		case "/act_777":
			_, _ = io.WriteString(w, `{"id":"act_777","currency":"USD"}`)
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	d.settingsNow = pinnedSettingsClock
	rb, err := d.ReadSettings(context.Background(), "proj", model.ProviderMetaAds, row)
	if err != nil {
		t.Fatalf("ReadSettings: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return rb, fields
}

// TestMeta_ReadSettings_TargetingMatchesTheSnapshot: with the flag on, the ad set is asked for its
// targeting and schedule, and regions, cities and ad_schedule compare like with like (order,
// repeats and key type ignored; no cities on both sides is a match).
func TestMeta_ReadSettings_TargetingMatchesTheSnapshot(t *testing.T) {
	t.Setenv(constants.EnvMetaTargetingReadbackEnabled, "true")
	rb, fields := metaTargetingReadback(t, geoRow(), metaTargetingAdSetBody)
	if !strings.HasSuffix(fields, ",targeting,adset_schedule") {
		t.Errorf("ad set fields = %q, want the targeting and schedule asked for", fields)
	}
	assertSettingsFields(t, rb, map[string]fieldWant{
		settingsFieldBudgetAmount:    {"5000.00", "5000.00", model.SettingsMatch},
		settingsFieldBudgetType:      {"lifetime", "lifetime", model.SettingsMatch},
		settingsFieldName:            {"KubeCon — Leads", "KubeCon — Leads", model.SettingsMatch},
		settingsFieldStatus:          {"", "PAUSED", model.SettingsUnknown},
		settingsFieldStartDate:       {"2026-08-01", "2026-08-01", model.SettingsMatch},
		settingsFieldEndDate:         {"2026-08-31", "2026-08-31", model.SettingsMatch},
		settingsFieldBiddingStrategy: {"", "LOWEST_COST_WITHOUT_CAP", model.SettingsUnknown},
		settingsFieldRegions:         {"3843,3847", "3843,3847", model.SettingsMatch},
		settingsFieldCities:          {"", "", model.SettingsMatch},
		settingsFieldAdSchedule:      {"days=1,5 08:00-17:00 ADVERTISER", "days=1,5 08:00-17:00 ADVERTISER", model.SettingsMatch},
	})
}

// TestMeta_ReadSettings_TargetingDivergesAndUnknowns: an edited ad set diverges; a row with no
// snapshot has no recorded side; an ad set whose schedule or geo cannot be read has no upstream side.
func TestMeta_ReadSettings_TargetingDivergesAndUnknowns(t *testing.T) {
	t.Setenv(constants.EnvMetaTargetingReadbackEnabled, "true")
	edited := strings.Replace(metaTargetingAdSetBody, `{"key":3843}`, `{"key":"3843"}],"cities":[{"key":"2420605","radius":10}`, 1)
	edited = strings.Replace(edited, `"end_minute":1020`, `"end_minute":1080`, 1)
	rb, _ := metaTargetingReadback(t, geoRow(), edited)
	for name, want := range map[string]fieldWant{
		settingsFieldRegions:    {"3843,3847", "3843,3847", model.SettingsMatch},
		settingsFieldCities:     {"", "2420605", model.SettingsDiverged},
		settingsFieldAdSchedule: {"days=1,5 08:00-17:00 ADVERTISER", "days=1,5 08:00-18:00 ADVERTISER", model.SettingsDiverged},
	} {
		f := settingsField(t, rb, name)
		if derefOr(f.Recorded) != want.recorded || derefOr(f.Upstream) != want.upstream || f.Comparison != want.verdict {
			t.Errorf("%s = %q / %q %s, want %q / %q %s", name, derefOr(f.Recorded), derefOr(f.Upstream), f.Comparison, want.recorded, want.upstream, want.verdict)
		}
	}

	legacy := geoRow()
	legacy.ConfigSnapshot = nil
	rb, _ = metaTargetingReadback(t, legacy, metaTargetingAdSetBody)
	for _, name := range []string{settingsFieldRegions, settingsFieldCities, settingsFieldAdSchedule} {
		if f := settingsField(t, rb, name); f.Recorded != nil || f.Upstream == nil || f.Comparison != model.SettingsUnknown {
			t.Errorf("no snapshot: %s = %+v, want no recorded side and unknown", name, f)
		}
	}

	broken := strings.Replace(metaTargetingAdSetBody, `"days":[1,5]`, `"days":"weekdays"`, 1)
	broken = strings.Replace(broken, `{"key":3843}`, `{"key":"Bay Area"}`, 1)
	rb, _ = metaTargetingReadback(t, geoRow(), broken)
	for _, name := range []string{settingsFieldRegions, settingsFieldCities, settingsFieldAdSchedule} {
		if f := settingsField(t, rb, name); f.Upstream != nil || f.Recorded == nil || f.Comparison != model.SettingsUnknown {
			t.Errorf("unreadable: %s = %+v, want no upstream side and unknown", name, f)
		}
	}
	if f := settingsField(t, rb, settingsFieldBudgetAmount); f.Comparison != model.SettingsMatch {
		t.Errorf("an unreadable targeting must not fail the rest of the read: budget = %+v", f)
	}
}

// TestMeta_ReadSettings_TargetingOffIsUnchanged: with the flag off the ad set read asks for no
// targeting and the readback carries exactly the seven fields it always did.
func TestMeta_ReadSettings_TargetingOffIsUnchanged(t *testing.T) {
	t.Setenv(constants.EnvMetaTargetingReadbackEnabled, "1")
	rb, fields := metaTargetingReadback(t, geoRow(), metaTargetingAdSetBody)
	if strings.Contains(fields, "targeting") || strings.Contains(fields, "adset_schedule") {
		t.Errorf("ad set fields = %q, want no targeting asked for", fields)
	}
	if len(rb.Fields) != 7 {
		t.Errorf("readback has %d fields, want the 7 it always had: %+v", len(rb.Fields), rb.Fields)
	}
}

// TestRenderAdSchedule: blocks render with sorted, de-duplicated days and clock times; none is "".
func TestRenderAdSchedule(t *testing.T) {
	got := renderAdSchedule([]meta.AdScheduleBlock{
		{Days: []int{3, 1, 3}, StartMinute: 60, EndMinute: 120, TimezoneType: "USER"},
		{Days: []int{6, 0}, StartMinute: 0, EndMinute: 1440, TimezoneType: "advertiser"},
	})
	if want := "days=0,6 00:00-24:00 ADVERTISER; days=1,3 01:00-02:00 USER"; got != want {
		t.Errorf("renderAdSchedule = %q, want %q", got, want)
	}
	if got := renderAdSchedule(nil); got != "" {
		t.Errorf("no blocks = %q, want empty", got)
	}
	if got := renderSettingsKeys([]string{"10", " 9", "10", ""}); got != "9,10" {
		t.Errorf("renderSettingsKeys = %q, want 9,10", got)
	}
}
