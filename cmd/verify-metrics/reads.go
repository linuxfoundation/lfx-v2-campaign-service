// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service/rules"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// readMode is the -read selector: which gated read a run exercises.
type readMode string

const (
	readMetrics        readMode = "metrics"
	readAudience       readMode = "audience"
	readKeywords       readMode = "keywords"
	readAccountMonitor readMode = "account-monitor"
)

// readModes lists every selector in a stable, documented order for usage messages.
var readModes = []readMode{readMetrics, readAudience, readKeywords, readAccountMonitor}

// refusedWrites are selectors an operator might reasonably try that name a WRITE. They are
// refused outright, with the reason, rather than answered "unknown": this command is read-only by
// construction (a READ ONLY database session, an encryptor that refuses Encrypt, and a transport
// that refuses every non-read request), so a write could not succeed here — and a tool that
// verifies a contract by changing a live ad account is not a verification tool.
var refusedWrites = map[string]string{
	"keyword-targeting": "keyword targeting writes replace a live ad group's keyword list",
	"keyword-actions":   "keyword actions pause or remove live keywords",
	"negative-keywords": "negative keyword writes change a live campaign's targeting",
}

// writeRefusal is the usage text for a refused write selector.
func writeRefusal(sel string) string {
	return fmt.Sprintf("-read %s is refused: it is a write (%s). verify-metrics is read-only by construction — "+
		"the database session is READ ONLY, the encryptor refuses Encrypt, and the transport refuses every request "+
		"that is not a read — so no write is offered; verify a write path through the service in a test ad account",
		sel, refusedWrites[sel])
}

// gatedRead is one (read, provider) pair this command verifies: the published endpoint, the
// service method that endpoint calls (and this command calls), and the endpoint's payload rules.
type gatedRead struct {
	endpoint string
	method   string
	// windows is the endpoint's window subset and defaultWindow its default when the caller
	// sends none; both nil/"" for the account monitor, which takes days instead.
	windows       []model.MetricsWindow
	defaultWindow model.MetricsWindow
	// reportBacked marks a read whose metrics come from an asynchronous platform report kept in
	// a saved-report store between requests: a run polls it in-process until it finishes.
	reportBacked bool
}

// The endpoints' window subsets, restated from internal/service (redditAudienceWindows,
// microsoftKeywordWindows, twitterAudienceWindows) and design/ (the *WindowEnum the generated
// decoder enforces), so a window the endpoint refuses is refused here as the same usage error.
var (
	redditReportWindows = []model.MetricsWindow{
		model.MetricsWindowLast30Days, model.MetricsWindowLast7Days, model.MetricsWindowLastMonth,
		model.MetricsWindowThisMonth, model.MetricsWindowToday,
	}
	microsoftReportWindows = redditReportWindows
	twitterAudienceWindows = []model.MetricsWindow{
		model.MetricsWindowLast7Days, model.MetricsWindowToday, model.MetricsWindowYesterday,
	}
)

// gatedReads is every non-metrics read this command verifies, keyed by selector and provider.
var gatedReads = map[readMode]map[model.Provider]gatedRead{
	readAudience: {
		model.ProviderRedditAds: {
			endpoint: "GET /projects/{project_id}/reddit-ads/audience", method: "Orchestrator.ReadRedditAudienceInsights",
			windows: redditReportWindows, defaultWindow: model.MetricsWindowLast30Days,
		},
		model.ProviderMicrosoftAds: {
			endpoint: "GET /projects/{project_id}/microsoft-ads/audience", method: "Orchestrator.ReadReportedAudience",
			windows: microsoftReportWindows, defaultWindow: model.MetricsWindowLast30Days, reportBacked: true,
		},
		model.ProviderTwitterAds: {
			endpoint: "GET /projects/{project_id}/twitter-ads/audience", method: "Orchestrator.ReadTwitterAudienceInsights",
			windows: twitterAudienceWindows, defaultWindow: model.MetricsWindowLast7Days,
		},
	},
	readKeywords: {
		model.ProviderRedditAds: {
			endpoint: "GET /projects/{project_id}/reddit-ads/keywords", method: "Orchestrator.ReadRedditKeywordPerformance",
			windows: redditReportWindows, defaultWindow: model.MetricsWindowLast30Days,
		},
		model.ProviderMicrosoftAds: {
			endpoint: "GET /projects/{project_id}/microsoft-ads/keywords", method: "Orchestrator.ReadReportedKeywordPerformance",
			windows: microsoftReportWindows, defaultWindow: model.MetricsWindowLast30Days, reportBacked: true,
		},
	},
	readAccountMonitor: {
		model.ProviderMicrosoftAds: {
			endpoint: "GET /projects/{project_id}/connection-microsoft-ads/account-monitor", method: "Orchestrator.ReadReportedAccountCampaigns",
			reportBacked: true,
		},
		model.ProviderTwitterAds: {
			endpoint: "GET /projects/{project_id}/connection-twitter-ads/account-monitor", method: "Orchestrator.ReadReportedAccountCampaigns",
			reportBacked: true,
		},
	},
}

// notGatedReasons explains the combinations a reader would expect to find gated but that are
// not, so the refusal names where to read them instead.
var notGatedReasons = map[readMode]map[model.Provider]string{
	readKeywords: {
		model.ProviderTwitterAds: "x has no gated keyword read: " + constants.EnvTwitterMetricsEnabled +
			" gates only the x account monitor and audience read; x keyword targeting is a campaign setting, not a report, and its write is refused",
	},
	readAccountMonitor: {
		model.ProviderRedditAds: "the reddit account monitor is a live read that is not behind " + constants.EnvRedditMetricsEnabled +
			", so there is nothing to verify before flipping it; use the service's monitor-reddit-ads-account endpoint",
	},
}

// lookupGatedRead returns the gated read for (read, provider), or a usage error saying why the
// combination is not one this command verifies.
func lookupGatedRead(read readMode, p model.Provider) (gatedRead, error) {
	if g, ok := gatedReads[read][p]; ok {
		return g, nil
	}
	if why, ok := notGatedReasons[read][p]; ok {
		return gatedRead{}, usagef("-read %s -provider %s is not supported: %s", read, p, why)
	}
	providers := make([]string, 0, len(gatedReads[read]))
	for q := range gatedReads[read] {
		providers = append(providers, string(q))
	}
	sort.Strings(providers)
	return gatedRead{}, usagef("-read %s -provider %s is not supported: %s has no %s read behind a deployment flag, so there is nothing to verify "+
		"before flipping one (gated %s reads: %s)", read, p, p, read, read, strings.Join(providers, ", "))
}

// windowAllowed reports whether w is in the endpoint's subset, and renders the subset for the
// usage error, sorted as the endpoint's 400 message sorts it.
func windowAllowed(g gatedRead, w model.MetricsWindow) (bool, string) {
	names := make([]string, 0, len(g.windows))
	ok := false
	for _, x := range g.windows {
		names = append(names, string(x))
		ok = ok || x == w
	}
	sort.Strings(names)
	return ok, strings.Join(names, ", ")
}

// reportStores is the in-process saved-report state a run's orchestrator is wired with.
type reportStores struct {
	accounts *memAccountReports
	insights *memInsightReports
}

// newReadOrchestrator builds a service.Orchestrator on the read-only campaign reader and the
// one dispatcher this run uses, wired with in-process saved-report stores (memstore.go) in place
// of the service's database tables.
func newReadOrchestrator(be *backend, p model.Provider, d service.PlatformDispatcher) *service.Orchestrator {
	orch := service.NewOrchestrator(&readOnlyCampaigns{CampaignReader: be.campaigns}, nil, map[model.Provider]service.PlatformDispatcher{p: d})
	stores := reportStores{accounts: newMemAccountReports(), insights: newMemInsightReports()}
	orch.SetAccountReportStore(stores.accounts)
	orch.SetKeywordReportStore(stores.insights)
	orch.SetAudienceReportStore(stores.insights)
	return orch
}

// reportState is what the poll loop needs to know about one report-backed read's answer.
type reportState struct {
	pending bool // a report is still building
	served  bool // a finished report is being served (metrics_as_of is set)
}

// errReportNeverServed is a report-backed read that ended with neither a finished report nor
// one building, after contacting the platform: the submission failed or was refused.
var errReportNeverServed = errors.New("no finished report was served and none is building: the platform refused or failed the report " +
	"(see the logged warnings above, and -raw for the exchanges)")

// pollReported calls read until a finished report is served or none is building, sleeping
// interval between calls — the service does the same across requests, keeping the report in its
// store; this run keeps it in memory and asks again. Each call is the endpoint's whole method, so
// the answer it finally returns is the one the endpoint would serve on the request after the
// report finished.
func pollReported[T any](ctx context.Context, out *scrubWriter, interval time.Duration, read func(context.Context) (*T, reportState, error)) (*T, reportState, error) {
	for attempt := 1; ; attempt++ {
		res, st, err := read(ctx)
		// Served ends the wait even while pending: a report collected past the freshness limit
		// is served AND a replacement submitted at once, and the served one is the evidence.
		if err != nil || !st.pending || st.served {
			return res, st, err
		}
		out.printf("poll:     attempt %d: report still building; checking again in %s\n", attempt, interval)
		select {
		case <-ctx.Done():
			return nil, st, fmt.Errorf("the report was still building when the run's -timeout expired; rerun with a longer -timeout: %w", ctx.Err())
		case <-time.After(interval):
		}
	}
}

// runGatedRead runs a non-metrics read and returns the value to print.
func runGatedRead(ctx context.Context, opts *options, orch *service.Orchestrator, out *scrubWriter) (any, reportState, error) {
	p, proj := opts.provider, opts.project
	switch opts.read {
	case readAudience:
		switch p {
		case model.ProviderRedditAds:
			r, err := orch.ReadRedditAudienceInsights(ctx, proj, p, opts.window)
			return r, reportState{served: true}, err
		case model.ProviderTwitterAds:
			r, err := orch.ReadTwitterAudienceInsights(ctx, proj, p, opts.window)
			return r, reportState{served: true}, err
		case model.ProviderMicrosoftAds:
			return asAny(pollReported(ctx, out, opts.pollInterval, func(ctx context.Context) (*model.ReportedAudienceRead, reportState, error) {
				r, err := orch.ReadReportedAudience(ctx, proj, p, opts.window)
				if err != nil {
					return nil, reportState{}, err
				}
				return r, reportState{pending: r.MetricsPending, served: r.MetricsAsOf != nil}, nil
			}))
		}
	case readKeywords:
		switch p {
		case model.ProviderRedditAds:
			r, err := orch.ReadRedditKeywordPerformance(ctx, proj, p, opts.window)
			return r, reportState{served: true}, err
		case model.ProviderMicrosoftAds:
			return asAny(pollReported(ctx, out, opts.pollInterval, func(ctx context.Context) (*model.ReportedKeywordRead, reportState, error) {
				r, err := orch.ReadReportedKeywordPerformance(ctx, proj, p, opts.window)
				if err != nil {
					return nil, reportState{}, err
				}
				return r, reportState{pending: r.MetricsPending, served: r.MetricsAsOf != nil}, nil
			}))
		}
	case readAccountMonitor:
		read, st, err := pollReported(ctx, out, opts.pollInterval, func(ctx context.Context) (*model.ReportedAccountRead, reportState, error) {
			r, err := orch.ReadReportedAccountCampaigns(ctx, proj, p, opts.accountID, opts.days)
			if err != nil {
				return nil, reportState{}, err
			}
			return r, reportState{pending: r.MetricsPending, served: r.MetricsAsOf != nil}, nil
		})
		if err != nil {
			return nil, st, err
		}
		return evaluateMonitor(p, opts.days, read), st, nil
	}
	return nil, reportState{}, fmt.Errorf("-read %s -provider %s has no read wired", opts.read, p)
}

func asAny[T any](v *T, st reportState, err error) (any, reportState, error) {
	if v == nil {
		return nil, st, err
	}
	return v, st, err
}

// monitorResult is the account monitor as the endpoint builds it: the orchestrator's read, and
// the rows and action items the endpoint's rule engine derives from it.
type monitorResult struct {
	Read        *model.ReportedAccountRead
	Rows        []model.AccountMonitorRow
	ActionItems []model.AccountMonitorActionItem
}

// evaluateMonitor applies the rule engine the endpoint applies (MonitorMicrosoftAdsAccount,
// MonitorTwitterAdsAccount).
func evaluateMonitor(p model.Provider, days int, read *model.ReportedAccountRead) *monitorResult {
	out := &monitorResult{Read: read}
	switch p {
	case model.ProviderMicrosoftAds:
		out.Rows, out.ActionItems = rules.EvaluateMicrosoftMonitor(read.Rows, days)
	case model.ProviderTwitterAds:
		out.Rows, out.ActionItems = rules.EvaluateTwitterMonitor(read.Rows, read.MetricsWindowStart, read.MetricsWindowEnd)
	}
	return out
}

// printGatedResult renders a non-metrics result through the same body redaction -raw uses —
// secret-named keys withheld, URLs reduced, every recorded secret scrubbed — before the final
// output scrub.
func printGatedResult(out *scrubWriter, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("render result: %w", err)
	}
	out.printf("result:\n%s\n", redactBody(body, out.secrets))
	return nil
}

// scrubbedLogger routes the service's own slog output (the report-backed reads log a failed
// check or submission and carry on) through the scrubber, so a logged error cannot print a
// secret the rest of the output withholds.
//
// Each attribute (the message included) is scrubbed BEFORE the handler serialises it, as well as
// the finished line after: TextHandler quotes a value with a quote or backslash in it, so an
// error that already quoted a credential (a %q in a parse error) reaches the output escaped
// twice, a form the after-the-fact scrub cannot match.
func scrubbedLogger(w *scrubWriter) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: slog.LevelInfo,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			v := a.Value.Resolve()
			switch v.Kind() {
			case slog.KindString:
				return slog.String(a.Key, w.secrets.scrub(v.String()))
			case slog.KindAny:
				return slog.String(a.Key, w.secrets.scrub(fmt.Sprint(v.Any())))
			}
			return a
		},
	}))
}
