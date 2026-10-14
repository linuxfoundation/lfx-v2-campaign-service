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
	"time"

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

// Strict provenance applies to the keyword read too: an unrecorded creating account is refused
// before Reddit is contacted.
func TestReddit_ReadRedditKeywordPerformance_UnknownProvenanceRefused(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	s := newRedditAudienceServer(t)
	scope := append(redditScope("t3_c"), model.ProjectCampaignScope{PlatformCampaignID: "t3_old"})
	_, err := s.dispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}).
		ReadRedditKeywordPerformance(context.Background(), "proj", model.ProviderRedditAds, model.MetricsWindowLast30Days, scope)
	if !errors.Is(err, domain.ErrCampaignProvenanceUnknown) || !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Fatalf("error = %v, want provenance unknown joined with the mismatch", err)
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("Reddit was contacted %d time(s)", n)
	}
}

// A scope with BOTH a foreign-account campaign and one recording no account reports both faults
// on the keyword read too (ErrCampaignProvenanceMixed joined with both sentinels), in either order,
// before Reddit is contacted.
func TestReddit_ReadRedditKeywordPerformance_ForeignAndUnknownReportsBoth(t *testing.T) {
	foreign := model.ProjectCampaignScope{PlatformCampaignID: "t3_f", Result: json.RawMessage(`{"accountId":"t2_other"}`)}
	unknown := model.ProjectCampaignScope{PlatformCampaignID: "t3_old"}
	for name, scope := range map[string][]model.ProjectCampaignScope{
		"foreign first": append(redditScope("t3_c"), foreign, unknown),
		"unknown first": {unknown, foreign},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(constants.EnvRedditMetricsEnabled, "true")
			s := newRedditAudienceServer(t)
			_, err := s.dispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}).
				ReadRedditKeywordPerformance(context.Background(), "proj", model.ProviderRedditAds, model.MetricsWindowLast30Days, scope)
			for _, want := range []error{domain.ErrCampaignProvenanceMixed, domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch} {
				if !errors.Is(err, want) {
					t.Errorf("error = %v, want it to match %v", err, want)
				}
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("Reddit was contacted %d time(s)", n)
			}
		})
	}
}

// The keyword read spends from the same insight allowance as the audience read: on a shared ledger
// with room for the audience read's 3 attempts and nothing more, the keyword read is refused whole.
func TestReddit_ReadRedditKeywordPerformance_SharesTheInsightBudget(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	s := newRedditAudienceServer(t)
	d := s.dispatcherWithBudget(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, reddit.NewReportBudgets(time.Minute, 100, 3))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := d.ReadRedditAudienceInsights(ctx, "proj", model.ProviderRedditAds, model.MetricsWindowLast30Days, redditScope("t3_c")); err != nil {
		t.Fatalf("audience read: %v", err)
	}
	kp, err := d.ReadRedditKeywordPerformance(ctx, "proj", model.ProviderRedditAds, model.MetricsWindowLast30Days, redditScope("t3_c"))
	if !errors.Is(err, reddit.ErrReportBudgetExhausted) || kp != nil {
		t.Fatalf("got (%+v, %v), want ErrReportBudgetExhausted", kp, err)
	}
	if n := len(s.requests()); n != 3 {
		t.Errorf("sent %d requests, want only the audience read's 3", n)
	}
}

// A refresh inside the TTL spends no further reporting requests.
func TestReddit_ReadRedditKeywordPerformance_RefreshReusesTheRead(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	s := newRedditAudienceServer(t)
	d := s.dispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)})
	for i := 0; i < 3; i++ {
		if _, err := d.ReadRedditKeywordPerformance(context.Background(), "proj", model.ProviderRedditAds, model.MetricsWindowLast30Days, redditScope("t3_c")); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}
	if n := len(s.requests()); n != 1 {
		t.Errorf("reporting requests = %d, want 1", n)
	}
}

// The guard is keyed on the TRIMMED bound account — the id the scope was checked against — not the
// connection's raw value, so a padded account id caches under the same key as the clean one.
func TestReddit_ReadRedditKeywordPerformance_GuardKeyUsesTrimmedAccount(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	s := newRedditAudienceServer(t)
	c := activeRedditConn(goodRedditCreds)
	c.AccountID = "  " + c.AccountID + " "
	d := s.dispatcher(fakeConnReader{conn: c})
	if _, err := d.ReadRedditKeywordPerformance(context.Background(), "proj", model.ProviderRedditAds, model.MetricsWindowLast30Days, redditScope("t3_c")); err != nil {
		t.Fatalf("ReadRedditKeywordPerformance: %v", err)
	}
	want := redditReportKey("t2_acct", model.MetricsWindowLast30Days, d.keywords.now(), []string{"t3_c"})
	d.keywords.mu.Lock()
	_, ok := d.keywords.cache[want]
	keys := make([]string, 0, len(d.keywords.cache))
	for k := range d.keywords.cache {
		keys = append(keys, k)
	}
	d.keywords.mu.Unlock()
	if !ok {
		t.Errorf("guard cache keys = %q, want %q (the trimmed account)", keys, want)
	}
}
