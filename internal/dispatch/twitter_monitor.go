// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// The X account monitor is REPORT-BACKED, like Microsoft's (microsoft_monitor.go): it implements
// service.AccountReportReader, not service.AccountMetricsReader, for every `days` value. X's
// synchronous stats endpoint is capped at 7 days per request and shares a 250-requests-per-15-
// minutes budget across every foundation on the shared LF token, while the asynchronous
// stats-jobs API covers up to 90 days (see internal/platform/twitter/monitor.go). One path for
// every window, so a 7-day and a 30-day view never disagree about how their numbers were read.
//
// The trust boundary is the other monitors' exactly: the project's OWN connection only
// (resolveOwned, never the LF system-account fallback) and only the account that connection is
// bound to (requireTwitterManagedAccount). See the Trust boundary section of
// docs/knowledge/architecture/account-monitor-endpoints.md.
//
// It is gated behind TWITTER_METRICS_ENABLED (twitterMonitorEnabled), default off, on the same
// terms as Microsoft's and Reddit's: the stats-jobs contract — active_entities, job creation, the
// job-status read and the results file — follows X's public documentation but has NOT been
// exercised against a live X Ads account. The flag gates ONLY this monitor; X's per-campaign
// metrics read (ReadMetrics), a different and long-standing synchronous endpoint, is unaffected.
var _ service.AccountReportReader = (*TwitterDispatcher)(nil)

// twitterMonitorEnabled gates the report-backed X monitor, mirroring microsoftMonitorEnabled:
// only the exact value "true" enables it, read per call. Disabled, every AccountReportReader
// method answers the same 400 as a platform with no monitor, before any credential is resolved
// or any request made.
func twitterMonitorEnabled() error {
	if os.Getenv(constants.EnvTwitterMetricsEnabled) != "true" {
		return fmt.Errorf("x account monitor is disabled (%s is not \"true\") while the stats-jobs contract is unverified: %w",
			constants.EnvTwitterMetricsEnabled, domain.ErrAccountMetricsUnsupported)
	}
	return nil
}

// resolveTwitterMonitorClient validates the requested account id, resolves the project's own
// connection, refuses any account it is not bound to, and returns the CACHED client for that
// connection. Every AccountReportReader method starts here, so all three enforce the same
// boundary before any upstream call.
//
// The cached client, not a fresh one: SubmitAccountReport creates stats jobs through the
// client's write pacer, and that pacer only bounds the account's write rate if every caller for
// the connection shares it (see TwitterDispatcher.clients).
func (d *TwitterDispatcher) resolveTwitterMonitorClient(ctx context.Context, projectID string, platform model.Provider, accountID string) (*twitter.Client, error) {
	if err := twitterMonitorEnabled(); err != nil {
		return nil, err
	}
	// STRICT shape check, mirroring the design layer's Pattern + MaxLength exactly (no trim) —
	// the defense-in-depth every monitor dispatcher re-runs for a caller that bypasses Goa.
	if err := twitter.ValidateMonitorAccountID(accountID); err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrAccountIDMalformed, err)
	}
	res, err := d.creds.resolveOwned(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	creds, storedAccountID, err := validateTwitterConnection(projectID, res)
	if err != nil {
		return nil, err
	}
	if err := requireTwitterManagedAccount(projectID, storedAccountID, accountID); err != nil {
		return nil, err
	}
	return d.cachedTwitterClient(projectID, platform, res, creds, storedAccountID,
		strings.TrimSpace(res.providerConfig["funding_instrument_id"])), nil
}

// requireTwitterManagedAccount refuses a request naming an account other than the one the
// project's connection is bound to — the round-18 rule every monitor applies. An X connection
// holds exactly one account_id, and validateTwitterConnection has already refused an EMPTY one
// (ErrAccountNotSelected), so this is plain equality.
func requireTwitterManagedAccount(projectID, stored, requested string) error {
	if stored != requested {
		return fmt.Errorf("%w: x/twitter connection for project %s resolves to account %s, not the requested account %s",
			domain.ErrAccountNotManagedByConnection, projectID, stored, requested)
	}
	return nil
}

// ListAccountCampaigns implements service.AccountReportReader: the account's live campaigns with
// identity, status, budget and flight, metrics left zero for the orchestrator to fill from a
// report. A campaign whose budget or line-item flight could not be read is returned with
// FetchFailed — listed, but not evaluated, the treatment Google and Microsoft give an unparseable
// budget.
func (d *TwitterDispatcher) ListAccountCampaigns(ctx context.Context, projectID string, platform model.Provider, accountID string) ([]model.AccountCampaignMetrics, error) {
	client, err := d.resolveTwitterMonitorClient(ctx, projectID, platform, accountID)
	if err != nil {
		return nil, err
	}
	campaigns, err := client.ListAccountCampaigns(ctx)
	if err != nil {
		return nil, fmt.Errorf("list x ads account campaigns: %w", err)
	}
	out := make([]model.AccountCampaignMetrics, 0, len(campaigns))
	for _, c := range campaigns {
		var ranges []model.FlightRange
		if len(c.Flights) > 0 {
			ranges = make([]model.FlightRange, 0, len(c.Flights))
			for _, f := range c.Flights {
				ranges = append(ranges, model.FlightRange{StartDate: f.StartDate, EndDate: f.EndDate})
			}
		}
		out = append(out, model.AccountCampaignMetrics{
			PlatformCampaignID: c.ID,
			Name:               c.Name,
			Status:             c.Status,
			BudgetDay:          c.DailyBudget,
			TotalBudget:        c.TotalBudget,
			StartDate:          c.StartDate,
			EndDate:            c.EndDate,
			FlightRanges:       ranges,
			FetchFailed:        c.BudgetUnparseable || c.FlightUnparseable,
		})
	}
	return out, nil
}

// SubmitAccountReport implements service.AccountReportReader.
func (d *TwitterDispatcher) SubmitAccountReport(ctx context.Context, projectID string, platform model.Provider, accountID string, days int) (*model.AccountReportSubmission, error) {
	if err := validateMonitorDays(days); err != nil {
		return nil, err
	}
	client, err := d.resolveTwitterMonitorClient(ctx, projectID, platform, accountID)
	if err != nil {
		return nil, err
	}
	reportID, start, end, err := client.SubmitAccountCampaignReport(ctx, days)
	if errors.Is(err, twitter.ErrStatsJobBudget) {
		// Declined before any job was created; the orchestrator logs it as a skip.
		return nil, fmt.Errorf("submit x ads account report: %w: %w", domain.ErrAccountReportBudgetTooShort, err)
	}
	if errors.Is(err, twitter.ErrTooManyActiveCampaigns) {
		// Permanent while the account stays this busy: the orchestrator surfaces it to the
		// caller (409) instead of logging it and retrying on every read.
		return nil, fmt.Errorf("submit x ads account report: %w: %w", domain.ErrAccountTooManyActiveCampaigns, err)
	}
	if errors.Is(err, twitter.ErrReportWindowNotWholeHours) {
		// Permanent for the account's timezone; surfaced the same way. Nothing was requested.
		return nil, fmt.Errorf("submit x ads account report: %w: %w", domain.ErrAccountTimezoneUnsupported, err)
	}
	if err != nil {
		return nil, fmt.Errorf("submit x ads account report: %w", err)
	}
	return &model.AccountReportSubmission{ReportID: reportID, WindowStart: start, WindowEnd: end}, nil
}

// CheckAccountReport implements service.AccountReportReader: one job-status read, and the rows
// if every job has finished.
//
// Conversions stay nil on every row. X has no scalar conversions metric — it splits them across
// per-event-type objects under metric groups this read does not request (see ReadMetrics) — and
// with no conversion tag X reports nothing at all, which cannot be told apart from a measured
// zero. nil keeps the rules' no-conversions finding from firing on a guess.
func (d *TwitterDispatcher) CheckAccountReport(ctx context.Context, projectID string, platform model.Provider, accountID, reportID string) (*model.AccountReportCheck, error) {
	if strings.TrimSpace(reportID) == "" {
		return nil, errors.New("x ads account report check: report id is required")
	}
	client, err := d.resolveTwitterMonitorClient(ctx, projectID, platform, accountID)
	if err != nil {
		return nil, err
	}
	res, err := client.CheckAccountCampaignReport(ctx, reportID)
	if err != nil {
		return nil, fmt.Errorf("check x ads account report: %w", err)
	}
	out := &model.AccountReportCheck{Partial: res.Partial}
	switch res.Status {
	case twitter.AccountReportStatusPending:
		out.Status = model.AccountReportPending
	case twitter.AccountReportStatusFailed:
		out.Status = model.AccountReportFailed
	case twitter.AccountReportStatusSuccess:
		out.Status = model.AccountReportReady
		// Non-nil even when empty: a finished report with no rows is a real "nothing served".
		out.Rows = make([]model.AccountReportRow, 0, len(res.Rows))
		for _, r := range res.Rows {
			out.Rows = append(out.Rows, model.AccountReportRow{
				PlatformCampaignID: r.CampaignID,
				Spend:              float64(r.SpendMicro) / 1_000_000,
				Impressions:        r.Impressions,
				Clicks:             r.Clicks,
			})
		}
	default:
		return nil, fmt.Errorf("check x ads account report: unmapped status %q", res.Status)
	}
	return out, nil
}
