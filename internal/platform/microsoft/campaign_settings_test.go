// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGetCampaignSettings_ReadsOneCampaignOfAnyType(t *testing.T) {
	rec := &budgetRecorder{}
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		_, _ = io.WriteString(w, `{"Campaigns":[{"Id":321,"Name":"KubeCon — Search","Status":"Paused","BudgetType":"DailyBudgetStandard","DailyBudget":75.25,"BudgetId":null,"BiddingScheme":{"Type":"EnhancedCpcBiddingScheme"}}],"PartialErrors":[]}`)
	})
	got, err := c.GetCampaignSettings(context.Background(), "321")
	if err != nil {
		t.Fatalf("GetCampaignSettings: %v", err)
	}
	if got == nil || got.CampaignID != "321" || *got.Name != "KubeCon — Search" || *got.Status != "Paused" ||
		*got.BudgetType != BudgetTypeDailyStandard || *got.DailyBudget != 75.25 || *got.Shared || *got.BiddingSchemeType != "EnhancedCpc" {
		t.Fatalf("settings = %+v", got)
	}
	reqs := rec.all()
	if len(reqs) != 1 || reqs[0].method != http.MethodPost || !strings.HasSuffix(reqs[0].path, "/Campaigns/QueryByIds") {
		t.Fatalf("want exactly one POST .../Campaigns/QueryByIds, got %+v", reqs)
	}
	if want := `{"AccountId":1234567,"CampaignIds":[321],"CampaignType":"Search,Shopping,DynamicSearchAds,Audience,Hotel,PerformanceMax,App"}`; reqs[0].body != want {
		t.Errorf("body = %s, want %s", reqs[0].body, want)
	}
}

// Absent fields stay ABSENT: a nil, never a zero standing in for a value Microsoft did not send.
func TestGetCampaignSettings_AbsentFieldsAreNilNotZero(t *testing.T) {
	c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"Campaigns":[{"Id":321}]}`)
	})
	got, err := c.GetCampaignSettings(context.Background(), "321")
	if err != nil {
		t.Fatalf("GetCampaignSettings: %v", err)
	}
	if got.Name != nil || got.Status != nil || got.BudgetType != nil || got.DailyBudget != nil || got.BiddingSchemeType != nil {
		t.Fatalf("absent fields were filled: %+v", got)
	}
	// An absent BudgetId is Microsoft's documented "own budget".
	if got.Shared == nil || *got.Shared {
		t.Fatalf("absent BudgetId must read as not shared, got %v", got.Shared)
	}
}

func TestGetCampaignSettings_Outcomes(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		absent  bool
		wantErr bool
		shared  *bool
	}{
		{name: "shared budget", status: 200, body: `{"Campaigns":[{"Id":321,"BudgetId":99}]}`, shared: boolp(true)},
		{name: "zero budget id is own budget", status: 200, body: `{"Campaigns":[{"Id":321,"BudgetId":0}]}`, shared: boolp(false)},
		{name: "unreadable budget id is unknown, not private", status: 200, body: `{"Campaigns":[{"Id":321,"BudgetId":-4}]}`},
		{name: "no such campaign, as a PartialError", status: 200, body: `{"Campaigns":[null],"PartialErrors":[{"Code":1100,"ErrorCode":"CampaignServiceInvalidCampaignId","Index":0}]}`, absent: true},
		{name: "another campaign is untrustworthy", status: 200, body: `{"Campaigns":[{"Id":322}]}`, wantErr: true},
		{name: "a duplicated Id is untrustworthy", status: 200, body: `{"Campaigns":[{"Id":322,"Id":321}]}`, wantErr: true},
		{name: "a duplicated DailyBudget is untrustworthy", status: 200, body: `{"Campaigns":[{"Id":321,"DailyBudget":1,"DailyBudget":2}]}`, wantErr: true},
		{name: "a non-numeric DailyBudget is untrustworthy", status: 200, body: `{"Campaigns":[{"Id":321,"DailyBudget":{"x":1}}]}`, wantErr: true},
		{name: "a negative DailyBudget is untrustworthy", status: 200, body: `{"Campaigns":[{"Id":321,"DailyBudget":-1}]}`, wantErr: true},
		{name: "a non-string Name is untrustworthy", status: 200, body: `{"Campaigns":[{"Id":321,"Name":7}]}`, wantErr: true},
		{name: "an omitted Campaigns field is untrustworthy", status: 200, body: `{}`, wantErr: true},
		{name: "a malformed body is untrustworthy", status: 200, body: `{"Campaigns":[`, wantErr: true},
		{name: "401 is an error, not an absence", status: 401, body: `{}`, wantErr: true},
		{name: "403 is an error, not an absence", status: 403, body: `{}`, wantErr: true},
		{name: "5xx is an error", status: 500, body: `{}`, wantErr: true},
		{name: "429 after retries is an error", status: 429, body: `{}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newAPIClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			got, err := c.GetCampaignSettings(context.Background(), "321")
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
				if (tc.shared == nil) != (got.Shared == nil) || (tc.shared != nil && *tc.shared != *got.Shared) {
					t.Fatalf("Shared = %v, want %v", got.Shared, tc.shared)
				}
			}
		})
	}
}

// A malformed id is refused before anything is sent.
func TestGetCampaignSettings_MalformedIDSendsNothing(t *testing.T) {
	rec := &budgetRecorder{}
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		_, _ = io.WriteString(w, `{}`)
	})
	if _, err := c.GetCampaignSettings(context.Background(), " 321"); err == nil {
		t.Fatal("expected a malformed id to be refused")
	}
	if n := len(rec.all()); n != 0 {
		t.Fatalf("a malformed id still sent %d request(s)", n)
	}
}

func boolp(b bool) *bool { return &b }
