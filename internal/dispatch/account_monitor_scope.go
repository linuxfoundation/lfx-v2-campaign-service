// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"fmt"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// The four platforms whose single upstream ad account is shared by every foundation
// (docs/architecture.md, "Account Tenancy"). LinkedIn and Microsoft deliberately do not
// implement service.AccountMonitorScoper, so their account monitors keep the account-wide read.
var (
	_ service.AccountMonitorScoper = (*GoogleAdsDispatcher)(nil)
	_ service.AccountMonitorScoper = (*MetaDispatcher)(nil)
	_ service.AccountMonitorScoper = (*RedditDispatcher)(nil)
	_ service.AccountMonitorScoper = (*TwitterDispatcher)(nil)
)

// MonitorScopePrecheck implements service.AccountMonitorScoper for Google Ads: the same customer
// id shape check ListAccountCampaignMetrics runs before it resolves a credential.
func (d *GoogleAdsDispatcher) MonitorScopePrecheck(accountID string) error {
	if err := googleads.ValidateCustomerID(accountID); err != nil {
		return fmt.Errorf("%w: %w", domain.ErrAccountIDMalformed, err)
	}
	return nil
}

// MonitorScopeForAccount implements service.AccountMonitorScoper for Google Ads with the same
// provenance check the keyword and audience reads use: a campaign recorded under a different
// customer than the monitored one fails the whole read.
func (d *GoogleAdsDispatcher) MonitorScopeForAccount(scope []model.ProjectCampaignScope, accountID string) ([]string, error) {
	return googleAdsScopeForCustomer(scope, accountID, "read google ads account monitor")
}

// MonitorScopePrecheck implements service.AccountMonitorScoper for Meta: the same account id shape
// check ReadAccountMonitor runs before it resolves a credential.
func (d *MetaDispatcher) MonitorScopePrecheck(accountID string) error {
	if err := meta.ValidateAccountID(accountID); err != nil {
		return fmt.Errorf("%w: %w", domain.ErrAccountIDMalformed, err)
	}
	return nil
}

// MonitorScopeForAccount implements service.AccountMonitorScoper for Meta; the account id is
// normalised to its act_ form inside metaScopeForAccount.
func (d *MetaDispatcher) MonitorScopeForAccount(scope []model.ProjectCampaignScope, accountID string) ([]string, error) {
	return metaScopeForAccount(scope, accountID, "read meta account monitor")
}

// MonitorScopePrecheck implements service.AccountMonitorScoper for Reddit: the same account id
// shape check readAccountMonitor runs before it resolves a credential.
func (d *RedditDispatcher) MonitorScopePrecheck(accountID string) error {
	if err := reddit.ValidateAccountID(accountID); err != nil {
		return fmt.Errorf("%w: %w", domain.ErrAccountIDMalformed, err)
	}
	return nil
}

// MonitorScopeForAccount implements service.AccountMonitorScoper for Reddit, which also refuses a
// campaign that records no creating account (see redditScopeForAccount).
func (d *RedditDispatcher) MonitorScopeForAccount(scope []model.ProjectCampaignScope, accountID string) ([]string, error) {
	return redditScopeForAccount(scope, accountID, "read reddit account monitor", domain.ErrAudienceScopeInvalid)
}

// MonitorScopePrecheck implements service.AccountMonitorScoper for X: the TWITTER_METRICS_ENABLED
// gate and the account id shape check, exactly as resolveTwitterMonitorClient runs them, so a
// disabled monitor is a 400 whatever the project has recorded.
func (d *TwitterDispatcher) MonitorScopePrecheck(accountID string) error {
	return twitterMonitorPrecheck(accountID)
}

// MonitorScopeForAccount implements service.AccountMonitorScoper for X.
func (d *TwitterDispatcher) MonitorScopeForAccount(scope []model.ProjectCampaignScope, accountID string) ([]string, error) {
	return twitterScopeForAccount(scope, accountID, "read x account monitor")
}
