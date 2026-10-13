// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

func TestReddit_ReadRedditKeywordPerformance_MapsRowsOnTheBoundAccount(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	s := newRedditAudienceServer(t)
	kp, err := s.dispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}).
		ReadRedditKeywordPerformance(context.Background(), "proj", model.ProviderRedditAds, model.MetricsWindowLast7Days, redditScope("t3_c"))
	if err != nil {
		t.Fatalf("ReadRedditKeywordPerformance: %v", err)
	}
	want := model.RedditKeywordRow{CampaignID: "t3_c", Text: "service mesh", Impressions: 1000, Clicks: 40, CostMicros: 2_500_000, Ctr: 0.04}
	if kp.Window != model.MetricsWindowLast7Days || kp.Truncated || len(kp.Rows) != 1 || kp.Rows[0] != want {
		t.Fatalf("kp = %+v, want one row %+v", kp, want)
	}
	reqs := s.requests()
	if len(reqs) != 1 || reqs[0] != "POST /api/v3/ad_accounts/t2_acct/reports" {
		t.Errorf("requests = %v, want one report on the bound account", reqs)
	}
}

// Every refusal is decided before Reddit is contacted, and the scope refusals carry the KEYWORD
// read's sentinels, not the audience read's.
func TestReddit_ReadRedditKeywordPerformance_RefusedBeforeAnyRequest(t *testing.T) {
	tooMany := make([]string, reddit.MaxReportCampaigns+1)
	for i := range tooMany {
		tooMany[i] = "t3_" + strings.Repeat("k", i+1)
	}
	foreign := model.ProjectCampaignScope{PlatformCampaignID: "t3_f", Result: json.RawMessage(`{"accountId":"t2_other"}`)}
	for _, tc := range []struct {
		name    string
		repo    connReader
		window  model.MetricsWindow
		scope   []model.ProjectCampaignScope
		want    error
		notWant error
	}{
		{"unsupported window", fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, model.MetricsWindowLast14Days, redditScope("t3_c"), domain.ErrMetricsWindowUnsupported, nil},
		{"only the LF system connection", &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: activeRedditConn(goodRedditCreds)}}, model.MetricsWindowLast30Days, redditScope("t3_c"), domain.ErrNotFound, nil},
		{"malformed stored id", fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, model.MetricsWindowLast30Days, redditScope("t3_c", "bad id"), domain.ErrKeywordReportScopeInvalid, domain.ErrAudienceScopeInvalid},
		{"scope too large", fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, model.MetricsWindowLast30Days, redditScope(tooMany...), domain.ErrKeywordReportScopeTooLarge, domain.ErrAudienceScopeTooLarge},
		{"campaign under another account", fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, model.MetricsWindowLast30Days, append(redditScope("t3_c"), foreign), domain.ErrCampaignAccountMismatch, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvRedditMetricsEnabled, "true")
			s := newRedditAudienceServer(t)
			kp, err := s.dispatcher(tc.repo).ReadRedditKeywordPerformance(context.Background(), "proj", model.ProviderRedditAds, tc.window, tc.scope)
			if !errors.Is(err, tc.want) || kp != nil {
				t.Errorf("got (%+v, %v), want %v", kp, err, tc.want)
			}
			if tc.notWant != nil && errors.Is(err, tc.notWant) {
				t.Errorf("error %v carries the audience sentinel", err)
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("Reddit was contacted %d time(s)", n)
			}
		})
	}
}

func TestReddit_ReadRedditKeywordPerformance_GatedOffByDefault(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "")
	s := newRedditAudienceServer(t)
	d := s.dispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)})
	if err := d.KeywordsEnabled(); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
		t.Errorf("KeywordsEnabled = %v", err)
	}
	if _, err := d.ReadRedditKeywordPerformance(context.Background(), "proj", model.ProviderRedditAds, model.MetricsWindowLast30Days, redditScope("t3_c")); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
		t.Errorf("error = %v", err)
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("Reddit was contacted %d time(s) while disabled", n)
	}
}

func TestReddit_ReadRedditKeywordPerformance_UpstreamFailureIsUnclassified(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	s := newRedditAudienceServer(t)
	s.status = http.StatusTooManyRequests + 100 // 529: an unretried 5xx
	_, err := s.dispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}).
		ReadRedditKeywordPerformance(context.Background(), "proj", model.ProviderRedditAds, model.MetricsWindowLast30Days, redditScope("t3_c"))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, sentinel := range []error{domain.ErrConnectionNotUsable, domain.ErrNotFound, domain.ErrKeywordReportScopeInvalid, domain.ErrKeywordInsightsUnsupported} {
		if errors.Is(err, sentinel) {
			t.Errorf("an upstream failure must not carry %v", sentinel)
		}
	}
	if strings.Contains(err.Error(), "t2_acct") {
		t.Errorf("error leaks the account id: %v", err)
	}
}
