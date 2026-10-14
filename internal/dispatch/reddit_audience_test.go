// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// redditAudienceServer answers every report with one row for the campaign in the request's filter
// and records each request's path and filter.
type redditAudienceServer struct {
	mu      sync.Mutex
	paths   []string
	filters []string
	status  int
	api     *httptest.Server
	tok     *httptest.Server
}

func newRedditAudienceServer(t *testing.T) *redditAudienceServer {
	t.Helper()
	s := &redditAudienceServer{status: http.StatusOK}
	s.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Data struct {
				Filter     string   `json:"filter"`
				Breakdowns []string `json:"breakdowns"`
			} `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.paths = append(s.paths, r.Method+" "+r.URL.Path)
		s.filters = append(s.filters, req.Data.Filter)
		status := s.status
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"CANARY"}}`)
			return
		}
		id := strings.TrimPrefix(req.Data.Filter, "campaign:id==")
		key := strings.ToLower(strings.Join(req.Data.Breakdowns, ""))
		val := map[string]string{"gender": `"female"`, "country": `"US"`, "community": `"kubernetes"`}[key]
		_, _ = io.WriteString(w, `{"data":{"metrics":[{"campaign_id":"`+id+`","impressions":1000,"clicks":40,"spend":2500000,"`+key+`":`+val+`}]},"pagination":{}}`)
	}))
	t.Cleanup(s.api.Close)
	s.tok = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(s.tok.Close)
	return s
}

func (s *redditAudienceServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

func (s *redditAudienceServer) dispatcherWithBudget(repo connReader, b *reddit.ReportBudgets) *RedditDispatcher {
	return NewRedditDispatcher(repo, identityEncryptor{}, reddit.WithBaseURL(s.api.URL+"/api/v3"), reddit.WithTokenURL(s.tok.URL), reddit.WithReportBudgets(b))
}

func (s *redditAudienceServer) dispatcher(repo connReader) *RedditDispatcher {
	// A ledger of its own, so these tests neither wait on nor exhaust the process-wide one.
	return NewRedditDispatcher(repo, identityEncryptor{}, reddit.WithBaseURL(s.api.URL+"/api/v3"), reddit.WithTokenURL(s.tok.URL),
		reddit.WithReportBudgets(reddit.NewReportBudgets(time.Minute, 1<<20, 1<<20)))
}

func redditScope(ids ...string) []model.ProjectCampaignScope {
	out := make([]model.ProjectCampaignScope, 0, len(ids))
	for _, id := range ids {
		// Recorded under the fixture connection's account (activeRedditConn): an unrecorded one is
		// refused (TestReddit_ReadRedditAudienceInsights_UnknownProvenanceRefusesWholeRead).
		out = append(out, model.ProjectCampaignScope{PlatformCampaignID: id, Result: json.RawMessage(`{"accountId":"t2_acct"}`)})
	}
	return out
}

func TestReddit_ReadRedditAudienceInsights_MapsBucketsOnTheBoundAccount(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	s := newRedditAudienceServer(t)
	d := s.dispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)})

	ai, err := d.ReadRedditAudienceInsights(context.Background(), "proj", model.ProviderRedditAds, model.MetricsWindowThisMonth, redditScope("t3_c"))
	if err != nil {
		t.Fatalf("ReadRedditAudienceInsights: %v", err)
	}
	if ai.Window != model.MetricsWindowThisMonth {
		t.Errorf("window = %q", ai.Window)
	}
	want := []model.RedditAudienceBucket{
		{Dimension: model.RedditAudienceDimensionGender, Value: "female", Impressions: 1000, Clicks: 40, CostMicros: 2_500_000, Ctr: 0.04},
		{Dimension: model.RedditAudienceDimensionCountry, Value: "US", Impressions: 1000, Clicks: 40, CostMicros: 2_500_000, Ctr: 0.04},
		{Dimension: model.RedditAudienceDimensionCommunity, Value: "kubernetes", Impressions: 1000, Clicks: 40, CostMicros: 2_500_000, Ctr: 0.04},
	}
	if len(ai.Buckets) != len(want) {
		t.Fatalf("buckets = %+v", ai.Buckets)
	}
	for i := range want {
		if ai.Buckets[i] != want[i] {
			t.Errorf("bucket %d = %+v, want %+v", i, ai.Buckets[i], want[i])
		}
	}
	// The model tokens must be the client's; a drift would publish a value the design does not declare.
	if model.RedditAudienceDimensionGender != reddit.AudienceDimensionGender ||
		model.RedditAudienceDimensionCountry != reddit.AudienceDimensionCountry ||
		model.RedditAudienceDimensionCommunity != reddit.AudienceDimensionCommunity {
		t.Error("model and client dimension tokens diverged")
	}
	for _, p := range s.requests() {
		if p != "POST /api/v3/ad_accounts/t2_acct/reports" {
			t.Errorf("request %q, want only the connection's bound account", p)
		}
	}
}

// Every refusal below is decided before Reddit is contacted.
func TestReddit_ReadRedditAudienceInsights_RefusedBeforeAnyRequest(t *testing.T) {
	noAccount := activeRedditConn(goodRedditCreds)
	noAccount.AccountID = ""
	badAccount := activeRedditConn(goodRedditCreds)
	badAccount.AccountID = "t2/acct"
	tooMany := make([]string, reddit.MaxAudienceCampaigns+1)
	for i := range tooMany {
		tooMany[i] = "t3_" + strings.Repeat("c", i+1)
	}

	for _, tc := range []struct {
		name   string
		repo   connReader
		window model.MetricsWindow
		scope  []model.ProjectCampaignScope
		want   []error
	}{
		{"unsupported window", fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, model.MetricsWindowYesterday, redditScope("t3_c"), []error{domain.ErrMetricsWindowUnsupported}},
		{"no connection", fakeConnReader{err: domain.ErrNotFound}, model.MetricsWindowLast30Days, redditScope("t3_c"), []error{domain.ErrNotFound}},
		{"no account selected", fakeConnReader{conn: noAccount}, model.MetricsWindowLast30Days, redditScope("t3_c"), []error{domain.ErrConnectionNotUsable, domain.ErrAccountNotSelected}},
		{"unaddressable stored account", fakeConnReader{conn: badAccount}, model.MetricsWindowLast30Days, redditScope("t3_c"), []error{domain.ErrConnectionNotUsable}},
		{"incomplete credential", fakeConnReader{conn: activeRedditConn(`{}`)}, model.MetricsWindowLast30Days, redditScope("t3_c"), []error{domain.ErrConnectionNotUsable}},
		{"malformed stored campaign id", fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, model.MetricsWindowLast30Days, redditScope("t3_c", "t3_c,campaign:id==t3_x"), []error{domain.ErrAudienceScopeInvalid}},
		{"empty scope never widens", fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, model.MetricsWindowLast30Days, nil, []error{domain.ErrAudienceScopeInvalid}},
		{"scope too large", fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, model.MetricsWindowLast30Days, redditScope(tooMany...), []error{domain.ErrAudienceScopeTooLarge}},
		// The LF system row exists and is healthy, but the project has none of its own: the read
		// must NOT borrow the shared account (resolveOwned, not resolve).
		{"only the LF system connection", &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: activeRedditConn(goodRedditCreds)}}, model.MetricsWindowLast30Days, redditScope("t3_c"), []error{domain.ErrNotFound}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvRedditMetricsEnabled, "true")
			s := newRedditAudienceServer(t)
			ai, err := s.dispatcher(tc.repo).ReadRedditAudienceInsights(context.Background(), "proj", model.ProviderRedditAds, tc.window, tc.scope)
			for _, w := range tc.want {
				if !errors.Is(err, w) {
					t.Errorf("error = %v, want it to wrap %v", err, w)
				}
			}
			if ai != nil {
				t.Errorf("a refused read returned %+v", ai)
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("Reddit was contacted %d time(s)", n)
			}
		})
	}
}

// Off unless REDDIT_METRICS_ENABLED is exactly "true": the same 400 as an unsupported platform,
// before any credential is resolved.
func TestReddit_ReadRedditAudienceInsights_GatedOffByDefault(t *testing.T) {
	for _, v := range []string{"", "false", "TRUE", "1"} {
		t.Run("value="+v, func(t *testing.T) {
			t.Setenv(constants.EnvRedditMetricsEnabled, v)
			s := newRedditAudienceServer(t)
			d := s.dispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)})
			if err := d.AudienceEnabled(); !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
				t.Errorf("AudienceEnabled = %v, want ErrKeywordInsightsUnsupported", err)
			}
			_, err := d.ReadRedditAudienceInsights(context.Background(), "proj", model.ProviderRedditAds, model.MetricsWindowLast30Days, redditScope("t3_c"))
			if !errors.Is(err, domain.ErrKeywordInsightsUnsupported) {
				t.Errorf("error = %v, want ErrKeywordInsightsUnsupported", err)
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("Reddit was contacted %d time(s) while disabled", n)
			}
		})
	}
}

// ANY campaign created under a different ad account refuses the whole read (409) rather than
// returning the rest as though it were the project's whole picture.
func TestReddit_ReadRedditAudienceInsights_AccountMismatchRefusesWholeRead(t *testing.T) {
	foreign := model.ProjectCampaignScope{PlatformCampaignID: "t3_f", Result: json.RawMessage(`{"accountId":"t2_other"}`)}
	for name, scope := range map[string][]model.ProjectCampaignScope{
		"all foreign": {foreign},
		"partial":     append(redditScope("t3_c"), foreign),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(constants.EnvRedditMetricsEnabled, "true")
			s := newRedditAudienceServer(t)
			_, err := s.dispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}).
				ReadRedditAudienceInsights(context.Background(), "proj", model.ProviderRedditAds, model.MetricsWindowLast30Days, scope)
			if !errors.Is(err, domain.ErrCampaignAccountMismatch) || errors.Is(err, domain.ErrCampaignProvenanceUnknown) || errors.Is(err, domain.ErrCampaignProvenanceMixed) {
				t.Fatalf("error = %v, want ErrCampaignAccountMismatch alone", err)
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("Reddit was contacted %d time(s)", n)
			}
		})
	}
}

// A campaign that does not record its creating account refuses the whole read — alone or beside
// matching ones — before Reddit is contacted: its id cannot be proven to name the same campaign on
// the connected account.
func TestReddit_ReadRedditAudienceInsights_UnknownProvenanceRefusesWholeRead(t *testing.T) {
	legacy := []model.ProjectCampaignScope{{PlatformCampaignID: "t3_old"}, {PlatformCampaignID: "t3_blob", Result: json.RawMessage(`{"adGroupId":"t5_x"}`)}}
	for name, scope := range map[string][]model.ProjectCampaignScope{
		"all unrecorded":   legacy,
		"one beside known": append(redditScope("t3_a"), legacy[0]),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(constants.EnvRedditMetricsEnabled, "true")
			s := newRedditAudienceServer(t)
			_, err := s.dispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}).
				ReadRedditAudienceInsights(context.Background(), "proj", model.ProviderRedditAds, model.MetricsWindowLast30Days, scope)
			if !errors.Is(err, domain.ErrCampaignProvenanceUnknown) || !errors.Is(err, domain.ErrCampaignAccountMismatch) || errors.Is(err, domain.ErrCampaignProvenanceMixed) {
				t.Fatalf("error = %v, want ErrCampaignProvenanceUnknown joined with ErrCampaignAccountMismatch, not mixed", err)
			}
			if n := len(s.requests()); n != 0 {
				t.Errorf("Reddit was contacted %d time(s)", n)
			}
		})
	}
}

// A scope with BOTH a foreign-account campaign and one recording no account reports both faults —
// in either order — not just the first one found, so the service can name both remedies.
func TestReddit_ReadRedditAudienceInsights_ForeignAndUnknownReportsBoth(t *testing.T) {
	foreign := model.ProjectCampaignScope{PlatformCampaignID: "t3_f", Result: json.RawMessage(`{"accountId":"t2_other"}`)}
	unknown := model.ProjectCampaignScope{PlatformCampaignID: "t3_old"}
	for name, scope := range map[string][]model.ProjectCampaignScope{
		"foreign first": append(redditScope("t3_a"), foreign, unknown),
		"unknown first": {unknown, foreign},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(constants.EnvRedditMetricsEnabled, "true")
			s := newRedditAudienceServer(t)
			_, err := s.dispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}).
				ReadRedditAudienceInsights(context.Background(), "proj", model.ProviderRedditAds, model.MetricsWindowLast30Days, scope)
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

// Recorded, matching provenance is read: each owned campaign gets its own filtered reports.
func TestReddit_ReadRedditAudienceInsights_MatchingProvenanceReads(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	s := newRedditAudienceServer(t)
	if _, err := s.dispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}).
		ReadRedditAudienceInsights(context.Background(), "proj", model.ProviderRedditAds, model.MetricsWindowLast30Days, redditScope("t3_a", "t3_b")); err != nil {
		t.Fatalf("ReadRedditAudienceInsights: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	got := map[string]int{}
	for _, f := range s.filters {
		got[f]++
	}
	if len(got) != 2 || got["campaign:id==t3_a"] != 3 || got["campaign:id==t3_b"] != 3 {
		t.Errorf("filters = %v, want three reports for each owned campaign", got)
	}
}

// An upstream failure is returned unclassified (→ 503 at the service), never as a domain sentinel
// that would turn it into a 4xx.
func TestReddit_ReadRedditAudienceInsights_UpstreamFailureIsUnclassified(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	s := newRedditAudienceServer(t)
	s.status = http.StatusForbidden
	_, err := s.dispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}).
		ReadRedditAudienceInsights(context.Background(), "proj", model.ProviderRedditAds, model.MetricsWindowLast30Days, redditScope("t3_c"))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, sentinel := range []error{domain.ErrConnectionNotUsable, domain.ErrNotFound, domain.ErrCampaignAccountMismatch, domain.ErrAudienceScopeInvalid, domain.ErrKeywordInsightsUnsupported} {
		if errors.Is(err, sentinel) {
			t.Errorf("an upstream refusal must not carry %v", sentinel)
		}
	}
	if strings.Contains(err.Error(), "t2_acct") {
		t.Errorf("error leaks the account id: %v", err)
	}
}

// An exhausted reporting budget fails the read whole, unclassified (→ the fixed 503), and sends
// nothing.
func TestReddit_ReadRedditAudienceInsights_BudgetExhaustedIsUnclassified(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "true")
	s := newRedditAudienceServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ai, err := s.dispatcherWithBudget(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, reddit.NewReportBudgets(time.Minute, 100, 2)).
		ReadRedditAudienceInsights(ctx, "proj", model.ProviderRedditAds, model.MetricsWindowLast30Days, redditScope("t3_c"))
	if !errors.Is(err, reddit.ErrReportBudgetExhausted) || ai != nil {
		t.Fatalf("got (%+v, %v), want ErrReportBudgetExhausted and no result", ai, err)
	}
	for _, sentinel := range []error{domain.ErrConnectionNotUsable, domain.ErrNotFound, domain.ErrCampaignAccountMismatch, domain.ErrAudienceScopeInvalid, domain.ErrKeywordInsightsUnsupported, domain.ErrAudienceScopeTooLarge} {
		if errors.Is(err, sentinel) {
			t.Errorf("a budget refusal must not carry %v", sentinel)
		}
	}
	if n := len(s.requests()); n > 2 {
		t.Errorf("sent %d requests on a budget of 2", n)
	}
}
