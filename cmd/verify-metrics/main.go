// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Command verify-metrics is a local, read-only operator tool for verifying a GATED campaign
// metrics contract against a live ad account before the deployment flag is flipped:
//
//	go run ./cmd/verify-metrics -provider reddit-ads -project <slug|lf-system> \
//	    -campaign <platform campaign id> -window last_7_days [-raw]
//
// The Reddit and Microsoft metrics reads stay behind REDDIT_METRICS_ENABLED and
// MICROSOFT_METRICS_ENABLED (and X's reporting features behind TWITTER_METRICS_ENABLED) until
// someone has exercised the contract on a real account. This command is that exercise. It
// resolves the connection exactly as the service does (the connection repository, the system
// fallback, the AES-GCM decrypt), builds the dispatcher with the service's own constructor, and
// calls the dispatcher's ReadMetrics — the method the published metrics endpoint calls — so what
// it prints is what the endpoint would answer once the flag is on.
//
// It is read-only by construction rather than by convention:
//   - the database session is opened READ ONLY (default_transaction_read_only), and the
//     dispatcher is handed a connection reader exposing only Get and Disconnected;
//   - the encryptor it is handed refuses Encrypt;
//   - every upstream request passes a transport that refuses any method other than GET/HEAD,
//     except the POSTs a metrics read is made of (OAuth token exchanges, Reddit's report query,
//     Microsoft's report submit/poll), before the request leaves the process.
//
// Nothing secret is printed: every decrypted credential value and every Authorization header
// and token-exchange value it observes is scrubbed from all output, token exchange bodies are
// withheld entirely, and URLs and JSON bodies are passed through pkg/redact plus a key-name
// redactor (see redact.go).
//
// The feature flag is forced on in THIS PROCESS's environment only; the deployment is untouched.
// Google Ads is refused: its metrics read is not gated, and its client is out of scope here.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

const (
	// systemProjectAlias is the operator-facing name for model.SystemProjectID, the reserved
	// LF-owned scope. The real key contains a colon; an alias saves quoting it in a shell.
	systemProjectAlias = "lf-system"

	defaultTimeout = 3 * time.Minute

	exitOK    = 0
	exitError = 1
	exitUsage = 2
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
	provider model.Provider
	project  string // the connection scope key, alias already resolved
	campaign string // the PLATFORM's campaign id
	window   model.MetricsWindow
	raw      bool
	timeout  time.Duration
}

// usageError marks a flag problem, which exits 2 rather than 1.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return &usageError{msg: fmt.Sprintf(format, a...)} }

// parseArgs parses and validates the flags. It never touches the database or the network.
func parseArgs(args []string, stderr io.Writer) (*options, error) {
	fs := flag.NewFlagSet("verify-metrics", flag.ContinueOnError)
	fs.SetOutput(stderr)
	provider := fs.String("provider", "", fmt.Sprintf("provider to read (one of %v)", supportedProviders))
	project := fs.String("project", "", "project slug or UUID whose connection to use, exactly as the service keys it; "+
		systemProjectAlias+" selects the LF system account ("+model.SystemProjectID+")")
	campaign := fs.String("campaign", "", "the PLATFORM's campaign id (not this service's campaign UUID)")
	window := fs.String("window", string(model.MetricsWindowLast7Days), "metrics window (today, yesterday, last_7_days, last_14_days, last_30_days, this_month, last_month)")
	raw := fs.Bool("raw", false, "also print every upstream request/response exchange, redacted")
	timeout := fs.Duration("timeout", defaultTimeout, "overall deadline for the read")
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

	proj := strings.TrimSpace(*project)
	if proj == "" {
		return nil, usagef("-project is required (a project slug or UUID, or %s for the LF system account)", systemProjectAlias)
	}
	if proj == systemProjectAlias {
		proj = model.SystemProjectID
	}

	camp := strings.TrimSpace(*campaign)
	if camp == "" {
		return nil, usagef("-campaign is required (the platform's campaign id)")
	}

	w := model.MetricsWindow(strings.TrimSpace(*window))
	if !model.IsValidMetricsWindow(w) {
		return nil, usagef("-window %q is not a metrics window", *window)
	}
	if *timeout <= 0 {
		return nil, usagef("-timeout must be positive")
	}
	return &options{provider: p, project: proj, campaign: camp, window: w, raw: *raw, timeout: *timeout}, nil
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

	rec := newRecorder(secrets)
	reader, err := newMetricsReader(opts.provider, be, secrets, rec.client(), a.endpoints)
	if err != nil {
		errOut.printf("verify-metrics: %v\n", err)
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
