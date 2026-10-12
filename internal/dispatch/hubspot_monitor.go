// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// hubspotMonitorConcurrency bounds how many statistics reads the email account monitor has in
// flight at once. The statistics endpoint answers for one email per request here (the client
// filters to exactly one id and proves it), so a read is at most 101 LOGICAL calls (100 emails
// plus token-info). Each is idempotent, so the client retries a 429 up to 3 times (retryMax), and
// a read can therefore send up to 404 HTTP attempts. Nothing here proves that fits a private
// app's burst allowance: HubSpot's limits
// are per app and shared with every other caller of the same token (100 requests per 10s on the
// lowest tiers). Two at a time keeps this read's own share modest; under contention a request can
// still be throttled past the client's retries, and the read then fails as a whole (503) rather
// than returning a partial result. The client has no pacer to reuse, so none is added here.
const hubspotMonitorConcurrency = 2

// hubspotMonitorTarget is one email the monitor will ask HubSpot about.
type hubspotMonitorTarget struct {
	campaignID string
	emailID    string
	name       string
	abVariant  bool
	deleted    bool
}

// hubspotRecordedEmails is the part of a HubSpot campaign row's Result blob the monitor reads:
// the portal the email was created in, and the A/B variant email when one was created
// (campaignFromHubSpot writes both).
type hubspotRecordedEmails struct {
	PortalID      string `json:"portalId"`
	ABTestVariant *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"abTestVariant"`
}

// ReadEmailMonitor implements service.EmailMonitorReader: the statistics counters of the
// marketing emails THIS SERVICE created for the project — the campaigns the caller passes, which
// the orchestrator read from this service's own table by project_id — sent within the trailing
// `days` UTC days. A pure read.
//
// Scope is the point. A HubSpot portal is shared across projects, so a portal-wide statistics read
// would report other projects' sends; this asks about each recorded email id by itself (the
// client filters to exactly that id and refuses a response that covers anything else), and never
// without one.
//
// Order, each refusal before any upstream call it would otherwise waste:
//
//  1. days is re-checked (the service layer checks it too; a non-HTTP caller does not pass Goa);
//  2. the connection is resolved the way Dispatch and ReadMetrics resolve it — the project's own,
//     else the LF system fallback. Unlike the ad monitors (which refuse the fallback because
//     their read is ACCOUNT-wide and would expose every project on a shared account), this read
//     never widens past the project's own recorded email ids, and each is checked against the
//     portal it was created in (step 3). Refusing the fallback here would 404 exactly the
//     projects whose emails WERE sent through the LF system portal. Only a project with no
//     connection and no system row gets ErrNotFound (404);
//  3. the token's portal is read (token-info): an email id means something only inside the
//     portal that minted it, so each row's recorded creating portal must equal it. A row that
//     records none, records another, or carries a malformed id is counted unattributable and NOT
//     read — reading it could report another portal's email of the same number;
//  4. one statistics read per remaining email, at most hubspotMonitorConcurrency at once.
//
// DEFINITE OR NOTHING. Any failure of steps 3–4 fails the whole read. A 401/403 is a credential
// the platform refused — tagged ErrConnectionNotUsable (400; 500 when it came from the LF system
// row). Everything else — transport, a 5xx, a 429 still refused after the client's retries, a
// malformed, untrustworthy (identityjson) or filter-violating response, a null counter — fails
// the whole read with no partial result: a monitor that silently drops the one email it could
// not read reports a total that is wrong while looking complete. The
// one per-email answer that is NOT a failure is HubSpot reporting no send of that email inside the
// span (hubspot.ErrNoSentEmailInWindow), which is counted, not read as zeros.
func (d *HubSpotDispatcher) ReadEmailMonitor(ctx context.Context, projectID string, platform model.Provider, campaigns []*model.Campaign, days int) (*model.HubSpotEmailMonitorRead, error) {
	if err := validateMonitorDays(days); err != nil {
		return nil, err
	}
	// res is kept: a 401/403 seen AFTER a clean resolution is classified against the row the
	// credential came from (see permissionRejected).
	client, res, err := d.resolveHubSpotClientWithCreds(ctx, projectID, platform)
	if err != nil {
		return nil, err
	}
	start, end, err := client.MonitorSpan(days)
	if err != nil {
		return nil, fmt.Errorf("hubspot email monitor: %w", err)
	}
	read := &model.HubSpotEmailMonitorRead{
		Emails:    []model.HubSpotMonitorEmail{},
		SpanStart: start,
		SpanEnd:   end,
	}
	// permissionRejected tags a 401/403 exactly as SearchEmails, SearchCampaigns and
	// CreateCampaign do: a revoked token or a missing scope does not recover by retrying, so it
	// is a connection that cannot be used as configured (400 for a project-owned token) — and,
	// through res.systemScoped, a defect of the operator-owned LF row when the credential came
	// from the system fallback (500). Untagged it would be the default, retryable 503.
	permissionRejected := func(what string, err error) error {
		return res.systemScoped(fmt.Errorf("%w: hubspot email monitor: %s: %w", domain.ErrConnectionNotUsable, what, err))
	}

	// Bounded by its own short deadline, as in ReadMetrics: the client's retry policy alone can
	// outlast the whole call budget on sustained throttling.
	portalCtx, cancelPortal := context.WithTimeout(ctx, portalLookupTimeout)
	current, perr := client.AuthenticatedPortalID(portalCtx)
	cancelPortal()
	if perr != nil {
		if hubspot.IsPermissionRejection(perr) {
			return nil, permissionRejected("token-info refused the credential", perr)
		}
		return nil, fmt.Errorf("hubspot email monitor: cannot establish which portal this token authenticates against: %w", perr)
	}
	// metrics_as_of is the instant the LAST upstream response of the read arrived — an UPPER
	// bound: every counter in the result was read at or before it. Starts at the token-info
	// answer, so a read with nothing to ask about still states when it ran, and advances with
	// each statistics response. Capturing it before the fan-out would describe counters read up
	// to 20s later as if read earlier.
	var asOfMu sync.Mutex
	asOf := client.Now()
	markReceived := func() {
		now := client.Now()
		asOfMu.Lock()
		if now.After(asOf) {
			asOf = now
		}
		asOfMu.Unlock()
	}

	targets, unattributable := hubspotMonitorTargets(campaigns, current)
	read.EmailsUnattributable = unattributable
	read.EmailsChecked = len(targets)

	results := make([]*hubspot.EmailCounters, len(targets))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(hubspotMonitorConcurrency)
	for i, t := range targets {
		g.Go(func() error {
			counters, cerr := client.GetEmailCounters(gctx, t.emailID, start, end)
			markReceived()
			if errors.Is(cerr, hubspot.ErrNoSentEmailInWindow) {
				return nil // results[i] stays nil: counted below, never read as zeros
			}
			if cerr != nil {
				return fmt.Errorf("hubspot email monitor: read statistics for email %s: %w", t.emailID, cerr)
			}
			results[i] = counters
			return nil
		})
	}
	if werr := g.Wait(); werr != nil {
		if hubspot.IsPermissionRejection(werr) {
			return nil, permissionRejected("statistics refused the credential", werr)
		}
		return nil, werr
	}
	read.AsOf = asOf

	for i, t := range targets {
		c := results[i]
		if c == nil {
			read.EmailsNotSentInWindow++
			continue
		}
		read.Emails = append(read.Emails, model.HubSpotMonitorEmail{
			CampaignID: t.campaignID,
			EmailID:    t.emailID,
			Name:       t.name,
			ABVariant:  t.abVariant,
			Deleted:    t.deleted,
			Counters: model.HubSpotEmailCounters{
				Sent: c.Sent, Delivered: c.Delivered, Opens: c.Opens, Clicks: c.Clicks,
				Bounces: c.Bounces, Unsubscribes: c.Unsubscribes, SpamReports: c.SpamReports,
			},
		})
	}
	return read, nil
}

// hubspotMonitorTargets turns the project's recorded campaigns into the emails to read, in the
// campaigns' order (each campaign's own email, then its A/B variant), and counts the recorded
// emails that cannot be read safely. Every decision here is local; nothing is sent.
//
// Attribution is decided BEFORE de-duplication, and only attributable emails are de-duplicated.
// A HubSpot email id is unique only within its portal, so the same number on a row recorded
// against another portal (or a malformed one) is a DIFFERENT email: letting it mark the id as
// seen would suppress an older, attributable row with that number, and a foreign row met after
// an attributable one would escape the unattributable count. Every attributable email is in the
// current portal, so the dedupe key is portal+id; an id seen twice there is read once, under its
// first (newest) row — unless that row is soft-deleted and a later one is live, in which case the
// live row owns it. Soft-deleted rows are read at all because a local delete neither stops nor
// deletes the HubSpot email; they are marked Deleted.
func hubspotMonitorTargets(campaigns []*model.Campaign, currentPortal string) ([]hubspotMonitorTarget, int) {
	targets := make([]hubspotMonitorTarget, 0, len(campaigns))
	seen := map[string]int{} // portal/id → index in targets
	unattributable := 0
	for _, c := range campaigns {
		if c == nil {
			continue
		}
		var rec hubspotRecordedEmails
		// An undecodable blob records no portal, which is the unattributable case below. A type
		// error still leaves the fields decoded before it (a portal, but no variant), so the record
		// is reset on ANY decode error rather than trusted in part. The reset must not make a
		// recorded variant vanish, though: if the blob is still a JSON object with a non-null
		// abTestVariant, that variant is a second recorded email, counted unattributable with the
		// row's own. Only a blob that is not even a JSON object records nothing more to count.
		variantUnreadable := false
		if err := json.Unmarshal(c.Result, &rec); err != nil {
			rec = hubspotRecordedEmails{}
			var fields map[string]json.RawMessage
			if json.Unmarshal(c.Result, &fields) == nil {
				if v, ok := fields["abTestVariant"]; ok && string(bytes.TrimSpace(v)) != "null" {
					variantUnreadable = true
				}
			}
		}
		deleted := c.Status == model.CampaignStatusDeleted
		// Ids are taken VERBATIM, never trimmed: ValidateEmailID is what decides whether a stored
		// id is usable, and a padded " 123 " is a malformed record to count, not one to repair
		// and request. A PRESENT variant is always a candidate, so one with an empty or blank id
		// is counted unattributable too; only an absent variant contributes nothing.
		candidates := []hubspotMonitorTarget{{campaignID: c.ID, emailID: c.PlatformCampaignID, name: c.CampaignName, deleted: deleted}}
		if rec.ABTestVariant != nil {
			candidates = append(candidates, hubspotMonitorTarget{
				campaignID: c.ID, emailID: rec.ABTestVariant.ID,
				name: rec.ABTestVariant.Name, abVariant: true, deleted: deleted,
			})
		} else if variantUnreadable {
			// No portal was trusted, so this candidate is counted below, never requested.
			candidates = append(candidates, hubspotMonitorTarget{campaignID: c.ID, abVariant: true, deleted: deleted})
		}
		portal := strings.TrimSpace(rec.PortalID)
		attributable := portal != "" && portal == currentPortal
		for _, t := range candidates {
			if !attributable || hubspot.ValidateEmailID(t.emailID) != nil {
				unattributable++
				continue
			}
			key := portal + "/" + t.emailID
			if at, dup := seen[key]; dup {
				// The same email on a soft-deleted row and a live one belongs to the live row:
				// the email is not "deleted" while a live campaign still holds it.
				if targets[at].deleted && !t.deleted {
					targets[at] = t
				}
				continue
			}
			seen[key] = len(targets)
			targets = append(targets, t)
		}
	}
	return targets, unattributable
}
