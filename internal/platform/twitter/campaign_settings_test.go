// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type settingsReply struct {
	status int
	body   string
}

// settingsServer answers by PATH and records each request under a mutex, so the test goroutine
// reads them only after the handler has written.
func settingsServer(t *testing.T, routes map[string]settingsReply) (*Client, func() []budgetCall) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []budgetCall
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		seen = append(seen, budgetCall{r.Method, r.URL.Path, r.URL.RawQuery})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		rep, ok := routes[r.URL.Path]
		if !ok {
			rep = settingsReply{http.StatusTeapot, `{}`}
		}
		if rep.status == http.StatusTooManyRequests {
			// A reset far beyond the client's retry cap: it gives up at once rather than sleeping,
			// which is the same exhausted-429 error a retried-out throttle produces.
			w.Header().Set("Retry-After", "86400")
		}
		w.WriteHeader(rep.status)
		_, _ = io.WriteString(w, rep.body)
	}))
	t.Cleanup(srv.Close)
	return newToggleTestClient(t, srv.URL), func() []budgetCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]budgetCall(nil), seen...)
	}
}

const (
	xSettingsCampaignPath = "/12/accounts/acc1/campaigns/cmp1"
	xSettingsLineItemPath = "/12/accounts/acc1/line_items/li1"
	xSettingsCampaign     = `{"data":{"id":"cmp1","name":"KubeCon — Awareness","entity_status":"PAUSED","budget_optimization":"CAMPAIGN","daily_budget_amount_local_micro":50000000,"total_budget_amount_local_micro":null,"deleted":false}}`
	xSettingsLineItem     = `{"data":{"id":"li1","campaign_id":"cmp1","start_time":"2026-08-01T00:00:00Z","end_time":"2026-08-31T00:00:00Z","bid_strategy":"AUTO","deleted":false}}`
)

func TestXGetCampaignSettings_ReadsCampaignThenLineItem(t *testing.T) {
	c, seen := settingsServer(t, map[string]settingsReply{
		xSettingsCampaignPath: {200, xSettingsCampaign},
		xSettingsLineItemPath: {200, xSettingsLineItem},
	})
	got, err := c.GetCampaignSettings(context.Background(), "cmp1", "li1")
	if err != nil {
		t.Fatalf("GetCampaignSettings: %v", err)
	}
	if got.CampaignID != "cmp1" || *got.Name != "KubeCon — Awareness" || *got.EntityStatus != "PAUSED" ||
		*got.DailyMicros != 50000000 || got.TotalMicros != nil || *got.BudgetOptimization != "CAMPAIGN" {
		t.Fatalf("campaign = %+v", got)
	}
	li := got.LineItem
	if li == nil || *li.StartTime != "2026-08-01T00:00:00Z" || *li.EndTime != "2026-08-31T00:00:00Z" || *li.BidStrategy != "AUTO" {
		t.Fatalf("line item = %+v", li)
	}
	g := seen()
	if len(g) != 2 || g[0].Method != http.MethodGet || g[0].Path != xSettingsCampaignPath ||
		g[1].Method != http.MethodGet || g[1].Path != xSettingsLineItemPath || g[1].RawQuery != "with_deleted=true" {
		t.Errorf("requests = %+v", g)
	}
}

func TestXGetCampaignSettings_Outcomes(t *testing.T) {
	cases := []struct {
		name       string
		campaign   settingsReply
		lineItem   settingsReply
		absent     bool
		noLineItem bool
		wantErr    bool
		wantNotIn  bool
	}{
		{name: "a 404 campaign is absent", campaign: settingsReply{404, `{}`}, absent: true},
		{name: "a deleted campaign is absent", campaign: settingsReply{200, `{"data":{"id":"cmp1","deleted":true}}`}, absent: true},
		{name: "a 404 line item leaves the campaign readable", campaign: settingsReply{200, xSettingsCampaign}, lineItem: settingsReply{404, `{}`}, noLineItem: true},
		{name: "a deleted line item leaves the campaign readable", campaign: settingsReply{200, xSettingsCampaign}, lineItem: settingsReply{200, `{"data":{"id":"li1","campaign_id":"cmp1","deleted":true}}`}, noLineItem: true},
		{name: "a line item of another campaign is refused", campaign: settingsReply{200, xSettingsCampaign}, lineItem: settingsReply{200, `{"data":{"id":"li1","campaign_id":"cmp9"}}`}, wantErr: true, wantNotIn: true},
		{name: "another line item echoed is untrustworthy", campaign: settingsReply{200, xSettingsCampaign}, lineItem: settingsReply{200, `{"data":{"id":"li2","campaign_id":"cmp1"}}`}, wantErr: true},
		{name: "another campaign echoed is untrustworthy", campaign: settingsReply{200, `{"data":{"id":"cmp2"}}`}, wantErr: true},
		{name: "a duplicated id is untrustworthy", campaign: settingsReply{200, `{"data":{"id":"cmp2","id":"cmp1"}}`}, wantErr: true},
		{name: "a duplicated data key is untrustworthy", campaign: settingsReply{200, `{"data":{"id":"cmp2"},"data":{"id":"cmp1"}}`}, wantErr: true},
		{name: "a fractional budget is untrustworthy", campaign: settingsReply{200, `{"data":{"id":"cmp1","daily_budget_amount_local_micro":1.5}}`}, wantErr: true},
		{name: "a negative budget is untrustworthy", campaign: settingsReply{200, `{"data":{"id":"cmp1","total_budget_amount_local_micro":-1}}`}, wantErr: true},
		{name: "a malformed body is untrustworthy", campaign: settingsReply{200, `{"data":{`}, wantErr: true},
		{name: "a 401 is an error, not absence", campaign: settingsReply{401, `{}`}, wantErr: true},
		{name: "a 403 is an error, not absence", campaign: settingsReply{403, `{}`}, wantErr: true},
		{name: "a 5xx is an error", campaign: settingsReply{503, `{}`}, wantErr: true},
		{name: "a 429 the client will not wait out is an error", campaign: settingsReply{429, `{}`}, wantErr: true},
		{name: "a line item 5xx is an error", campaign: settingsReply{200, xSettingsCampaign}, lineItem: settingsReply{500, `{}`}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			routes := map[string]settingsReply{xSettingsCampaignPath: tc.campaign}
			if tc.lineItem.status != 0 {
				routes[xSettingsLineItemPath] = tc.lineItem
			}
			c, _ := settingsServer(t, routes)
			got, err := c.GetCampaignSettings(context.Background(), "cmp1", "li1")
			switch {
			case tc.wantErr:
				if err == nil || got != nil {
					t.Fatalf("want an error and nothing read, got %+v, %v", got, err)
				}
				if tc.wantNotIn != errors.Is(err, ErrLineItemNotInCampaign) {
					t.Fatalf("errors.Is(ErrLineItemNotInCampaign) mismatch: %v", err)
				}
			case tc.absent:
				if err != nil || got != nil {
					t.Fatalf("want (nil, nil), got %+v, %v", got, err)
				}
			default:
				if err != nil || got == nil || tc.noLineItem != (got.LineItem == nil) {
					t.Fatalf("got %+v, %v; want line item absent=%v", got, err, tc.noLineItem)
				}
			}
		})
	}
}
