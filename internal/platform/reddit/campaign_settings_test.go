// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"net/http"
	"testing"
	"time"
)

const redditSettingsBody = `{"data":{"id":"t3_camp","ad_account_id":"t2_test","name":"KubeCon — Traffic","configured_status":"PAUSED","goal_type":"LIFETIME_SPEND","goal_value":250000000,"is_campaign_budget_optimization":true,"start_time":"2026-08-01T00:00:00+00:00","end_time":"2026-08-31T00:00:00+00:00","bid_strategy":"BIDLESS"}}`

func TestRedditGetCampaignSettings_ReadsTheAccountScopedCampaign(t *testing.T) {
	c, seen := budgetTestClient(t, http.StatusOK, redditSettingsBody)
	got, err := c.GetCampaignSettings(context.Background(), "t3_camp")
	if err != nil {
		t.Fatalf("GetCampaignSettings: %v", err)
	}
	if got == nil || got.CampaignID != "t3_camp" || got.AdAccountID != "t2_test" || *got.Name != "KubeCon — Traffic" ||
		*got.ConfiguredStatus != "PAUSED" || *got.GoalType != GoalTypeLifetimeSpend || *got.GoalValueMicros != 250000000 ||
		!*got.CampaignBudgetOptimization || *got.StartTime != "2026-08-01T00:00:00+00:00" || *got.EndTime != "2026-08-31T00:00:00+00:00" ||
		*got.BidStrategy != "BIDLESS" {
		t.Fatalf("settings = %+v", got)
	}
	if g := seen(); len(g) != 1 || g[0] != "GET /api/v3/ad_accounts/t2_test/campaigns/t3_camp " {
		t.Errorf("requests = %q, want exactly one GET of the account-scoped campaign", g)
	}
}

func TestRedditGetCampaignSettings_Outcomes(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		absent  bool
		wantErr bool
	}{
		{name: "absent fields stay nil", status: 200, body: `{"data":{"id":"t3_camp"}}`},
		{name: "a deleted campaign is reported, not absent", status: 200, body: `{"data":{"id":"t3_camp","configured_status":"DELETED"}}`},
		{name: "404 is absent", status: 404, body: `{}`, absent: true},
		{name: "a fractional goal_value is untrustworthy", status: 200, body: `{"data":{"id":"t3_camp","goal_value":1.5}}`, wantErr: true},
		{name: "another campaign is untrustworthy", status: 200, body: `{"data":{"id":"t3_other"}}`, wantErr: true},
		{name: "a duplicated goal_value is untrustworthy", status: 200, body: `{"data":{"id":"t3_camp","goal_value":1,"goal_value":2}}`, wantErr: true},
		{name: "a duplicated data key is untrustworthy", status: 200, body: `{"data":{"id":"t3_other"},"data":{"id":"t3_camp"}}`, wantErr: true},
		{name: "no data is untrustworthy", status: 200, body: `{"data":null}`, wantErr: true},
		{name: "a malformed body is untrustworthy", status: 200, body: `{"data":{"id":`, wantErr: true},
		{name: "401 is an error, not absence", status: 401, body: `{}`, wantErr: true},
		{name: "403 is an error, not absence", status: 403, body: `{}`, wantErr: true},
		{name: "5xx is an error", status: 502, body: `{}`, wantErr: true},
		{name: "429 after retries is an error", status: 429, body: `{}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := budgetTestClient(t, tc.status, tc.body, withRetryBaseDelay(time.Millisecond))
			got, err := c.GetCampaignSettings(context.Background(), "t3_camp")
			switch {
			case tc.wantErr:
				if err == nil || got != nil {
					t.Fatalf("want an error and nothing read, got %+v, %v", got, err)
				}
			case tc.absent:
				if err != nil || got != nil {
					t.Fatalf("want (nil, nil), got %+v, %v", got, err)
				}
			default:
				if err != nil || got == nil {
					t.Fatalf("GetCampaignSettings: %+v, %v", got, err)
				}
				if got.GoalValueMicros != nil || got.GoalType != nil || got.Name != nil {
					t.Fatalf("absent fields were filled: %+v", got)
				}
			}
		})
	}
}
