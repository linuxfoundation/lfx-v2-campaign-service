// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// hubspotMonitorConcurrency bounds how many statistics reads the email account monitor has in
// flight at once. HubSpot's statistics endpoint answers for one email per request here (the
// client filters to exactly one id and proves it), so the monitor issues one request per email;
// four at a time keeps a full read well inside a private app's burst allowance while finishing
// inside the 20s call budget.
const hubspotMonitorConcurrency = 4

// hubspotMonitorTarget is one email the monitor will ask HubSpot about.
type hubspotMonitorTarget struct {
	campaignID string
	emailID    string
	name       string
	abVariant  bool
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
//  2. the project's OWN connection is resolved — never the LF system fallback, exactly as the
//     ad-platform monitors and the connection test resolve — so a project with no HubSpot
//     connection of its own gets ErrNotFound (404), not the LF portal's numbers;
//  3. the token's portal is read (token-info): an email id means something only inside the
//     portal that minted it, so each row's recorded creating portal must equal it. A row that
//     records none, records another, or carries a malformed id is counted unattributable and NOT
//     read — reading it could report another portal's email of the same number;
//  4. one statistics read per remaining email, at most hubspotMonitorConcurrency at once.
//
// DEFINITE OR NOTHING. Any failure of steps 3–4 — transport, a 5xx, a 429 still refused after the
// client's retries, a 401/403, a malformed, untrustworthy (identityjson) or filter-violating
// response, a null counter — fails the whole read with no partial result: a monitor that silently
// drops the one email it could not read reports a total that is wrong while looking complete. The
// one per-email answer that is NOT a failure is HubSpot reporting no send of that email inside the
// span (hubspot.ErrNoSentEmailInWindow), which is counted, not read as zeros.
func (d *HubSpotDispatcher) ReadEmailMonitor(ctx context.Context, projectID string, platform model.Provider, campaigns []*model.Campaign, days int) (*model.HubSpotEmailMonitorRead, error) {
	if err := validateMonitorDays(days); err != nil {
		return nil, err
	}
	client, _, err := d.resolveHubSpotClientVia(ctx, projectID, platform, d.creds.resolveOwned)
	if err != nil {
		return nil, err
	}
	start, end, asOf, err := client.MonitorSpan(days)
	if err != nil {
		return nil, fmt.Errorf("hubspot email monitor: %w", err)
	}
	read := &model.HubSpotEmailMonitorRead{
		Emails:    []model.HubSpotMonitorEmail{},
		SpanStart: start,
		SpanEnd:   end,
		AsOf:      asOf,
	}

	// Bounded by its own short deadline, as in ReadMetrics: the client's retry policy alone can
	// outlast the whole call budget on sustained throttling.
	portalCtx, cancelPortal := context.WithTimeout(ctx, portalLookupTimeout)
	current, perr := client.AuthenticatedPortalID(portalCtx)
	cancelPortal()
	if perr != nil {
		return nil, fmt.Errorf("hubspot email monitor: cannot establish which portal this token authenticates against: %w", perr)
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
		return nil, werr
	}

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
// emails that cannot be read safely. An email id seen twice is read once, under its first
// (newest) row. Every decision here is local; nothing is sent.
func hubspotMonitorTargets(campaigns []*model.Campaign, currentPortal string) ([]hubspotMonitorTarget, int) {
	targets := make([]hubspotMonitorTarget, 0, len(campaigns))
	seen := map[string]struct{}{}
	unattributable := 0
	for _, c := range campaigns {
		if c == nil {
			continue
		}
		var rec hubspotRecordedEmails
		// An undecodable blob records no portal, which is the unattributable case below; the
		// decode error itself carries nothing more to report.
		_ = json.Unmarshal(c.Result, &rec)
		candidates := []hubspotMonitorTarget{{campaignID: c.ID, emailID: strings.TrimSpace(c.PlatformCampaignID), name: c.CampaignName}}
		if rec.ABTestVariant != nil && strings.TrimSpace(rec.ABTestVariant.ID) != "" {
			candidates = append(candidates, hubspotMonitorTarget{
				campaignID: c.ID, emailID: strings.TrimSpace(rec.ABTestVariant.ID),
				name: rec.ABTestVariant.Name, abVariant: true,
			})
		}
		attributable := strings.TrimSpace(rec.PortalID) != "" && strings.TrimSpace(rec.PortalID) == currentPortal
		for _, t := range candidates {
			if _, dup := seen[t.emailID]; dup {
				continue
			}
			seen[t.emailID] = struct{}{}
			if !attributable || hubspot.ValidateEmailID(t.emailID) != nil {
				unattributable++
				continue
			}
			targets = append(targets, t)
		}
	}
	return targets, unattributable
}
