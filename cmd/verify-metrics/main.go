// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Command verify-metrics is a local, read-only operator tool for verifying a GATED read against
// a live ad account before the deployment flag that gates it is flipped:
//
//	go run ./cmd/verify-metrics -provider reddit-ads -project <slug|lf-system> \
//	    -campaign <platform campaign id> -window last_7_days [-raw]
//	go run ./cmd/verify-metrics -read audience -provider reddit-ads -project <slug> [-window last_30_days]
//	go run ./cmd/verify-metrics -read keywords -provider microsoft-ads -project <slug>
//	go run ./cmd/verify-metrics -read account-monitor -provider twitter-ads -project <slug> \
//	    -account-id <id> -days 30
//
// The Reddit and Microsoft reporting reads stay behind REDDIT_METRICS_ENABLED and
// MICROSOFT_METRICS_ENABLED, and X's account monitor and audience read behind
// TWITTER_METRICS_ENABLED, until someone has exercised the contract on a real account. This
// command is that exercise. It resolves the connection exactly as the service does (the
// connection repository, the system fallback, the AES-GCM decrypt) and builds the dispatcher with
// the service's own constructor. -read selects the read: `metrics` (the default) calls the
// dispatcher's ReadMetrics, the method the published metrics endpoint calls; `audience`,
// `keywords` and `account-monitor` call the service.Orchestrator method the published endpoint
// calls (gatedReads in reads.go names each). What it prints is that method's answer — the
// service-layer model the endpoint renders into its wire shape, plus, for the account monitor,
// the endpoint's rule-engine rows and action items — so it shows what the endpoint would serve
// once the flag is on, though not in the wire's field names, and without the monitor's totals.
//
// It is read-only by construction rather than by convention:
//   - the database session is opened READ ONLY (default_transaction_read_only), the dispatcher is
//     handed a connection reader exposing only Get and Disconnected, and the orchestrator a
//     campaign repository whose writes are refused;
//   - the encryptor it is handed refuses Encrypt;
//   - a report-backed read's saved-report state is kept in process memory, never the database;
//   - every upstream request passes a transport that refuses any method other than GET/HEAD,
//     except the named POSTs the selected read on the selected provider is made of (postAllowlist
//     in capture.go), before
//     the request leaves the process;
//   - write selectors (keyword targeting, keyword actions, negative keywords) are refused.
//
// Nothing secret is printed: every decrypted credential value and every Authorization header
// and token-exchange value it observes is scrubbed from all output, the service's own log lines
// included; token exchange bodies are withheld entirely, and URLs and JSON bodies are passed
// through pkg/redact plus a key-name redactor (see redact.go).
//
// The feature flag is forced on in THIS PROCESS's environment only; the deployment is untouched.
// Google Ads is refused: its reads are not gated, and its client is out of scope here.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

const (
	// systemProjectAlias is the operator-facing name for model.SystemProjectID, the reserved
	// LF-owned scope. The real key contains a colon; an alias saves quoting it in a shell.
	systemProjectAlias = "lf-system"

	defaultTimeout = 3 * time.Minute
	// defaultReportTimeout is a report-backed read's default deadline: Microsoft documents its
	// reports as completing "within minutes", and X's stats jobs likewise take minutes.
	defaultReportTimeout = 15 * time.Minute
	defaultPollInterval  = 20 * time.Second

	exitOK    = 0
	exitError = 1
	exitUsage = 2
	// exitNothingVerified is a gated read that never contacted the platform (an empty project
	// scope): the endpoint's answer, but no evidence about the contract, so not a pass.
	exitNothingVerified = 3
)

// gateFlags names the deployment flag each supported provider's metrics read is gated on, or ""
// when its campaign metrics read has no gate. Twitter's ReadMetrics is not itself gated, but
// TWITTER_METRICS_ENABLED gates the X reporting features built on the same stats contract, so it
// is forced on for an X run too, keeping the run's environment the one a flipped flag would give.
var gateFlags = map[model.Provider]string{
	model.ProviderRedditAds:    constants.EnvRedditMetricsEnabled,
	model.ProviderMicrosoftAds: constants.EnvMicrosoftMetricsEnabled,
	model.ProviderTwitterAds:   constants.EnvTwitterMetricsEnabled,
	model.ProviderLinkedInAds:  "",
	model.ProviderMetaAds:      "",
}

// supportedProviders lists gateFlags' keys in a stable, documented order for usage messages.
var supportedProviders = []model.Provider{
	model.ProviderRedditAds, model.ProviderMicrosoftAds, model.ProviderTwitterAds,
	model.ProviderLinkedInAds, model.ProviderMetaAds,
}

// options are the parsed command-line flags.
type options struct {
	read     readMode
	provider model.Provider
	project  string // the connection scope key, alias already resolved
	campaign string // the PLATFORM's campaign id (metrics only)
	window   model.MetricsWindow
	// accountID and days are the account monitor's payload (account_id, days).
	accountID string
	days      int
	raw       bool
	timeout   time.Duration
	// pollInterval is the wait between checks of a report-backed read's report.
	pollInterval time.Duration
	gated        gatedRead // the non-metrics read's description; zero for metrics
}

// usageError marks a flag problem, which exits 2 rather than 1.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return &usageError{msg: fmt.Sprintf(format, a...)} }

// readFlagUsage is the -read flag's help. It says why writes are absent, so an operator looking
// for a keyword-targeting check learns it is deliberate.
var readFlagUsage = fmt.Sprintf("which gated read to run (one of %v). metrics needs -campaign; audience and keywords read the "+
	"project's own campaigns over -window; account-monitor needs -account-id and -days. Writes — keyword targeting, "+
	"keyword actions, negative keywords — are NOT supported: this tool is read-only by construction (read-only database "+
	"session, encrypt-refusing encryptor, a transport that refuses every non-read request), so it verifies a read "+
	"contract without changing the live ad account", readModes)

// parseArgs parses and validates the flags. It never touches the database or the network.
func parseArgs(args []string, stderr io.Writer) (*options, error) {
	fs := flag.NewFlagSet("verify-metrics", flag.ContinueOnError)
	fs.SetOutput(stderr)
	read := fs.String("read", string(readMetrics), readFlagUsage)
	provider := fs.String("provider", "", fmt.Sprintf("provider to read (one of %v)", supportedProviders))
	project := fs.String("project", "", "project slug or UUID whose connection to use, exactly as the service keys it; "+
		systemProjectAlias+" selects the LF system account ("+model.SystemProjectID+"), for -read metrics only")
	campaign := fs.String("campaign", "", "-read metrics: the PLATFORM's campaign id (not this service's campaign UUID)")
	window := fs.String("window", "", "reporting window. -read metrics: any metrics window, default last_7_days; "+
		"audience/keywords: the endpoint's subset and default (last_30_days; last_7_days on twitter-ads)")
	accountID := fs.String("account-id", "", "-read account-monitor: the ad account to read (the endpoint's account_id)")
	days := fs.Int("days", 0, fmt.Sprintf("-read account-monitor: trailing days to read metrics over (%d-%d; the endpoint's days)",
		domain.MonitorDaysMin, domain.MonitorDaysMax))
	raw := fs.Bool("raw", false, "also print every upstream request/response exchange, redacted")
	timeout := fs.Duration("timeout", 0, fmt.Sprintf("overall deadline for the read (default %s; %s for a report-backed read)",
		defaultTimeout, defaultReportTimeout))
	pollInterval := fs.Duration("poll-interval", defaultPollInterval, "report-backed reads only: wait between checks of the platform's report")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		return nil, &usageError{msg: err.Error()}
	}
	// flag.Parse stops at the first non-flag word and silently ignores every flag after it, so a
	// stray word would run against a different campaign or window than the operator typed.
	if rest := fs.Args(); len(rest) > 0 {
		return nil, usagef("unexpected argument %q: this command takes flags only, and every flag after the first non-flag word is ignored", rest[0])
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	sel := strings.TrimSpace(*read)
	if _, isWrite := refusedWrites[sel]; isWrite {
		return nil, usagef("%s", writeRefusal(sel))
	}
	rm := readMode(sel)
	if !slices.Contains(readModes, rm) {
		return nil, usagef("-read %q is not supported (one of %v)", sel, readModes)
	}

	p := model.Provider(strings.TrimSpace(*provider))
	switch p {
	case "":
		return nil, usagef("-provider is required (one of %v)", supportedProviders)
	case model.ProviderGoogleAds:
		return nil, usagef("google-ads is not supported by verify-metrics: its metrics read is not behind a deployment flag, " +
			"so there is nothing to verify before flipping one, and its client is out of scope for this tool; " +
			"use the service's get-campaign-metrics endpoint instead")
	}
	if _, ok := gateFlags[p]; !ok {
		return nil, usagef("-provider %q is not supported (one of %v)", p, supportedProviders)
	}
	o := &options{read: rm, provider: p, raw: *raw, pollInterval: *pollInterval}
	if rm != readMetrics {
		g, err := lookupGatedRead(rm, p)
		if err != nil {
			return nil, err
		}
		o.gated = g
	}

	proj := strings.TrimSpace(*project)
	if proj == "" {
		return nil, usagef("-project is required (a project slug or UUID, or %s for the LF system account)", systemProjectAlias)
	}
	if proj == systemProjectAlias || proj == model.SystemProjectID {
		if rm != readMetrics {
			// The endpoint refuses the reserved scope (rejectSystemScope, a 404) before any read.
			return nil, usagef("-project %s is refused for -read %s: the endpoint refuses the LF system scope; name the project whose own campaigns to read", proj, rm)
		}
		proj = model.SystemProjectID
	}
	o.project = proj

	if err := o.parseReadFlags(set, *campaign, *window, *accountID, *days); err != nil {
		return nil, err
	}

	o.timeout = *timeout
	if !set["timeout"] {
		o.timeout = defaultTimeout
		if o.gated.reportBacked {
			o.timeout = defaultReportTimeout
		}
	}
	if o.timeout <= 0 {
		return nil, usagef("-timeout must be positive")
	}
	if set["poll-interval"] && !o.gated.reportBacked {
		return nil, usagef("-poll-interval applies only to a report-backed read (microsoft-ads audience, keywords and account-monitor; twitter-ads account-monitor)")
	}
	if o.pollInterval <= 0 {
		return nil, usagef("-poll-interval must be positive")
	}
	return o, nil
}

// parseReadFlags validates the flags that depend on -read, each with the endpoint payload's rule.
// A flag the selected read does not take is refused rather than ignored: silently dropping it
// would run a different read than the operator typed.
func (o *options) parseReadFlags(set map[string]bool, campaign, window, accountID string, days int) error {
	// refuse rejects each named flag the operator set; pairs are flag name, reason.
	refuse := func(pairs ...string) error {
		for i := 0; i+1 < len(pairs); i += 2 {
			if set[pairs[i]] {
				return usagef("-%s does not apply to -read %s: %s", pairs[i], o.read, pairs[i+1])
			}
		}
		return nil
	}
	const monitorOnly = "it is the account monitor's"
	switch o.read {
	case readMetrics:
		if err := refuse("account-id", monitorOnly, "days", monitorOnly); err != nil {
			return err
		}
		o.campaign = strings.TrimSpace(campaign)
		if o.campaign == "" {
			return usagef("-campaign is required (the platform's campaign id)")
		}
		o.window = model.MetricsWindowLast7Days
		if set["window"] {
			o.window = model.MetricsWindow(strings.TrimSpace(window))
		}
		if !model.IsValidMetricsWindow(o.window) {
			return usagef("-window %q is not a metrics window", window)
		}
	case readAudience, readKeywords:
		across := "it reads across the project's own campaigns, as the endpoint does"
		if err := refuse("campaign", across, "account-id", monitorOnly, "days", monitorOnly); err != nil {
			return err
		}
		o.window = o.gated.defaultWindow
		if set["window"] {
			o.window = model.MetricsWindow(strings.TrimSpace(window))
		}
		if ok, allowed := windowAllowed(o.gated, o.window); !ok {
			return usagef("-window %q is refused by %s: window must be one of: %s", window, o.gated.endpoint, allowed)
		}
	case readAccountMonitor:
		if err := refuse("campaign", "the monitor reads every campaign on -account-id", "window", "the monitor takes -days"); err != nil {
			return err
		}
		o.accountID = accountID // not trimmed: the endpoint's pattern admits no whitespace
		if o.accountID == "" {
			return usagef("-account-id is required (the endpoint's account_id)")
		}
		var verr error
		switch o.provider {
		case model.ProviderMicrosoftAds:
			verr = microsoft.ValidateMonitorAccountID(o.accountID)
		case model.ProviderTwitterAds:
			verr = twitter.ValidateMonitorAccountID(o.accountID)
		}
		if verr != nil {
			return usagef("-account-id: %v", verr)
		}
		if !set["days"] {
			return usagef("-days is required (%d-%d, the endpoint's days)", domain.MonitorDaysMin, domain.MonitorDaysMax)
		}
		if days < domain.MonitorDaysMin || days > domain.MonitorDaysMax {
			return usagef("-days: %v", domain.ErrMonitorDaysInvalid)
		}
		o.days = days
	}
	return nil
}

// app carries the injectable seams: production opens the real database, tests hand in fakes and
// point the platform clients at an httptest server.
type app struct {
	stdout, stderr io.Writer
	openBackend    func(ctx context.Context) (*backend, error)
	endpoints      endpoints
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := (&app{stdout: os.Stdout, stderr: os.Stderr, openBackend: openLiveBackend}).run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}

// run is the whole command, returning the process exit status.
func (a *app) run(ctx context.Context, args []string) int {
	opts, err := parseArgs(args, a.stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		_, _ = fmt.Fprintln(a.stderr, "verify-metrics:", err)
		return exitUsage
	}

	secrets := newSecretSet()
	out := &scrubWriter{w: a.stdout, secrets: secrets}
	errOut := &scrubWriter{w: a.stderr, secrets: secrets}
	// Every slog line the service or a dispatcher writes in this run goes through the scrubber
	// too, whichever read runs.
	prev := slog.Default()
	slog.SetDefault(scrubbedLogger(errOut))
	defer slog.SetDefault(prev)

	ctx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()

	be, err := a.openBackend(ctx)
	if err != nil {
		errOut.printf("verify-metrics: %v\n", err)
		return exitError
	}
	defer be.close()

	if flagName := gateFlags[opts.provider]; flagName != "" {
		// The dispatcher reads the gate per call from the environment; setting it here affects
		// this process only, which is the point: verify BEFORE the deployment's flag is flipped.
		if err := os.Setenv(flagName, "true"); err != nil {
			errOut.printf("verify-metrics: set %s for this process: %v\n", flagName, err)
			return exitError
		}
	}

	rec := newRecorder(secrets, opts.read, opts.provider)
	d, err := newDispatcher(opts.provider, be, secrets, rec.client(), a.endpoints)
	if err != nil {
		errOut.printf("verify-metrics: %v\n", err)
		return exitError
	}
	if opts.read != readMetrics {
		return a.runGated(ctx, opts, be, d, rec, out, errOut)
	}
	reader, ok := d.(metricsReader)
	if !ok {
		errOut.printf("verify-metrics: %s has no metrics read\n", opts.provider)
		return exitError
	}

	out.printf("provider: %s\nproject:  %s\ncampaign: %s\nwindow:   %s\n", opts.provider, opts.project, opts.campaign, opts.window)
	if flagName := gateFlags[opts.provider]; flagName != "" {
		out.printf("gate:     %s forced to \"true\" in this process only; the deployment's value is unchanged\n", flagName)
	}
	if os.Getenv(constants.EnvForceSystemAdsAccount) == "true" {
		// The flag redirects creates, not metrics reads: every metrics read resolves its account
		// from the row's recorded provenance, or the project-then-system fallback when that is
		// unknown (PR #362 review). -project lf-system is how to read under the system scope.
		out.printf("note:     %s=true in this environment, but metrics reads ignore it: they follow the campaign row's recorded account, or the project-then-system fallback; pass -project lf-system to read under the LF system scope\n",
			constants.EnvForceSystemAdsAccount)
	}

	campaign, found, err := lookupCampaign(ctx, be, opts)
	if err != nil {
		errOut.printf("verify-metrics: %v\n", err)
		return exitError
	}
	if found {
		out.printf("row:      found in this service's campaigns for the project; its recorded account provenance is checked as the service checks it\n")
	} else {
		out.printf("row:      not a campaign this service holds for the project; reading by platform id with no account-provenance check\n")
	}

	metrics, readErr := reader.ReadMetrics(ctx, opts.project, opts.provider, campaign, opts.window)

	if opts.raw {
		rec.print(out)
	}
	if readErr != nil {
		errOut.printf("verify-metrics: read failed: %v\n", readErr)
		return exitError
	}
	body, err := json.MarshalIndent(toResult(metrics), "", "  ")
	if err != nil {
		errOut.printf("verify-metrics: render result: %v\n", err)
		return exitError
	}
	out.printf("result:\n%s\n", body)
	return exitOK
}

// runGated runs an audience, keywords or account-monitor read through the orchestrator method
// its endpoint calls, and prints the result.
func (a *app) runGated(ctx context.Context, opts *options, be *backend, d service.PlatformDispatcher, rec *recorder, out, errOut *scrubWriter) int {
	out.printf("read:     %s\nprovider: %s\nproject:  %s\n", opts.read, opts.provider, opts.project)
	if opts.read == readAccountMonitor {
		out.printf("account:  %s\ndays:     %d\n", opts.accountID, opts.days)
	} else {
		out.printf("window:   %s\n", opts.window)
	}
	out.printf("endpoint: %s\nmethod:   %s (the method that endpoint calls)\n", opts.gated.endpoint, opts.gated.method)
	if flagName := gateFlags[opts.provider]; flagName != "" {
		out.printf("gate:     %s forced to \"true\" in this process only; the deployment's value is unchanged\n", flagName)
	}
	if opts.gated.reportBacked {
		out.printf("report:   kept in process memory, not the service's saved-report table; checked every %s until it finishes or -timeout\n", opts.pollInterval)
	}

	res, st, readErr := runGatedRead(ctx, opts, newReadOrchestrator(be, opts.provider, d), out)
	if opts.raw {
		rec.print(out)
	}
	if readErr != nil {
		errOut.printf("verify-metrics: read failed: %v\n", readErr)
		return exitError
	}
	if opts.gated.reportBacked && !st.served {
		if rec.count() > 0 {
			errOut.printf("verify-metrics: read failed: %v\n", errReportNeverServed)
			return exitError
		}
	}
	if err := printGatedResult(out, res); err != nil {
		errOut.printf("verify-metrics: %v\n", err)
		return exitError
	}
	if rec.count() == 0 {
		// Every read here answers an empty scope without contacting the platform, as the
		// endpoint does. That is not evidence about the contract, so it is not exit 0.
		errOut.printf("verify-metrics: no upstream request was made: the project has no campaigns on %s in this service (an empty scope), "+
			"so the endpoint answers empty without contacting the platform and nothing was verified; pick a project that has some\n", opts.provider)
		return exitNothingVerified
	}
	return exitOK
}

// lookupCampaign finds this service's row for the platform campaign, so the read carries the
// same provenance blob the service would pass and the dispatcher's account-match guard runs. A
// campaign this service does not hold (one created in the platform's own UI, or any campaign
// read under the system scope, which owns no rows) is read by platform id alone, which is the
// guard's documented "unknown provenance" case.
//
// One project can hold several rows for the same upstream id with DIFFERENT provenance (the id
// is unique only within an ad account, and legacy rows record none). Picking one would make the
// guard's verdict depend on row order, so that case is refused rather than guessed.
func lookupCampaign(ctx context.Context, be *backend, opts *options) (*model.Campaign, bool, error) {
	campaign := &model.Campaign{ProjectID: opts.project, Platform: opts.provider, PlatformCampaignID: opts.campaign}
	scopes, err := be.campaigns.ListProjectPlatformCampaignIDs(ctx, opts.project, opts.provider)
	if err != nil {
		return nil, false, fmt.Errorf("look up the project's %s campaigns: %w", opts.provider, err)
	}
	var matches []model.ProjectCampaignScope
	for _, s := range scopes {
		if s.PlatformCampaignID == opts.campaign {
			matches = append(matches, s)
		}
	}
	switch len(matches) {
	case 0:
		return campaign, false, nil
	case 1:
		campaign.Result = matches[0].Result
		return campaign, true, nil
	}
	return nil, false, fmt.Errorf("the project holds %d campaign rows for platform campaign %s with different account provenance; "+
		"the read would depend on which one was picked, so it is refused (read it through the service endpoint by campaign UUID)",
		len(matches), opts.campaign)
}

// result is the printed form of model.CampaignMetrics. Conversions stays a pointer so "not
// measured" (null) and a measured zero remain distinguishable, as they are in the model.
type result struct {
	CampaignID  string              `json:"campaign_id"`
	Window      model.MetricsWindow `json:"window"`
	Impressions int64               `json:"impressions"`
	Clicks      int64               `json:"clicks"`
	CostMicros  int64               `json:"cost_micros"`
	Ctr         float64             `json:"ctr"`
	Conversions *float64            `json:"conversions"`
}

func toResult(m *model.CampaignMetrics) *result {
	if m == nil {
		return nil
	}
	return &result{
		CampaignID: m.CampaignID, Window: m.Window, Impressions: m.Impressions, Clicks: m.Clicks,
		CostMicros: m.CostMicros, Ctr: m.Ctr, Conversions: m.Conversions,
	}
}
