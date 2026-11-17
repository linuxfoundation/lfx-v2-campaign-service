// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// The platforms whose account monitor is campaign-scoped (docs/architecture.md, "Account
// Tenancy"): the four whose single upstream ad account is shared by every foundation, and
// LinkedIn, whose LF system row serves several foundations' accounts through its grants (#581).
// Microsoft deliberately does not implement service.AccountMonitorScoper, so its account monitor
// keeps the account-wide read.
var (
	_ service.AccountMonitorScoper = (*GoogleAdsDispatcher)(nil)
	_ service.AccountMonitorScoper = (*MetaDispatcher)(nil)
	_ service.AccountMonitorScoper = (*RedditDispatcher)(nil)
	_ service.AccountMonitorScoper = (*TwitterDispatcher)(nil)
	_ service.AccountMonitorScoper = (*LinkedInDispatcher)(nil)
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

var _ service.AccountMonitorDefaulter = (*GoogleAdsDispatcher)(nil)

// MonitorAccountForScope implements service.AccountMonitorDefaulter for Google Ads: the one
// customer the project's campaigns record as the one they were created under
// (googleAdsCreationCustomerID). It is then checked by MonitorScopeForAccount like a customer the
// caller named, so every refusal that applies to a named customer applies to this one.
//
// No entry recording a customer leaves nothing to name, and is refused as an unrecorded entry is
// on a named customer. Entries recorded under more than one customer are refused with
// ErrCampaignAccountMismatch: the monitor reads one customer, and picking one would report part
// of the project's campaigns as all of them. Naming either customer is refused the same way
// (googleAdsScopeForCustomer fails on any mismatch), so the remedy is re-dispatching or
// reconciling the campaigns onto one customer, not naming one.
func (d *GoogleAdsDispatcher) MonitorAccountForScope(scope []model.ProjectCampaignScope) (string, error) {
	const op = "read google ads account monitor"
	customers := make(map[string]bool, 1)
	customer := ""
	for _, s := range scope {
		if created := googleAdsCreationCustomerID(&model.Campaign{Result: s.Result}); created != "" {
			customers[created] = true
			customer = created
		}
	}
	switch len(customers) {
	case 0:
		return "", fmt.Errorf("%s: none of this project's %d campaigns record which google ads customer they were created under, so there is no customer to monitor them on: %w",
			op, len(scope), errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	case 1:
		return customer, nil
	default:
		return "", fmt.Errorf("%s: this project's campaigns were created under %d different google ads customers, so no one customer's monitor covers them all: %w",
			op, len(customers), domain.ErrCampaignAccountMismatch)
	}
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
//
// Unlike Google, Reddit and X it does not wrap the scope in requireRecordedMonitorProvenance: a
// recorded campaign with no creation account is read as "unknown, proceed". That stays safe now
// the read can run on the LF system credential (#615) because Meta campaign ids are globally
// unique objects, so an unrecorded id this project owns can only ever match that same campaign,
// never another foundation's. Making it strict would turn this project's legacy Meta campaigns
// that predate provenance into a 409 on its own connection, which is a separate change.
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

// MonitorScopePrecheck implements service.AccountMonitorScoper for LinkedIn: the account id shape
// check ReadAccountMonitor runs before it resolves a credential.
func (d *LinkedInDispatcher) MonitorScopePrecheck(accountID string) error {
	if err := linkedin.ValidateAccountID(accountID); err != nil {
		return fmt.Errorf("%w: %w", domain.ErrAccountIDMalformed, err)
	}
	return nil
}

// MonitorScopeForAccount implements service.AccountMonitorScoper for LinkedIn (#581) with the
// LF system row's rule, the one that applies whenever the orchestrator has not been told the read
// is served by the project's own connection (MonitorScoperForConnection). The monitor is
// campaign-scoped because one LF system credential serves several foundations' ad accounts (its
// default and its grants): an account-wide read through it would show one foundation another's
// campaigns. A campaign recorded under a different account than accountID fails the read
// (ErrCampaignAccountMismatch). One that records no account is OMITTED and logged rather than
// failing the whole monitor: on the shared credential its id cannot be proven to be this
// project's, and the response has no field to report it in.
func (d *LinkedInDispatcher) MonitorScopeForAccount(scope []model.ProjectCampaignScope, accountID string) ([]string, error) {
	return linkedInMonitorScope(scope, accountID, false, "")
}

var _ service.AccountMonitorConnectionScoper = (*LinkedInDispatcher)(nil)

// MonitorScoperForConnection implements service.AccountMonitorConnectionScoper for LinkedIn
// (#628). When projectID's read of accountID resolves to the project's OWN connection, a live
// campaign that records no creation account is kept, as belonging to that connection's account:
// requireLinkedInManagedAccount has proven the account is accountID, and that is what the monitor
// assumed before #581 when it read the project's own account whole. Campaigns created before
// account stamping therefore keep appearing for a project on its own connection.
//
// When the read resolves to the LF system row, the returned scoper is the system rule
// (MonitorScopeForAccount) with projectID for its log line. A resolution failure returns nil,
// which keeps the system rule; the read then reports the failure itself.
func (d *LinkedInDispatcher) MonitorScoperForConnection(ctx context.Context, projectID, accountID string) service.AccountMonitorScoper {
	res, _, _, err := d.resolveLinkedInMonitorCredentials(ctx, projectID, model.ProviderLinkedInAds, accountID)
	if err != nil || res == nil {
		return nil
	}
	return linkedInMonitorScoper{d: d, own: !res.fromSystem, projectID: projectID}
}

// linkedInMonitorScoper is the LinkedIn monitor scope bound to the connection that serves one
// read: own is true for the project's own connection, false for the LF system row.
type linkedInMonitorScoper struct {
	d         *LinkedInDispatcher
	own       bool
	projectID string
}

func (s linkedInMonitorScoper) MonitorScopePrecheck(accountID string) error {
	return s.d.MonitorScopePrecheck(accountID)
}

func (s linkedInMonitorScoper) MonitorScopeForAccount(scope []model.ProjectCampaignScope, accountID string) ([]string, error) {
	return linkedInMonitorScope(scope, accountID, s.own, s.projectID)
}

// linkedInMonitorScope is the LinkedIn monitor's scope rule. A recorded campaign from another
// account, or a malformed id, fails it either way. An unrecorded campaign is kept on the project's
// own connection (ownConnection) and omitted, with a log line, on the LF system row.
func linkedInMonitorScope(scope []model.ProjectCampaignScope, accountID string, ownConnection bool, projectID string) ([]string, error) {
	const op = "read linkedin account monitor"
	if ownConnection {
		return linkedInScopeForAccount(scope, accountID, op)
	}
	recorded := make([]model.ProjectCampaignScope, 0, len(scope))
	for _, s := range scope {
		if linkedInCreationAccountID(&model.Campaign{PlatformCampaignID: s.PlatformCampaignID, Result: s.Result}) != "" {
			recorded = append(recorded, s)
		}
	}
	if omitted := len(scope) - len(recorded); omitted > 0 {
		slog.Warn("linkedin account monitor: campaigns that record no creation account omitted from a read on the LF system credential",
			"project_id", projectID, "account_id", accountID, "omitted", omitted, "of", len(scope))
	}
	return linkedInScopeForAccount(recorded, accountID, op)
}

var _ service.AccountMonitorDefaulter = (*LinkedInDispatcher)(nil)

// MonitorAccountForScope implements service.AccountMonitorDefaulter for LinkedIn: the one ad
// account the project's campaigns record as the one they were created under, exactly as Google
// Ads names its customer (#591). A project creating through the LF system row has no connection
// of its own to take the account from, so this is how it names the default or granted account
// its campaigns live in.
func (d *LinkedInDispatcher) MonitorAccountForScope(scope []model.ProjectCampaignScope) (string, error) {
	const op = "read linkedin account monitor"
	accounts := make(map[string]bool, 1)
	account := ""
	for _, s := range scope {
		if created := linkedInCreationAccountID(&model.Campaign{PlatformCampaignID: s.PlatformCampaignID, Result: s.Result}); created != "" {
			accounts[created] = true
			account = created
		}
	}
	switch len(accounts) {
	case 0:
		return "", fmt.Errorf("%s: none of this project's %d campaigns record which linkedin ad account they were created under, so there is no account to monitor them on: %w",
			op, len(scope), errors.Join(domain.ErrCampaignProvenanceUnknown, domain.ErrCampaignAccountMismatch))
	case 1:
		return account, nil
	default:
		return "", fmt.Errorf("%s: this project's campaigns were created under %d different linkedin ad accounts, so no one account's monitor covers them all: %w",
			op, len(accounts), domain.ErrCampaignAccountMismatch)
	}
}

// linkedInScopeForAccount returns the scope's campaign ids when every entry that records a
// creation account records accountID, and refuses the whole scope otherwise (an unrecorded entry
// passes here; linkedInMonitorScope drops it first when the LF system row serves the read). Ids are checked for
// LinkedIn's canonical shape, so a stored id the platform cannot read is ErrAudienceScopeInvalid.
func linkedInScopeForAccount(scope []model.ProjectCampaignScope, accountID, op string) ([]string, error) {
	want := strings.TrimSpace(accountID)
	ids := make([]string, 0, len(scope))
	mismatched := 0
	for _, s := range scope {
		if verr := linkedin.ValidateAudienceCampaignID(s.PlatformCampaignID); verr != nil {
			return nil, fmt.Errorf("%s: a campaign in scope has a malformed linkedin campaign id (%d bytes): %w",
				op, len(s.PlatformCampaignID), domain.ErrAudienceScopeInvalid)
		}
		created := linkedInCreationAccountID(&model.Campaign{PlatformCampaignID: s.PlatformCampaignID, Result: s.Result})
		if created != "" && created != want {
			mismatched++
			continue
		}
		ids = append(ids, s.PlatformCampaignID)
	}
	if mismatched > 0 {
		return nil, fmt.Errorf("%s: %d of this project's %d campaigns were created under a different linkedin ad account than %s; returning only the rest would report a partial result as complete: %w",
			op, mismatched, len(scope), want, domain.ErrCampaignAccountMismatch)
	}
	return ids, nil
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
