// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

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
)

type settingsReply struct {
	status int
	body   string
}

// settingsTestClient answers each request by its PATH (campaign, ad set, account) and records the
// request URIs under a mutex, so the test goroutine reads them only after the handler has written.
func settingsTestClient(t *testing.T, routes map[string]settingsReply) (*Client, func() []string) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		rep, ok := routes[r.URL.Path]
		if !ok {
			rep = settingsReply{http.StatusTeapot, `{"error":{"message":"unrouted","code":1}}`}
		}
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(srv.Close)
	c := NewClient(
		Credentials{AccessToken: "tok"},
		AccountConfig{AccountID: "act_777"},
		WithBaseURL(srv.URL),
		WithClock(func() time.Time { return time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC) }),
		withRetryBaseDelay(time.Millisecond),
	)
	return c, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

const (
	metaSettingsCampaign = `{"id":"555","name":"KubeCon — Leads","status":"PAUSED","account_id":"777","objective":"OUTCOME_LEADS"}`
	metaSettingsAdSet    = `{"id":"888","campaign_id":"555","daily_budget":"5000","start_time":"2026-08-01T00:00:00+0000","end_time":"2026-08-31T16:59:59-0700","bid_strategy":"LOWEST_COST_WITHOUT_CAP"}`
	metaObjectMissing    = `{"error":{"message":"Unsupported get request","type":"GraphMethodException","code":100,"error_subcode":33}}`
)

func TestMetaGetCampaignSettings_ReadsCampaignThenAdSet(t *testing.T) {
	c, seen := settingsTestClient(t, map[string]settingsReply{
		"/555": {200, metaSettingsCampaign},
		"/888": {200, metaSettingsAdSet},
	})
	got, err := c.GetCampaignSettings(context.Background(), "555", "888")
	if err != nil {
		t.Fatalf("GetCampaignSettings: %v", err)
	}
	if got.CampaignID != "555" || *got.Name != "KubeCon — Leads" || *got.Status != "PAUSED" || got.AccountID != "act_777" {
		t.Fatalf("campaign = %+v", got)
	}
	as := got.AdSet
	if as == nil || *as.DailyMinor != 5000 || as.LifetimeMinor != nil || *as.StartTime != "2026-08-01T00:00:00+0000" ||
		*as.EndTime != "2026-08-31T16:59:59-0700" || *as.BidStrategy != "LOWEST_COST_WITHOUT_CAP" {
		t.Fatalf("ad set = %+v", as)
	}
	want := []string{
		"GET /555?fields=" + settingsCampaignFields,
		"GET /888?fields=" + settingsAdSetFields,
	}
	if g := seen(); len(g) != 2 || g[0] != want[0] || g[1] != want[1] {
		t.Errorf("requests = %q, want %q", g, want)
	}
}

func TestMetaGetCampaignSettings_NoAdSetReadsTheCampaignOnly(t *testing.T) {
	c, seen := settingsTestClient(t, map[string]settingsReply{"/555": {200, metaSettingsCampaign}})
	got, err := c.GetCampaignSettings(context.Background(), "555", "")
	if err != nil || got == nil || got.AdSet != nil {
		t.Fatalf("got %+v, %v; want the campaign with no ad set", got, err)
	}
	if n := len(seen()); n != 1 {
		t.Fatalf("want one request, got %d", n)
	}
}

func TestMetaGetCampaignSettings_Outcomes(t *testing.T) {
	cases := []struct {
		name       string
		campaign   settingsReply
		adSet      settingsReply
		noAdSet    bool
		wantErr    bool
		wantNotIn  bool
		wantStatus string
	}{
		{name: "an archived campaign is reported, not absent", campaign: settingsReply{200, `{"id":"555","name":"n","status":"ARCHIVED","account_id":"777"}`}, adSet: settingsReply{200, metaSettingsAdSet}, wantStatus: "ARCHIVED"},
		{name: "a deleted campaign is reported with its status, not absent", campaign: settingsReply{200, `{"id":"555","name":"n","status":"DELETED","account_id":"777"}`}, adSet: settingsReply{200, metaSettingsAdSet}, wantStatus: "DELETED"},
		// 100/33 cannot tell "deleted" from "this token cannot load it" on ANY HTTP status, and no
		// further read resolves that, so it is unverifiable (503) — never absence (404) — exactly
		// as on the adoption read. No account probe is sent to try to settle it.
		{name: "100/33 on a 400 is unverifiable", campaign: settingsReply{400, metaObjectMissing}, wantErr: true},
		{name: "100/33 on a 403 is unverifiable", campaign: settingsReply{403, metaObjectMissing}, wantErr: true},
		{name: "100/33 on a 404 is unverifiable", campaign: settingsReply{404, metaObjectMissing}, wantErr: true},
		{name: "a missing ad set leaves the campaign readable", campaign: settingsReply{200, metaSettingsCampaign}, adSet: settingsReply{400, metaObjectMissing}, noAdSet: true},
		{name: "an ad set of another campaign is refused", campaign: settingsReply{200, metaSettingsCampaign}, adSet: settingsReply{200, `{"id":"888","campaign_id":"556","daily_budget":"5000"}`}, wantErr: true, wantNotIn: true},
		{name: "an ad set with no campaign_id is refused", campaign: settingsReply{200, metaSettingsCampaign}, adSet: settingsReply{200, `{"id":"888","daily_budget":"5000"}`}, wantErr: true, wantNotIn: true},
		{name: "both budgets on the ad set is contradictory", campaign: settingsReply{200, metaSettingsCampaign}, adSet: settingsReply{200, `{"id":"888","campaign_id":"555","daily_budget":"1","lifetime_budget":"2"}`}, wantErr: true},
		{name: "a non-integer budget is untrustworthy", campaign: settingsReply{200, metaSettingsCampaign}, adSet: settingsReply{200, `{"id":"888","campaign_id":"555","daily_budget":"12.5"}`}, wantErr: true},
		{name: "a duplicated budget key is untrustworthy", campaign: settingsReply{200, metaSettingsCampaign}, adSet: settingsReply{200, `{"id":"888","campaign_id":"555","daily_budget":"1","daily_budget":"2"}`}, wantErr: true},
		{name: "another ad set echoed is untrustworthy", campaign: settingsReply{200, metaSettingsCampaign}, adSet: settingsReply{200, `{"id":"889","campaign_id":"555"}`}, wantErr: true},
		{name: "another campaign echoed is untrustworthy", campaign: settingsReply{200, `{"id":"556","name":"n","status":"PAUSED","account_id":"777"}`}, wantErr: true},
		{name: "a duplicated campaign id is untrustworthy", campaign: settingsReply{200, `{"id":"556","id":"555","name":"n","status":"PAUSED","account_id":"777"}`}, wantErr: true},
		{name: "a missing account_id is untrustworthy", campaign: settingsReply{200, `{"id":"555","name":"n","status":"PAUSED"}`}, wantErr: true},
		{name: "a malformed body is untrustworthy", campaign: settingsReply{200, `{"id":`}, wantErr: true},
		{name: "a 401 is an error, not absence", campaign: settingsReply{401, `{"error":{"message":"bad token","code":190}}`}, wantErr: true},
		{name: "a 403 is an error, not absence", campaign: settingsReply{403, `{"error":{"message":"denied","code":200}}`}, wantErr: true},
		{name: "a 5xx is an error", campaign: settingsReply{500, `{"error":{"message":"boom","code":1}}`}, wantErr: true},
		{name: "a 429 after retries is an error", campaign: settingsReply{429, `{"error":{"message":"slow down","code":4}}`}, wantErr: true},
		{name: "an ad set 5xx is an error", campaign: settingsReply{200, metaSettingsCampaign}, adSet: settingsReply{500, `{"error":{"message":"boom","code":1}}`}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			routes := map[string]settingsReply{"/555": tc.campaign}
			if tc.adSet.status != 0 {
				routes["/888"] = tc.adSet
			}
			// The account is routed as LOADABLE, so a probe that would turn 100/33 into absence
			// would succeed — and the assertion below catches it being sent at all.
			routes["/act_777"] = settingsReply{200, `{"id":"act_777"}`}
			c, seen := settingsTestClient(t, routes)
			got, err := c.GetCampaignSettings(context.Background(), "555", "888")
			// Nothing this read does touches the ad account: absence is never "proven" by probing
			// it, and a campaign that answered needs no proof either.
			for _, r := range seen() {
				if strings.HasPrefix(r, "GET /act_777") {
					t.Fatalf("the settings read probed the ad account: %q", seen())
				}
			}
			switch {
			case tc.wantErr:
				if err == nil || got != nil {
					t.Fatalf("want an error and nothing read, got %+v, %v", got, err)
				}
				if tc.wantNotIn != errors.Is(err, ErrAdSetNotInCampaign) {
					t.Fatalf("errors.Is(ErrAdSetNotInCampaign) = %v, want %v (%v)", !tc.wantNotIn, tc.wantNotIn, err)
				}
			default:
				if err != nil || got == nil {
					t.Fatalf("GetCampaignSettings: %+v, %v", got, err)
				}
				if tc.noAdSet != (got.AdSet == nil) {
					t.Fatalf("AdSet = %+v, want absent=%v", got.AdSet, tc.noAdSet)
				}
				if tc.wantStatus != "" && (got.Status == nil || *got.Status != tc.wantStatus) {
					t.Fatalf("Status = %v, want %q", got.Status, tc.wantStatus)
				}
			}
		})
	}
}

func TestMetaAccountCurrencyOffset(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reply   settingsReply
		offset  int64
		known   bool
		wantErr bool
	}{
		{name: "USD", reply: settingsReply{200, `{"currency":"USD"}`}, offset: 100, known: true},
		{name: "JPY is zero-decimal", reply: settingsReply{200, `{"currency":"JPY"}`}, offset: 1, known: true},
		{name: "an unmapped currency is unknown, not guessed", reply: settingsReply{200, `{"currency":"ZZZ"}`}},
		{name: "a failed preflight is an error", reply: settingsReply{500, `{"error":{"message":"boom","code":1}}`}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, seen := settingsTestClient(t, map[string]settingsReply{"/act_777": tc.reply})
			off, known, err := c.AccountCurrencyOffset(context.Background())
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil || off != tc.offset || known != tc.known {
				t.Fatalf("got (%d, %v, %v), want (%d, %v, nil)", off, known, err, tc.offset, tc.known)
			}
			if g := seen(); len(g) != 1 || !strings.HasPrefix(g[0], "GET /act_777?fields=currency") {
				t.Errorf("requests = %q", g)
			}
		})
	}
}
