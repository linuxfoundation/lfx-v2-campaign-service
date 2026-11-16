// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestTargetingSegment pins #587's Targeting segment on X: "Retargeting" for the retargeting
// tactic, so lf-dbt's "%PROSPECT%"-first parser stops filing it as Prospecting.
func TestTargetingSegment(t *testing.T) {
	for _, tc := range []struct{ tactic, slot, want string }{
		{"", "", "Prospecting"},
		{"retargeting", "#2", "Retargeting #2"},
		{"lookalike", "", "Prospecting lookalike"},
		{"", "#3", "Prospecting #3"},
	} {
		if got := targetingSegment(tc.tactic, tc.slot); got != tc.want {
			t.Errorf("targetingSegment(%q, %q) = %q, want %q", tc.tactic, tc.slot, got, tc.want)
		}
	}
}

// TestCreateCampaign_ReusesALegacyNamedRetargetingCampaign: X reuses a campaign by name on every
// create, so a retargeting retry spanning #587 must still find the campaign an earlier attempt
// created as "Prospecting retargeting", and reuse it rather than create a second one.
func TestCreateCampaign_ReusesALegacyNamedRetargetingCampaign(t *testing.T) {
	in := twitterBiddingInput(0)
	in.Tactic, in.SlotToken, in.StageToken = "retargeting", "#2", "BoFU"
	legacy := legacyTwitterCampaignName(in)
	if !strings.Contains(legacy, "| Prospecting retargeting #2 |") || legacy == buildTwitterCampaignName(in) {
		t.Fatalf("fixture: legacy name %q must differ from the new one", legacy)
	}

	var mu sync.Mutex
	campaignCreates := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "campaigns"):
			// X's name search: only the legacy name exists upstream.
			if strings.Contains(r.URL.Query().Get("q"), "Prospecting retargeting") {
				_, _ = w.Write([]byte(`{"data":[{"id":"cmp7","name":"` + legacy + `"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "campaigns"):
			mu.Lock()
			campaignCreates++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case strings.HasSuffix(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case strings.HasSuffix(r.URL.Path, "promoted_tweets"):
			_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
		WithBaseURL(srv.URL), WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	mu.Lock()
	creates := campaignCreates
	mu.Unlock()
	if res.CampaignID != "cmp7" || creates != 0 {
		t.Fatalf("campaign = %q with %d create(s); want the reused cmp7 and none", res.CampaignID, creates)
	}
	if res.CampaignName != legacy {
		t.Errorf("campaign name = %q, want the legacy %q it was found under", res.CampaignName, legacy)
	}
}
