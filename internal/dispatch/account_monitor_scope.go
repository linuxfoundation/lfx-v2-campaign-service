// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"errors"
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

// MonitorScopeForAccount implements service.AccountMonitorScoper for Google Ads with the
// provenance check the keyword and audience reads use — a campaign recorded under a different
// customer than the monitored one fails the whole read — and, unlike them, also refuses one that
// records no creating customer (requireRecordedMonitorProvenance).
func (d *GoogleAdsDispatcher) MonitorScopeForAccount(scope []model.ProjectCampaignScope, accountID string) ([]string, error) {
	const op = "read google ads account monitor"
	ids, err := googleAdsScopeForCustomer(scope, accountID, op)
	return requireRecordedMonitorProvenance(op, "google ads customer", scope, googleAdsCreationCustomerID, ids, err)
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

// MonitorScopeForAccount implements service.AccountMonitorScoper for X: twitterScopeForAccount,
// which also refuses a campaign that records no creating account here
// (requireRecordedMonitorProvenance).
func (d *TwitterDispatcher) MonitorScopeForAccount(scope []model.ProjectCampaignScope, accountID string) ([]string, error) {
	const op = "read x account monitor"
	ids, err := twitterScopeForAccount(scope, accountID, op)
	return requireRecordedMonitorProvenance(op, "x ads account", scope, twitterCreationAccountID, ids, err)
}

// requireRecordedMonitorProvenance applies redditScopeForAccount's strictness to a monitor scope
// that a permissive helper (googleAdsScopeForCustomer, twitterScopeForAccount) has already
// checked: ids and err are that helper's result, and created reads an entry's recorded creation
// account ("" = unknown).
//
// The insight reads keep an unrecorded row on purpose (see googleAdsScopeForCustomer). The account
// monitor cannot: on Google Ads and X a campaign id is only unique within its account, and the
// monitor reads an account named by the request (Google) or by a connection that may have been
// re-pointed (X), so an unrecorded id could select another foundation's campaign there and
// filterMonitorRows would return its budget and spend as this project's. A scope with any
// unrecorded entry therefore fails: ErrCampaignProvenanceUnknown joined with
// ErrCampaignAccountMismatch on its own, and ErrCampaignProvenanceMixed with both when the
// recorded entries also mismatch, exactly as redditScopeForAccount answers. The helper's other
// refusals (a malformed id) are returned unchanged.
func requireRecordedMonitorProvenance(op, accountKind string, scope []model.ProjectCampaignScope, created func(*model.Campaign) string, ids []string, err error) ([]string, error) {
	unknown := 0
	for _, s := range scope {
		if created(&model.Campaign{Result: s.Result}) == "" {
			unknown++
		}
	}
	switch {
	case unknown == 0:
		return ids, err
	case errors.Is(err, domain.ErrCampaignAccountMismatch):
		return nil, fmt.Errorf("%s: of this project's %d campaigns, %d do not record which %s they were created under and the others do not all fit the monitored one: %w",
			op, len(scope), unknown, accountKind, errors.Join(domain.ErrCampaignProvenanceMixed, domain.ErrCampaignProvenanceUnknown, err))
	case err != nil:
		return nil, err
	default:
		return nil, fmt.Errorf("%s: %d of this project's %d campaigns do not record which %s they were created under, so their ids cannot be proven to name the same campaigns on the monitored account: %w",
			op, unknown, len(scope), accountKind, errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	}
}
