// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// The Microsoft age/gender audience read is REPORT-BACKED exactly like the keyword read
// (microsoft_keyword_report.go): it implements service.AudienceReportReader over a saved
// AgeGenderAudienceReportRequest, with the keyword read's gate (MICROSOFT_METRICS_ENABLED), its
// connection rule (the project's OWN connection and the account it is bound to — resolveOwned,
// no LF system fallback), its scope rules (Campaigns only, de-duplicated, at most 300, canonical
// ids, ANY provenance mismatch refusing the whole read) and its permanent-refusal handling
// (2027 tagged ErrServiceDefect). Only the sentinels differ: a scope refusal here is the audience
// read's (ErrAudienceScopeInvalid / ErrAudienceScopeTooLarge), so its 409 names the read that
// refused. There is NO device dimension (see microsoft.SubmitAgeGenderReport).
var _ service.AudienceReportReader = (*MicrosoftDispatcher)(nil)

var microsoftAudienceScopeRules = microsoftReportScopeRules{
	read:       "read microsoft audience",
	empty:      microsoft.ErrAudienceReportScope,
	invalid:    domain.ErrAudienceScopeInvalid,
	tooLarge:   domain.ErrAudienceScopeTooLarge,
	validateID: microsoft.ValidateAudienceReportCampaignID,
}

// AudienceReportEnabled implements service.AudienceReportReader: the gate and the window, with
// no connection, no scope and no upstream call — KeywordReportEnabled's answers exactly.
func (d *MicrosoftDispatcher) AudienceReportEnabled(window model.MetricsWindow) error {
	if err := microsoftInsightsEnabled("audience"); err != nil {
		return err
	}
	if err := microsoft.ValidateKeywordReportWindow(window); err != nil {
		return fmt.Errorf("read microsoft audience: %w", errors.Join(domain.ErrMetricsWindowUnsupported, err))
	}
	return nil
}

// AudienceReportAccount implements service.AudienceReportReader. NO upstream call.
func (d *MicrosoftDispatcher) AudienceReportAccount(ctx context.Context, projectID string, platform model.Provider, window model.MetricsWindow, scope []model.ProjectCampaignScope) (string, error) {
	if err := d.AudienceReportEnabled(window); err != nil {
		return "", err
	}
	if _, err := microsoftReportScopeIDs(scope, microsoftAudienceScopeRules); err != nil {
		return "", err
	}
	client, err := d.resolveMicrosoftInsightsClient(ctx, projectID, platform, "", "audience")
	if err != nil {
		return "", err
	}
	if _, err := microsoftReportScope(scope, client.AccountID(), microsoftAudienceScopeRules); err != nil {
		return "", err
	}
	return client.AccountID(), nil
}

// SubmitAudienceReport implements service.AudienceReportReader.
func (d *MicrosoftDispatcher) SubmitAudienceReport(ctx context.Context, projectID string, platform model.Provider, accountID string, window model.MetricsWindow, scope []model.ProjectCampaignScope) (*model.InsightReportSubmission, error) {
	if err := microsoft.ValidateKeywordReportWindow(window); err != nil {
		return nil, fmt.Errorf("submit microsoft age/gender report: %w", errors.Join(domain.ErrMetricsWindowUnsupported, err))
	}
	client, err := d.resolveMicrosoftInsightsClient(ctx, projectID, platform, accountID, "audience")
	if err != nil {
		return nil, err
	}
	ids, err := microsoftReportScope(scope, client.AccountID(), microsoftAudienceScopeRules)
	if err != nil {
		return nil, err
	}
	reportID, start, end, err := client.SubmitAgeGenderReport(ctx, window, ids)
	if err != nil {
		if errors.Is(err, microsoft.ErrAudienceReportScopeRejected) {
			// PERMANENT and ours, for SubmitKeywordReport's reason: never widened to AccountIds.
			return nil, fmt.Errorf("submit microsoft age/gender report: %w: %w", domain.ErrServiceDefect, err)
		}
		return nil, fmt.Errorf("submit microsoft age/gender report: %w", err)
	}
	return &model.InsightReportSubmission{ReportID: reportID, WindowStart: start, WindowEnd: end, CampaignIDs: ids}, nil
}

// CheckAudienceReport implements service.AudienceReportReader: one Poll, and the rows if the
// report has finished.
func (d *MicrosoftDispatcher) CheckAudienceReport(ctx context.Context, projectID string, platform model.Provider, accountID, reportID string) (*model.AudienceReportCheck, error) {
	if strings.TrimSpace(reportID) == "" {
		return nil, errors.New("microsoft age/gender report check: report id is required")
	}
	if strings.TrimSpace(accountID) == "" {
		return nil, errors.New("microsoft age/gender report check: account id is required")
	}
	client, err := d.resolveMicrosoftInsightsClient(ctx, projectID, platform, accountID, "audience")
	if err != nil {
		return nil, err
	}
	res, err := client.CheckAgeGenderReport(ctx, reportID)
	if err != nil {
		return nil, fmt.Errorf("check microsoft age/gender report: %w", err)
	}
	out := &model.AudienceReportCheck{Partial: res.Partial}
	switch res.Status {
	case microsoft.AccountReportStatusPending:
		out.Status = model.AccountReportPending
	case microsoft.AccountReportStatusError:
		out.Status = model.AccountReportFailed
	case microsoft.AccountReportStatusSuccess:
		out.Status = model.AccountReportReady
		out.Rows = make([]model.AudienceReportRow, 0, len(res.Rows))
		for _, r := range res.Rows {
			// As CheckKeywordReport: a stored row's spend must convert to int64 micros exactly.
			if r.Spend*1e6 >= math.MaxInt64 {
				return nil, fmt.Errorf("check microsoft age/gender report: campaign %s spend %v exceeds the representable cost", r.CampaignID, r.Spend)
			}
			out.Rows = append(out.Rows, model.AudienceReportRow{
				CampaignID:  r.CampaignID,
				AgeGroup:    r.AgeGroup,
				Gender:      r.Gender,
				Impressions: r.Impressions,
				Clicks:      r.Clicks,
				Spend:       r.Spend,
			})
		}
	default:
		return nil, fmt.Errorf("check microsoft age/gender report: unmapped status %q", res.Status)
	}
	return out, nil
}
