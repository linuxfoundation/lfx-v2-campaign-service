// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// The Microsoft keyword read is REPORT-BACKED: it implements service.KeywordReportReader, not
// service.KeywordInsightsReader. Keyword performance comes only from the asynchronous
// KeywordPerformanceReportRequest, so the orchestrator keeps the last finished report and drives
// the next one across requests (Orchestrator.ReadReportedKeywordPerformance). These three
// methods are the stateless platform half.
//
// AUDIENCE INSIGHTS ARE NOT IMPLEMENTED for Microsoft, deliberately, so this dispatcher does not
// implement KeywordInsightsReader and Orchestrator.ReadAudienceInsights answers
// ErrKeywordInsightsUnsupported (400, "not supported for this platform"). Microsoft's
// AgeGenderAudienceReportRequest carries AgeGroup and Gender but no device dimension
// (AgeGenderAudienceReportColumn, learn.microsoft.com, read 2026-10-05), so the age/gender/device
// shape the audience read publishes would need a SECOND report for device, two pending reports
// per key, and a partial answer whenever only one had finished — not the clean single report
// this read is built on.
//
// Trust boundary: the project's OWN connection only (resolveOwned — no LF system fallback), the
// account that connection is bound to only, the scope from this service's database, and every
// refusal before any upstream call (KeywordReportAccount makes none).
var _ service.KeywordReportReader = (*MicrosoftDispatcher)(nil)

// microsoftKeywordsEnabled gates the keyword read on MICROSOFT_METRICS_ENABLED, the flag the
// campaign metrics read and the account monitor share, for the same reason: the Reporting
// contract follows Microsoft's published v13 documentation and has not been exercised against a
// live account. Disabled, the read answers the same 400 as a platform with no keyword read.
func microsoftKeywordsEnabled() error {
	if os.Getenv(constants.EnvMicrosoftMetricsEnabled) != "true" {
		return fmt.Errorf("microsoft keyword insights are disabled (%s is not \"true\") while the reporting contract is unverified: %w",
			constants.EnvMicrosoftMetricsEnabled, domain.ErrKeywordInsightsUnsupported)
	}
	return nil
}

// resolveMicrosoftKeywordClient resolves the project's own connection and returns a client for
// its bound account. requestedAccount, when non-empty, must be that account.
func (d *MicrosoftDispatcher) resolveMicrosoftKeywordClient(ctx context.Context, projectID string, platform model.Provider, requestedAccount string) (*microsoft.Client, error) {
	if err := microsoftKeywordsEnabled(); err != nil {
		return nil, err
	}
	res, err := d.creds.resolveOwned(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	creds, accountID, err := validateMicrosoftConnection(projectID, res)
	if err != nil {
		return nil, err
	}
	// The saved report is keyed by this id, so it must be exactly the shape the monitor keys on:
	// a stored id the strict check refuses can never be reported on.
	if verr := microsoft.ValidateMonitorAccountID(accountID); verr != nil {
		return nil, fmt.Errorf("%w: %w", domain.ErrConnectionNotUsable, verr)
	}
	if requestedAccount != "" {
		if err := requireMicrosoftManagedAccount(projectID, accountID, requestedAccount); err != nil {
			return nil, err
		}
	}
	return d.cachedMicrosoftClient(projectID, platform, res, creds, accountID), nil
}

// microsoftKeywordScopeIDs is the LOCAL half of the scope rules, needing no connection: every
// entry's id must be a canonical Microsoft campaign id (ErrKeywordReportScopeInvalid — refused
// here so a malformed stored id never reaches Microsoft, where it would fail every submission),
// ids are DE-DUPLICATED (the scope query's DISTINCT includes the result blob, and live-row
// uniqueness on (platform, platform_campaign_id) is enforced for Google only, so one Microsoft
// campaign can arrive as two rows), and the DISTINCT count must fit the report-scope ceiling.
// An empty scope is refused: nothing here may build a report wider than the caller's campaigns.
func microsoftKeywordScopeIDs(scope []model.ProjectCampaignScope) ([]string, error) {
	if len(scope) == 0 {
		return nil, fmt.Errorf("read microsoft keyword performance: %w", microsoft.ErrKeywordReportScope)
	}
	ids := make([]string, 0, len(scope))
	seen := make(map[string]bool, len(scope))
	for _, s := range scope {
		if err := microsoft.ValidateKeywordReportCampaignID(s.PlatformCampaignID); err != nil {
			return nil, fmt.Errorf("read microsoft keyword performance: %w: %w", domain.ErrKeywordReportScopeInvalid, err)
		}
		if !seen[s.PlatformCampaignID] {
			seen[s.PlatformCampaignID] = true
			ids = append(ids, s.PlatformCampaignID)
		}
	}
	if len(ids) > microsoft.MaxKeywordReportCampaigns {
		return nil, fmt.Errorf("read microsoft keyword performance: %d campaigns, at most %d per report: %w",
			len(ids), microsoft.MaxKeywordReportCampaigns, domain.ErrKeywordReportScopeTooLarge)
	}
	return ids, nil
}

// microsoftKeywordScope applies microsoftKeywordScopeIDs and then the provenance rule over EVERY
// row (not the de-duplicated ids — two rows for one campaign may record different accounts):
// ANY entry whose recorded creation account is not accountID refuses the whole read, the rule
// googleAdsScopeForCustomer applies for the reason recorded there. An entry with no recorded
// account is "unknown, proceed" (microsoftCreationAccountID).
func microsoftKeywordScope(scope []model.ProjectCampaignScope, accountID string) ([]string, error) {
	ids, err := microsoftKeywordScopeIDs(scope)
	if err != nil {
		return nil, err
	}
	mismatched := 0
	for _, s := range scope {
		created := microsoftCreationAccountID(&model.Campaign{Result: s.Result})
		if created != "" && created != accountID {
			mismatched++
		}
	}
	if mismatched > 0 {
		return nil, fmt.Errorf("read microsoft keyword performance: %d of this project's %d campaign rows were created under a different ad account than the one its connection is bound to (%s); returning only the rest would report a partial result as complete: %w",
			mismatched, len(scope), accountID, domain.ErrCampaignAccountMismatch)
	}
	return ids, nil
}

// KeywordReportAccount implements service.KeywordReportReader. NO upstream call: the gate, the
// window, the scope ceiling, the connection and the provenance check are all local.
func (d *MicrosoftDispatcher) KeywordReportAccount(ctx context.Context, projectID string, platform model.Provider, window model.MetricsWindow, scope []model.ProjectCampaignScope) (string, error) {
	if err := microsoftKeywordsEnabled(); err != nil {
		return "", err
	}
	// Window and scope size before the connection: both are permanent faults of the request or
	// of the project, and must answer the same way whatever state the connection is in.
	if err := microsoft.ValidateKeywordReportWindow(window); err != nil {
		return "", fmt.Errorf("read microsoft keyword performance: %w", errors.Join(domain.ErrMetricsWindowUnsupported, err))
	}
	if _, err := microsoftKeywordScopeIDs(scope); err != nil {
		return "", err
	}
	client, err := d.resolveMicrosoftKeywordClient(ctx, projectID, platform, "")
	if err != nil {
		return "", err
	}
	if _, err := microsoftKeywordScope(scope, client.AccountID()); err != nil {
		return "", err
	}
	return client.AccountID(), nil
}

// SubmitKeywordReport implements service.KeywordReportReader.
func (d *MicrosoftDispatcher) SubmitKeywordReport(ctx context.Context, projectID string, platform model.Provider, accountID string, window model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.KeywordReportSubmission, error) {
	if err := microsoft.ValidateKeywordReportWindow(window); err != nil {
		return nil, fmt.Errorf("submit microsoft keyword report: %w", errors.Join(domain.ErrMetricsWindowUnsupported, err))
	}
	client, err := d.resolveMicrosoftKeywordClient(ctx, projectID, platform, accountID)
	if err != nil {
		return nil, err
	}
	ids, err := microsoftKeywordScope(scope, client.AccountID())
	if err != nil {
		return nil, err
	}
	reportID, start, end, err := client.SubmitKeywordReport(ctx, window, ids)
	if err != nil {
		if errors.Is(err, microsoft.ErrKeywordReportScopeRejected) {
			// PERMANENT, and ours: Microsoft refused the request SHAPE this client builds
			// (campaign-only scope, error 2027), so every later read would be refused the same
			// way. Tagged as a service defect — a logged 500, not a retry-forever empty 200 and
			// not a 409 asking the caller to fix something they cannot. The scope is deliberately
			// NOT widened to AccountIds, which would read other projects' campaigns.
			return nil, fmt.Errorf("submit microsoft keyword report: %w: %w", domain.ErrServiceDefect, err)
		}
		return nil, fmt.Errorf("submit microsoft keyword report: %w", err)
	}
	return &model.KeywordReportSubmission{ReportID: reportID, WindowStart: start, WindowEnd: end, CampaignIDs: ids}, nil
}

// CheckKeywordReport implements service.KeywordReportReader: one Poll, and the rows if the
// report has finished, normalised onto the keyword read's published vocabulary.
func (d *MicrosoftDispatcher) CheckKeywordReport(ctx context.Context, projectID string, platform model.Provider, accountID, reportID string) (*model.KeywordReportCheck, error) {
	if strings.TrimSpace(reportID) == "" {
		return nil, errors.New("microsoft keyword report check: report id is required")
	}
	if strings.TrimSpace(accountID) == "" {
		return nil, errors.New("microsoft keyword report check: account id is required")
	}
	client, err := d.resolveMicrosoftKeywordClient(ctx, projectID, platform, accountID)
	if err != nil {
		return nil, err
	}
	res, err := client.CheckKeywordReport(ctx, reportID)
	if err != nil {
		return nil, fmt.Errorf("check microsoft keyword report: %w", err)
	}
	out := &model.KeywordReportCheck{Partial: res.Partial}
	switch res.Status {
	case microsoft.AccountReportStatusPending:
		out.Status = model.AccountReportPending
	case microsoft.AccountReportStatusError:
		out.Status = model.AccountReportFailed
	case microsoft.AccountReportStatusSuccess:
		out.Status = model.AccountReportReady
		out.Rows = make([]model.KeywordReportRow, 0, len(res.Rows))
		for _, r := range res.Rows {
			// The published cost is int64 micros; a spend whose micros would not fit is refused
			// here rather than wrapped, so a stored row always converts exactly.
			if r.Spend*1e6 >= math.MaxInt64 {
				return nil, fmt.Errorf("check microsoft keyword report: keyword %s spend %v exceeds the representable cost", r.KeywordID, r.Spend)
			}
			out.Rows = append(out.Rows, model.KeywordReportRow{
				CampaignID:   r.CampaignID,
				CampaignName: r.CampaignName,
				AdGroupID:    r.AdGroupID,
				AdGroupName:  r.AdGroupName,
				KeywordID:    r.KeywordID,
				Text:         r.Keyword,
				MatchType:    microsoftKeywordMatchType(r.MatchType),
				Status:       microsoftKeywordStatus(r.Status),
				QualityScore: r.QualityScore,
				Impressions:  r.Impressions,
				Clicks:       r.Clicks,
				Spend:        r.Spend,
				Conversions:  r.Conversions,
			})
		}
	default:
		return nil, fmt.Errorf("check microsoft keyword report: unmapped status %q", res.Status)
	}
	return out, nil
}

// microsoftKeywordMatchType maps BidMatchType (Broad, Exact, Phrase, Unknown) onto the keyword
// read's published enum. Anything else is UNKNOWN — a value this service does not recognise,
// never an absent match type — so one row cannot fail the generated client's validation.
func microsoftKeywordMatchType(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "exact":
		return "EXACT"
	case "phrase":
		return "PHRASE"
	case "broad":
		return "BROAD"
	default:
		return "UNKNOWN"
	}
}

// microsoftKeywordStatus maps KeywordStatus (Active, Paused; Deleted is dropped by the fold)
// onto the published enum, UNKNOWN for anything else.
func microsoftKeywordStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "active":
		return "ENABLED"
	case "paused":
		return "PAUSED"
	default:
		return "UNKNOWN"
	}
}
