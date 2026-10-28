// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/crypto"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

func TestParseArgs_ReadSelector(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string // substring; "" means success
		check   func(t *testing.T, o *options)
	}{
		{name: "default is metrics", args: []string{"-provider", "reddit-ads", "-project", "p", "-campaign", "t3_c"}, check: func(t *testing.T, o *options) {
			if o.read != readMetrics || o.window != model.MetricsWindowLast7Days || o.timeout != defaultTimeout {
				t.Errorf("unexpected options %+v", o)
			}
		}},
		{name: "explicit metrics", args: []string{"-read", "metrics", "-provider", "meta-ads", "-project", "p", "-campaign", "1"}},
		{name: "reddit audience defaults to the endpoint's window", args: []string{"-read", "audience", "-provider", "reddit-ads", "-project", "p"}, check: func(t *testing.T, o *options) {
			if o.read != readAudience || o.window != model.MetricsWindowLast30Days || o.gated.method != "Orchestrator.ReadRedditAudienceInsights" || o.timeout != defaultTimeout {
				t.Errorf("unexpected options %+v", o)
			}
		}},
		{name: "twitter audience defaults to last_7_days", args: []string{"-read", "audience", "-provider", "twitter-ads", "-project", "p"}, check: func(t *testing.T, o *options) {
			if o.window != model.MetricsWindowLast7Days || o.gated.method != "Orchestrator.ReadTwitterAudienceInsights" {
				t.Errorf("unexpected options %+v", o)
			}
		}},
		{name: "microsoft audience is report-backed", args: []string{"-read", "audience", "-provider", "microsoft-ads", "-project", "p", "-window", "this_month", "-poll-interval", "5s"}, check: func(t *testing.T, o *options) {
			if !o.gated.reportBacked || o.window != model.MetricsWindowThisMonth || o.pollInterval != 5*time.Second || o.timeout != defaultReportTimeout {
				t.Errorf("unexpected options %+v", o)
			}
		}},
		{name: "reddit keywords", args: []string{"-read", "keywords", "-provider", "reddit-ads", "-project", "p", "-window", "today"}, check: func(t *testing.T, o *options) {
			if o.gated.method != "Orchestrator.ReadRedditKeywordPerformance" || o.window != model.MetricsWindowToday {
				t.Errorf("unexpected options %+v", o)
			}
		}},
		{name: "microsoft keywords", args: []string{"-read", "keywords", "-provider", "microsoft-ads", "-project", "p"}, check: func(t *testing.T, o *options) {
			if o.gated.method != "Orchestrator.ReadReportedKeywordPerformance" || !o.gated.reportBacked {
				t.Errorf("unexpected options %+v", o)
			}
		}},
		{name: "twitter account monitor", args: []string{"-read", "account-monitor", "-provider", "twitter-ads", "-project", "p", "-account-id", "18ce54d4x5t", "-days", "30", "-timeout", "2m"}, check: func(t *testing.T, o *options) {
			if o.accountID != "18ce54d4x5t" || o.days != 30 || o.timeout != 2*time.Minute || o.gated.method != "Orchestrator.ReadReportedAccountCampaigns" {
				t.Errorf("unexpected options %+v", o)
			}
		}},
		{name: "microsoft account monitor", args: []string{"-read", "account-monitor", "-provider", "microsoft-ads", "-project", "p", "-account-id", "187654321", "-days", "7"}},

		{name: "unknown selector", args: []string{"-read", "bids", "-provider", "reddit-ads", "-project", "p"}, wantErr: `-read "bids" is not supported`},
		{name: "keyword targeting write refused", args: []string{"-read", "keyword-targeting", "-provider", "reddit-ads", "-project", "p"}, wantErr: "read-only by construction"},
		{name: "keyword actions write refused", args: []string{"-read", "keyword-actions", "-provider", "microsoft-ads", "-project", "p"}, wantErr: "it is a write"},
		{name: "negative keywords write refused", args: []string{"-read", "negative-keywords", "-provider", "twitter-ads", "-project", "p"}, wantErr: "it is a write"},
		{name: "twitter has no gated keyword read", args: []string{"-read", "keywords", "-provider", "twitter-ads", "-project", "p"}, wantErr: "x has no gated keyword read"},
		{name: "reddit monitor is not gated", args: []string{"-read", "account-monitor", "-provider", "reddit-ads", "-project", "p", "-account-id", "t2_a", "-days", "7"}, wantErr: "not behind " + constants.EnvRedditMetricsEnabled},
		{name: "linkedin audience is not gated", args: []string{"-read", "audience", "-provider", "linkedin-ads", "-project", "p"}, wantErr: "no audience read behind a deployment flag"},
		{name: "google-ads still refused", args: []string{"-read", "audience", "-provider", "google-ads", "-project", "p"}, wantErr: "google-ads is not supported"},
		{name: "system scope refused off metrics", args: []string{"-read", "audience", "-provider", "reddit-ads", "-project", "lf-system"}, wantErr: "refuses the LF system scope"},
		{name: "system scope key refused off metrics", args: []string{"-read", "keywords", "-provider", "microsoft-ads", "-project", model.SystemProjectID}, wantErr: "refuses the LF system scope"},
		{name: "campaign refused for audience", args: []string{"-read", "audience", "-provider", "reddit-ads", "-project", "p", "-campaign", "t3_c"}, wantErr: "-campaign does not apply to -read audience"},
		{name: "account-id refused for keywords", args: []string{"-read", "keywords", "-provider", "reddit-ads", "-project", "p", "-account-id", "t2_a"}, wantErr: "-account-id does not apply"},
		{name: "days refused for metrics", args: []string{"-provider", "reddit-ads", "-project", "p", "-campaign", "t3_c", "-days", "7"}, wantErr: "-days does not apply to -read metrics"},
		{name: "reddit audience refuses yesterday", args: []string{"-read", "audience", "-provider", "reddit-ads", "-project", "p", "-window", "yesterday"}, wantErr: "window must be one of: last_30_days, last_7_days, last_month, this_month, today"},
		{name: "microsoft keywords refuse last_14_days", args: []string{"-read", "keywords", "-provider", "microsoft-ads", "-project", "p", "-window", "last_14_days"}, wantErr: "window must be one of"},
		{name: "twitter audience refuses last_30_days", args: []string{"-read", "audience", "-provider", "twitter-ads", "-project", "p", "-window", "last_30_days"}, wantErr: "window must be one of: last_7_days, today, yesterday"},
		{name: "monitor needs account-id", args: []string{"-read", "account-monitor", "-provider", "twitter-ads", "-project", "p", "-days", "7"}, wantErr: "-account-id is required"},
		{name: "monitor needs days", args: []string{"-read", "account-monitor", "-provider", "twitter-ads", "-project", "p", "-account-id", "abc"}, wantErr: "-days is required"},
		{name: "monitor days below range", args: []string{"-read", "account-monitor", "-provider", "twitter-ads", "-project", "p", "-account-id", "abc", "-days", "6"}, wantErr: "days must be between 7 and 90"},
		{name: "monitor days above range", args: []string{"-read", "account-monitor", "-provider", "microsoft-ads", "-project", "p", "-account-id", "1", "-days", "91"}, wantErr: "days must be between 7 and 90"},
		{name: "microsoft account id shape", args: []string{"-read", "account-monitor", "-provider", "microsoft-ads", "-project", "p", "-account-id", "0123", "-days", "7"}, wantErr: "-account-id"},
		{name: "twitter account id shape", args: []string{"-read", "account-monitor", "-provider", "twitter-ads", "-project", "p", "-account-id", "ab-c", "-days", "7"}, wantErr: "-account-id"},
		{name: "monitor refuses window", args: []string{"-read", "account-monitor", "-provider", "twitter-ads", "-project", "p", "-account-id", "abc", "-days", "7", "-window", "today"}, wantErr: "the monitor takes -days"},
		{name: "poll interval on a live read", args: []string{"-read", "audience", "-provider", "reddit-ads", "-project", "p", "-poll-interval", "5s"}, wantErr: "-poll-interval applies only"},
		{name: "poll interval must be positive", args: []string{"-read", "keywords", "-provider", "microsoft-ads", "-project", "p", "-poll-interval", "0s"}, wantErr: "-poll-interval must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, err := parseArgs(tc.args, io.Discard)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				var ue *usageError
				if !errors.As(err, &ue) {
					t.Errorf("err %T is not a usage error, so it would not exit 2", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, o)
			}
		})
	}
}

// The usage text says why keyword-targeting writes are absent.
func TestUsage_SaysWritesAreNotSupported(t *testing.T) {
	var stderr bytes.Buffer
	if _, err := parseArgs([]string{"-h"}, &stderr); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, want flag.ErrHelp", err)
	}
	for _, want := range []string{"keyword targeting", "NOT supported", "read-only by construction", "account-monitor"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("usage missing %q:\n%s", want, stderr.String())
		}
	}
}

// A write selector exits 2 before any database or network is touched.
func TestRun_WriteSelectorRefusedBeforeAnyBackend(t *testing.T) {
	var stderr bytes.Buffer
	a := &app{stdout: io.Discard, stderr: &stderr, openBackend: func(context.Context) (*backend, error) {
		t.Fatal("the backend must not be opened for a write selector")
		return nil, nil
	}}
	if code := a.run(context.Background(), []string{"-read", "keyword-targeting", "-provider", "reddit-ads", "-project", "p"}); code != exitUsage {
		t.Errorf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "is refused: it is a write") {
		t.Errorf("stderr = %q, want the write refusal", stderr.String())
	}
}

// Each read admits only the POSTs it is made of: a POST another read needs is refused.
func TestAllowedRequest_PerRead(t *testing.T) {
	const (
		redditReport = "/api/v3/ad_accounts/t2_a/reports"
		msSubmit     = "/Reporting/v13/GenerateReport/Submit"
		msPoll       = "/Reporting/v13/GenerateReport/Poll"
		msQuery      = "/CampaignManagement/v13/Campaigns/QueryByAccountId"
		msAccount    = "/CustomerManagement/v13/Account/Query"
		xStatsJob    = "/12/stats/jobs/accounts/18ce54d4x5t"
		token        = "/api/v1/access_token"
	)
	type allowCase struct {
		read         readMode
		method, path string
		want         bool
	}
	cases := []allowCase{
		// metrics: unchanged.
		{readMetrics, http.MethodPost, redditReport, true},
		{readMetrics, http.MethodPost, msSubmit, true},
		{readMetrics, http.MethodPost, xStatsJob, false},
		{readMetrics, http.MethodPost, msQuery, false},
		// audience: Reddit's report, Microsoft Reporting, X stats jobs.
		{readAudience, http.MethodPost, token, true},
		{readAudience, http.MethodPost, redditReport, true},
		{readAudience, http.MethodPost, msSubmit, true},
		{readAudience, http.MethodPost, msPoll, true},
		{readAudience, http.MethodPost, xStatsJob, true},
		{readAudience, http.MethodPost, msQuery, false},
		{readAudience, http.MethodPost, msAccount, false},
		// keywords: Reddit's report, Microsoft Reporting — no X job, no campaign query.
		{readKeywords, http.MethodPost, redditReport, true},
		{readKeywords, http.MethodPost, msSubmit, true},
		{readKeywords, http.MethodPost, xStatsJob, false},
		{readKeywords, http.MethodPost, msQuery, false},
		// account-monitor: Microsoft's campaign and account queries plus Reporting; X jobs.
		{readAccountMonitor, http.MethodPost, msQuery, true},
		{readAccountMonitor, http.MethodPost, msAccount, true},
		{readAccountMonitor, http.MethodPost, msSubmit, true},
		{readAccountMonitor, http.MethodPost, msPoll, true},
		{readAccountMonitor, http.MethodPost, xStatsJob, true},
		{readAccountMonitor, http.MethodPost, redditReport, false},
		{readAccountMonitor, http.MethodGet, "/12/stats/jobs/accounts/18ce54d4x5t", true},
	}
	// Writes every read refuses: creates, keyword-targeting and negative-keyword writes, deletes.
	for _, r := range readModes {
		cases = append(cases,
			allowCase{r, http.MethodPost, "/CampaignManagement/v13/Campaigns", false},
			allowCase{r, http.MethodPost, "/CampaignManagement/v13/Keywords", false},
			allowCase{r, http.MethodPatch, "/api/v3/ad_groups/t4_g", false},
			allowCase{r, http.MethodPost, "/12/accounts/abc/targeting_criteria", false},
			allowCase{r, http.MethodDelete, "/12/stats/jobs/accounts/abc", false},
			allowCase{r, http.MethodPut, "/12/accounts/abc/campaigns/x", false},
		)
	}
	for _, tc := range cases {
		if got := allowedRequest(tc.read, tc.method, tc.path); got != tc.want {
			t.Errorf("allowedRequest(%s, %s %s) = %v, want %v", tc.read, tc.method, tc.path, got, tc.want)
		}
	}
	for _, r := range readModes {
		if len(postAllowlist[r]) == 0 {
			t.Errorf("read %s has no POST allowlist entry; every read needs at least the token exchange", r)
		}
	}
}

// The service's own log lines go through the scrubber.
func TestScrubbedLogger_ScrubsRecordedSecrets(t *testing.T) {
	const cred = "logged-secret-0123456789"
	secrets := newSecretSet()
	secrets.add(cred)
	var buf bytes.Buffer
	scrubbedLogger(&scrubWriter{w: &buf, secrets: secrets}).Warn("report check failed", "error", errors.New("upstream said "+cred))
	if strings.Contains(buf.String(), cred) || !strings.Contains(buf.String(), redacted) {
		t.Errorf("log line = %q, want the secret scrubbed", buf.String())
	}
}

// The gated result path redacts too: secret-named keys withheld, recorded secrets scrubbed.
func TestPrintGatedResult_Redacts(t *testing.T) {
	const cred = "result-secret-0123456789"
	secrets := newSecretSet()
	secrets.add(cred)
	var buf bytes.Buffer
	err := printGatedResult(&scrubWriter{w: &buf, secrets: secrets}, map[string]any{
		"Name": "campaign " + cred, "access_token": "tok-abcdefghij", "Impressions": 9007199254740993,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{cred, "tok-abcdefghij"} {
		if strings.Contains(buf.String(), leak) {
			t.Errorf("result leaked %q:\n%s", leak, buf.String())
		}
	}
	if !strings.Contains(buf.String(), "9007199254740993") {
		t.Errorf("result lost precision:\n%s", buf.String())
	}
}

func TestPollReported(t *testing.T) {
	var out bytes.Buffer
	w := &scrubWriter{w: &out, secrets: newSecretSet()}
	calls := 0
	got, st, err := pollReported(context.Background(), w, time.Millisecond, func(context.Context) (*int, reportState, error) {
		calls++
		v := calls
		return &v, reportState{pending: calls < 3, served: calls >= 3}, nil
	})
	if err != nil || *got != 3 || !st.served || calls != 3 {
		t.Fatalf("got=%v st=%+v err=%v calls=%d, want the third answer", got, st, err, calls)
	}
	if strings.Count(out.String(), "report still building") != 2 {
		t.Errorf("want two poll lines:\n%s", out.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, _, err = pollReported(ctx, w, time.Hour, func(context.Context) (*int, reportState, error) {
		return new(int), reportState{pending: true}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "still building") {
		t.Errorf("err = %v, want the still-building timeout", err)
	}

	boom := errors.New("boom")
	if _, _, err = pollReported(context.Background(), w, time.Millisecond, func(context.Context) (*int, reportState, error) {
		return nil, reportState{}, boom
	}); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the read's error", err)
	}
}

func TestMemReports_FollowTheRepositoryContract(t *testing.T) {
	ctx := context.Background()
	s := newMemAccountReports()
	key := model.AccountReportKey{ProjectID: "p", Platform: model.ProviderMicrosoftAds, AccountID: "1", Days: 7}
	if _, err := s.GetAccountReport(ctx, key); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("empty store: err = %v, want ErrNotFound", err)
	}
	if ok, _ := s.MarkAccountReportPending(ctx, key, model.PendingAccountReport{ReportID: "r1"}); !ok {
		t.Fatal("first mark must apply")
	}
	if ok, _ := s.MarkAccountReportPending(ctx, key, model.PendingAccountReport{ReportID: "r2"}); ok {
		t.Fatal("a second mark while one is pending must not apply")
	}
	if ok, _ := s.CompleteAccountReport(ctx, key, model.ReadyAccountReport{ReportID: "r2"}); ok {
		t.Fatal("completing a report that is not the pending one must not apply")
	}
	if ok, _ := s.CompleteAccountReport(ctx, key, model.ReadyAccountReport{ReportID: "r1"}); !ok {
		t.Fatal("completing the pending report must apply")
	}
	snap, _ := s.GetAccountReport(ctx, key)
	if snap.Pending != nil || snap.Ready == nil || snap.Ready.ReportID != "r1" {
		t.Fatalf("snapshot = %+v, want r1 ready and nothing pending", snap)
	}
	_, _ = s.MarkAccountReportPending(ctx, key, model.PendingAccountReport{ReportID: "r3"})
	if ok, _ := s.FailAccountReport(ctx, key, "r3", "failed", time.Now()); !ok {
		t.Fatal("failing the pending report must apply")
	}
	snap, _ = s.GetAccountReport(ctx, key)
	if snap.Pending != nil || snap.Ready.ReportID != "r1" || snap.LastFailure != "failed" {
		t.Fatalf("snapshot = %+v, want r1 still ready and the failure recorded", snap)
	}

	ins := newMemInsightReports()
	ik := model.InsightReportKey{ProjectID: "p", Platform: model.ProviderMicrosoftAds, AccountID: "1", Window: model.MetricsWindowToday}
	_, _ = ins.MarkKeywordReportPending(ctx, ik, model.PendingInsightReport{ReportID: "k1"})
	// The kinds are separate: a keyword report pending does not block or answer an audience one.
	if _, err := ins.GetAudienceReport(ctx, ik); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("audience kind saw the keyword kind's row: err = %v", err)
	}
	if ok, _ := ins.MarkAudienceReportPending(ctx, ik, model.PendingInsightReport{ReportID: "a1"}); !ok {
		t.Error("an audience mark must not be blocked by a keyword report")
	}
}

func TestReadOnlyCampaigns_RefusesEveryWrite(t *testing.T) {
	ctx := context.Background()
	r := &readOnlyCampaigns{}
	var none domain.CampaignLockToken
	_, e1 := r.UpsertCampaign(ctx, nil, nil)
	_, e2 := r.AdoptCampaign(ctx, nil, 0, nil)
	_, e3 := r.ReplaceCampaign(ctx, nil, 0, none, nil)
	_, e4 := r.VerifyClaimedVersion(ctx, "", "", "", 0, none)
	_, _, e5 := r.ClaimCampaignVersion(ctx, "", "", "", 0)
	e6 := r.ReleaseCampaignLock(ctx, none)
	e7 := r.DeleteCampaign(ctx, "", "", "", 0, nil, nil)
	for i, err := range []error{e1, e2, e3, e4, e5, e6, e7} {
		if !errors.Is(err, errReadOnly) {
			t.Errorf("write %d: err = %v, want errReadOnly", i+1, err)
		}
	}
}

// --- end-to-end against a fake Reddit: the audience read ------------------------------------

// fakeRedditBreakdowns answers each report query with one row for the breakdown it asks for,
// echoing the credentials back as the worst case for the redactor.
func fakeRedditBreakdowns(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var writes, reports atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/access_token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fakeAccessToken, "token_type": "bearer", "expires_in": 3600})
	})
	mux.HandleFunc("/api/v3/ad_accounts/t2_acct/reports", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writes.Add(1)
		}
		reports.Add(1)
		var req struct {
			Data struct {
				Filter     string   `json:"filter"`
				Breakdowns []string `json:"breakdowns"`
			} `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		id := strings.TrimPrefix(req.Data.Filter, "campaign:id==")
		key := strings.ToLower(strings.Join(req.Data.Breakdowns, ""))
		val := map[string]string{"gender": `"female"`, "country": `"US"`, "community": `"` + fakeClientSecret + `"`, "keyword": `"service mesh"`}[key]
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"metrics":[{"campaign_id":"`+id+`","impressions":1000,"clicks":40,"spend":2500000,"`+key+`":`+val+`}]},"pagination":{}}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writes.Add(1)
		t.Errorf("unexpected upstream request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &writes, &reports
}

func TestRun_RedditAudienceAgainstFakePlatform_IgnoresFlagAndRedacts(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "false")
	srv, writes, reports := fakeRedditBreakdowns(t)
	var stdout, stderr bytes.Buffer
	code := redditTestApp(t, srv, &stdout, &stderr).run(context.Background(),
		[]string{"-read", "audience", "-provider", "reddit-ads", "-project", "my-project", "-raw"})
	all := stdout.String() + stderr.String()
	if testing.Verbose() {
		t.Log(all)
	}
	if code != exitOK {
		t.Fatalf("exit = %d, output:\n%s", code, all)
	}
	assertNoSecrets(t, all)
	for _, want := range []string{
		"read:     audience", "window:   last_30_days", "Orchestrator.ReadRedditAudienceInsights",
		"GET /projects/{project_id}/reddit-ads/audience", constants.EnvRedditMetricsEnabled + ` forced to "true"`,
		`"Dimension": "gender"`, `"Value": "female"`, `"Impressions": 1000`, "/ad_accounts/t2_acct/reports -> 200",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
	if reports.Load() == 0 || writes.Load() != 0 {
		t.Errorf("reports=%d writes=%d, want report queries and no writes", reports.Load(), writes.Load())
	}
}

func TestRun_RedditKeywordsAgainstFakePlatform(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "")
	srv, writes, _ := fakeRedditBreakdowns(t)
	var stdout, stderr bytes.Buffer
	code := redditTestApp(t, srv, &stdout, &stderr).run(context.Background(),
		[]string{"-read", "keywords", "-provider", "reddit-ads", "-project", "my-project", "-window", "last_7_days"})
	all := stdout.String() + stderr.String()
	if code != exitOK {
		t.Fatalf("exit = %d, output:\n%s", code, all)
	}
	assertNoSecrets(t, all)
	if !strings.Contains(stdout.String(), `"Text": "service mesh"`) || writes.Load() != 0 {
		t.Errorf("want the keyword row and no writes:\n%s", all)
	}
}

// An empty scope is answered without contacting the platform, and the output says nothing was
// verified rather than passing an empty result off as a verified one.
func TestRun_EmptyScopeSaysNothingWasVerified(t *testing.T) {
	t.Setenv(constants.EnvRedditMetricsEnabled, "")
	srv, _, reports := fakeRedditBreakdowns(t)
	var stdout, stderr bytes.Buffer
	a := redditTestApp(t, srv, &stdout, &stderr)
	be, _ := a.openBackend(context.Background())
	be.campaigns = &fakeScopes{}
	code := a.run(context.Background(), []string{"-read", "audience", "-provider", "reddit-ads", "-project", "my-project"})
	if code != exitOK {
		t.Fatalf("exit = %d, output:\n%s%s", code, stdout.String(), stderr.String())
	}
	if reports.Load() != 0 || !strings.Contains(stdout.String(), "nothing was verified") {
		t.Errorf("reports=%d, stdout:\n%s", reports.Load(), stdout.String())
	}
}

// --- end-to-end against a fake Microsoft: the report-backed audience read --------------------

const (
	fakeMSDeveloperToken = "fake-developer-token-0123456"
	fakeMSDownloadSig    = "FAKEDOWNLOADSIG0123456789"
)

// fakeMicrosoftReporting answers Submit with a report id, the first Poll with Pending and every
// later one with Success and a signed download URL, and serves the report zip.
func fakeMicrosoftReporting(t *testing.T) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var polls, refused atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"access_token":"`+fakeAccessToken+`","refresh_token":"`+fakeRefreshToken+`","expires_in":3600,"token_type":"Bearer"}`)
		case strings.HasSuffix(r.URL.Path, "/GenerateReport/Submit"):
			_, _ = io.WriteString(w, `{"ReportRequestId":"r-1"}`)
		case strings.HasSuffix(r.URL.Path, "/GenerateReport/Poll"):
			if polls.Add(1) == 1 {
				_, _ = io.WriteString(w, `{"ReportRequestStatus":{"Status":"Pending"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"ReportRequestStatus":{"Status":"Success","ReportDownloadUrl":"`+srv.URL+`/download/r.zip?sv=2020&sig=`+fakeMSDownloadSig+`"}}`)
		case r.URL.Path == "/download/r.zip":
			var zbuf bytes.Buffer
			zw := zip.NewWriter(&zbuf)
			f, _ := zw.Create("AgeGenderAudienceReport.csv")
			_, _ = io.WriteString(f, "\"CampaignId\",\"AgeGroup\",\"Gender\",\"Impressions\",\"Clicks\",\"Spend\"\n\"111\",\"25-34\",\"Female\",\"100\",\"5\",\"2.50\"\n")
			_ = zw.Close()
			_, _ = w.Write(zbuf.Bytes())
		default:
			refused.Add(1)
			t.Errorf("unexpected upstream request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &polls, &refused
}

func microsoftTestApp(t *testing.T, srv *httptest.Server, stdout, stderr io.Writer) *app {
	t.Helper()
	enc, err := crypto.NewAESGCM(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := enc.Encrypt([]byte(`{"ClientID":"fake-client-id-000","ClientSecret":"` + fakeClientSecret +
		`","DeveloperToken":"` + fakeMSDeveloperToken + `","RefreshToken":"` + fakeRefreshToken + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	conn := &model.Connection{
		Provider: model.ProviderMicrosoftAds, AccountID: "1234567", EncryptedCredentials: ciphertext,
		Status: model.StatusActive, ProviderConfig: map[string]string{"customer_id": "9999999"},
	}
	be := &backend{
		conns:     &fakeConns{conn: conn},
		campaigns: &fakeScopes{scopes: []model.ProjectCampaignScope{{PlatformCampaignID: "111", Result: json.RawMessage(`{"accountId":"1234567"}`)}}},
		enc:       enc,
		close:     func() {},
	}
	return &app{
		stdout: stdout, stderr: stderr,
		openBackend: func(context.Context) (*backend, error) { return be, nil },
		endpoints:   endpoints{api: srv.URL, token: srv.URL + "/common/oauth2/v2.0/token", reporting: srv.URL, customer: srv.URL},
	}
}

// The report-backed read polls in process until the report finishes, then prints what the
// endpoint would serve — with the flag off in the environment, and nothing secret printed.
func TestRun_MicrosoftAudiencePollsUntilTheReportFinishes(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "false")
	srv, polls, refused := fakeMicrosoftReporting(t)
	var stdout, stderr bytes.Buffer
	code := microsoftTestApp(t, srv, &stdout, &stderr).run(context.Background(),
		[]string{"-read", "audience", "-provider", "microsoft-ads", "-project", "my-project", "-window", "last_7_days", "-poll-interval", "10ms", "-raw"})
	all := stdout.String() + stderr.String()
	if testing.Verbose() {
		t.Log(all)
	}
	if code != exitOK {
		t.Fatalf("exit = %d, output:\n%s", code, all)
	}
	assertNoSecrets(t, all)
	for _, leak := range []string{fakeMSDeveloperToken, fakeMSDownloadSig} {
		if strings.Contains(all, leak) {
			t.Errorf("output leaked %q", leak)
		}
	}
	for _, want := range []string{
		"Orchestrator.ReadReportedAudience", "attempt 1: report still building", `"AgeGroup": "25-34"`,
		`"Impressions": 100`, `"MetricsPending": false`, "zip entry AgeGenderAudienceReport.csv",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
	if polls.Load() < 2 || refused.Load() != 0 {
		t.Errorf("polls=%d refused=%d, want at least two polls and nothing unexpected", polls.Load(), refused.Load())
	}
}
