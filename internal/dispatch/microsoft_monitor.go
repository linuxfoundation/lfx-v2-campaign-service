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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// The Microsoft account monitor is REPORT-BACKED: it implements service.AccountReportReader,
// not service.AccountMetricsReader. Microsoft's delivery metrics come only from its asynchronous
// Reporting service, which takes minutes, so the orchestrator lists campaigns live, keeps the
// last finished report, and drives the next one across requests
// (Orchestrator.ReadReportedAccountCampaigns). These three methods are the stateless platform
// half of that.
//
// The trust boundary is the other four monitors' exactly: the project's OWN connection only
// (resolveOwned, never the LF system-account fallback) and only the account that connection is
// bound to (requireMicrosoftManagedAccount). See the Trust boundary section of
// docs/knowledge/architecture/account-monitor-endpoints.md.
var _ service.AccountReportReader = (*MicrosoftDispatcher)(nil)

// microsoftMonitorEnabled gates the report-backed monitor on the same flag as ReadMetrics, for
// the same reason: the Reporting request and response shapes follow Microsoft's published v13
// contract but have not yet been exercised against a live account (see the UNVERIFIED CONTRACT
// banner in internal/platform/microsoft/metrics.go). Disabled, the monitor answers the same 400
// as a platform with no monitor at all.
func microsoftMonitorEnabled() error {
	if os.Getenv(constants.EnvMicrosoftMetricsEnabled) != "true" {
		return fmt.Errorf("microsoft account monitor is disabled (%s is not \"true\") while the reporting contract is unverified: %w",
			constants.EnvMicrosoftMetricsEnabled, domain.ErrAccountMetricsUnsupported)
	}
	return nil
}

// resolveMicrosoftMonitorClient validates the requested account id, resolves the project's own
// connection, refuses any account it is not bound to, and returns a client for that account.
// Every AccountReportReader method starts here, so all three enforce the same boundary.
func (d *MicrosoftDispatcher) resolveMicrosoftMonitorClient(ctx context.Context, projectID string, platform model.Provider, accountID string) (*microsoft.Client, error) {
	if err := microsoftMonitorEnabled(); err != nil {
		return nil, err
	}
	// STRICT shape check, mirroring the design layer's Pattern exactly (no trim, no leading zero,
	// at most 18 digits) — the defense-in-depth every monitor dispatcher re-runs for a caller
	// that bypasses Goa. Not microsoft.ValidateAccountID, which trims and admits a 19th digit
	// for the create path, and would therefore accept ids the HTTP layer refuses.
	if err := microsoft.ValidateMonitorAccountID(accountID); err != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrAccountIDMalformed, err)
	}
	res, err := d.creds.resolveOwned(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	creds, storedAccountID, err := validateMicrosoftConnection(projectID, res)
	if err != nil {
		return nil, err
	}
	if err := requireMicrosoftManagedAccount(projectID, storedAccountID, accountID); err != nil {
		return nil, err
	}
	return d.cachedMicrosoftClient(projectID, platform, res, creds, storedAccountID), nil
}

// requireMicrosoftManagedAccount refuses a request naming an account other than the one the
// project's connection is bound to. A Microsoft connection holds exactly one account_id, so any
// other account is a request mismatch — the round-18 rule Reddit, LinkedIn and Meta already
// apply. validateMicrosoftConnection has already refused an EMPTY stored account
// (ErrAccountNotSelected), so this is plain equality.
func requireMicrosoftManagedAccount(projectID, stored, requested string) error {
	if stored != requested {
		return fmt.Errorf("%w: microsoft connection for project %s resolves to account %s, not the requested account %s",
			domain.ErrAccountNotManagedByConnection, projectID, stored, requested)
	}
	return nil
}

// ListAccountCampaigns implements service.AccountReportReader: the account's live campaigns with
// identity, status and budget, metrics left zero for the orchestrator to fill from a report.
//
// A shared-budget campaign gets PacingUnknown: its budget belongs to a pool spanning campaigns,
// so this campaign's share is not knowable and EvaluateMicrosoftMonitor must neither pace it nor
// call its zero BudgetDay a placeholder. An unparseable budget gets FetchFailed, the same
// treatment Google gives its budget-unparseable rows — returned, but not evaluated.
func (d *MicrosoftDispatcher) ListAccountCampaigns(ctx context.Context, projectID string, platform model.Provider, accountID string) ([]model.AccountCampaignMetrics, error) {
	client, err := d.resolveMicrosoftMonitorClient(ctx, projectID, platform, accountID)
	if err != nil {
		return nil, err
	}
	campaigns, err := client.ListAccountCampaigns(ctx)
	if err != nil {
		return nil, fmt.Errorf("list microsoft account campaigns: %w", err)
	}
	out := make([]model.AccountCampaignMetrics, 0, len(campaigns))
	for _, c := range campaigns {
		row := model.AccountCampaignMetrics{
			PlatformCampaignID: c.ID,
			Name:               c.Name,
			Status:             c.Status,
			BudgetDay:          c.DailyBudget,
			PacingUnknown:      c.SharedBudget,
			FetchFailed:        c.BudgetUnparseable,
			IsSearchChannel:    microsoftIsSearchCampaign(c.CampaignType),
		}
		out = append(out, row)
	}
	return out, nil
}

// microsoftIsSearchCampaign reports whether a campaign's CampaignType is Search. Microsoft
// renders the type as a flags value, so it is matched as a token rather than by equality.
func microsoftIsSearchCampaign(campaignType string) bool {
	for _, t := range strings.FieldsFunc(campaignType, func(r rune) bool { return r == ' ' || r == ',' }) {
		if strings.EqualFold(t, "Search") {
			return true
		}
	}
	return false
}

// SubmitAccountReport implements service.AccountReportReader.
func (d *MicrosoftDispatcher) SubmitAccountReport(ctx context.Context, projectID string, platform model.Provider, accountID string, days int) (*model.AccountReportSubmission, error) {
	if err := validateMonitorDays(days); err != nil {
		return nil, err
	}
	client, err := d.resolveMicrosoftMonitorClient(ctx, projectID, platform, accountID)
	if err != nil {
		return nil, err
	}
	reportID, start, end, err := client.SubmitAccountCampaignReport(ctx, days)
	if err != nil {
		return nil, fmt.Errorf("submit microsoft account report: %w", err)
	}
	return &model.AccountReportSubmission{ReportID: reportID, WindowStart: start, WindowEnd: end}, nil
}

// CheckAccountReport implements service.AccountReportReader: one Poll, and the rows if the report
// has finished.
func (d *MicrosoftDispatcher) CheckAccountReport(ctx context.Context, projectID string, platform model.Provider, accountID, reportID string) (*model.AccountReportCheck, error) {
	if strings.TrimSpace(reportID) == "" {
		return nil, errors.New("microsoft account report check: report id is required")
	}
	client, err := d.resolveMicrosoftMonitorClient(ctx, projectID, platform, accountID)
	if err != nil {
		return nil, err
	}
	res, err := client.CheckAccountCampaignReport(ctx, reportID)
	if err != nil {
		return nil, fmt.Errorf("check microsoft account report: %w", err)
	}
	out := &model.AccountReportCheck{Partial: res.Partial}
	switch res.Status {
	case microsoft.AccountReportStatusPending:
		out.Status = model.AccountReportPending
	case microsoft.AccountReportStatusError:
		out.Status = model.AccountReportFailed
	case microsoft.AccountReportStatusSuccess:
		out.Status = model.AccountReportReady
		// Non-nil even when empty: a finished account report with no rows is a real "nothing
		// served", and the store keeps it as [] rather than "no report".
		out.Rows = make([]model.AccountReportRow, 0, len(res.Rows))
		for _, r := range res.Rows {
			out.Rows = append(out.Rows, model.AccountReportRow{
				PlatformCampaignID: r.CampaignID,
				Spend:              r.Spend,
				Impressions:        r.Impressions,
				Clicks:             r.Clicks,
				Conversions:        r.Conversions,
			})
		}
	default:
		// The client refuses an unrecognized Microsoft status itself; this guards a status
		// constant added to the client later without a mapping here.
		return nil, fmt.Errorf("check microsoft account report: unmapped status %q", res.Status)
	}
	return out, nil
}
