// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"time"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service/rules"
)

// hubspotMonitorDiscovery is monitor-hubspot-account's descriptor for classifyDiscoveryError,
// named "account monitor" like the six ad-platform monitors' own descriptors. The remedy text is
// the HubSpot connection's: a private-app token is the whole credential. It names the PUBLISHED
// wire field private_app_token (design/connection.go), not the persisted privateAppToken, and it
// covers the 401/403 path too: there the field is present but the token is revoked or lacks the
// marketing-email scopes, so "set" alone would not tell the caller what to fix.
var hubspotMonitorDiscovery = accountDiscovery{
	provider:    model.ProviderHubSpot,
	displayName: "hubspot",
	notUsableRemedy: "check that it is active, that the stored credential is valid json with " +
		"private_app_token set, and that the token is still valid with the marketing-email scopes",
	operation: "account monitor",
}

// MonitorHubspotAccount is the HubSpot email account monitor (LFXV2-2665): the statistics of the
// marketing emails this service created for the project and sent within the trailing `days`, with
// the email rule engine's findings. Same guards and error classification as the ad-platform
// monitors (monitorAccount): the reserved system scope is refused first, days is validated, the
// orchestrator must be wired, and every read failure goes through classifyDiscoveryError — so no
// connection is 404; an unusable connection is 400 when it is the project's own and 500 when it is
// the LF system fallback's (ErrSystemConnectionNotUsable — an operator-owned row the caller cannot
// edit), which also covers a 401/403 HubSpot returns for that token; a credential that fails
// decryption is 500; anything else upstream is 503 — with no partial result.
func (s *ConnectionService) MonitorHubspotAccount(ctx context.Context, p *conn.MonitorHubspotAccountPayload) (*conn.HubspotEmailMonitor, error) {
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	if err := validateMonitorDays(p.Days); err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch(hubspotMonitorDiscovery.label())
	if err != nil {
		return nil, err
	}
	read, rerr := orch.ReadHubSpotEmailMonitor(ctx, p.ProjectID, model.ProviderHubSpot, p.Days)
	if rerr != nil {
		return nil, s.classifyDiscoveryError(ctx, p.ProjectID, hubspotMonitorDiscovery, rerr)
	}
	return buildHubSpotEmailMonitor(p.Days, read), nil
}

// buildHubSpotEmailMonitor assembles the response. The totals are the sum of exactly the emails
// returned, with rates computed from the SUMS, so the aggregate and the list describe the same
// population — the rule every sibling monitor follows (buildAccountMonitor).
func buildHubSpotEmailMonitor(days int, read *model.HubSpotEmailMonitorRead) *conn.HubspotEmailMonitor {
	emails := make([]*conn.HubspotEmailMonitorEmail, 0, len(read.Emails))
	var sum model.HubSpotEmailCounters
	for _, e := range read.Emails {
		sum = sum.Add(e.Counters)
		c, r := e.Counters, rules.HubSpotRates(e.Counters)
		emails = append(emails, &conn.HubspotEmailMonitorEmail{
			CampaignID: e.CampaignID, EmailID: e.EmailID, Name: e.Name, AbVariant: e.ABVariant, Deleted: e.Deleted,
			Sent: c.Sent, Delivered: c.Delivered, Opens: c.Opens, Clicks: c.Clicks,
			Bounces: c.Bounces, Unsubscribes: c.Unsubscribes, SpamReports: c.SpamReports,
			OpenRate: r.OpenRate, ClickRate: r.ClickRate, BounceRate: r.BounceRate, UnsubscribeRate: r.UnsubscribeRate, SpamRate: r.SpamRate,
		})
	}
	tr := rules.HubSpotRates(sum)
	out := &conn.HubspotEmailMonitor{
		Days:        days,
		Emails:      emails,
		ActionItems: toConnAccountMonitorActionItems(rules.EvaluateHubSpotMonitor(read.Emails)),
		Totals: &conn.HubspotEmailMonitorTotals{
			EmailCount: len(read.Emails),
			Sent:       sum.Sent, Delivered: sum.Delivered, Opens: sum.Opens, Clicks: sum.Clicks,
			Bounces: sum.Bounces, Unsubscribes: sum.Unsubscribes, SpamReports: sum.SpamReports,
			OpenRate: tr.OpenRate, ClickRate: tr.ClickRate, BounceRate: tr.BounceRate, UnsubscribeRate: tr.UnsubscribeRate, SpamRate: tr.SpamRate,
		},
		EmailsChecked:         read.EmailsChecked,
		EmailsNotSentInWindow: read.EmailsNotSentInWindow,
		EmailsUnattributable:  read.EmailsUnattributable,
		EmailsTruncated:       read.Truncated,
	}
	// All three or none: they are set exactly when HubSpot was read. An empty scope never calls
	// HubSpot, and a window or as-of stated for a read that did not happen would claim one did.
	if !read.AsOf.IsZero() && !read.SpanStart.IsZero() && !read.SpanEnd.IsZero() {
		// RFC3339Nano, not RFC3339: truncating 14:30:00.900 to 14:30:00 would publish an instant
		// EARLIER than the last response, breaking the upper-bound guarantee metrics_as_of makes.
		asOf := read.AsOf.UTC().Format(time.RFC3339Nano)
		ws := read.SpanStart.UTC().Format(time.DateOnly)
		we := read.SpanEnd.UTC().Format(time.DateOnly)
		out.MetricsAsOf, out.MetricsWindowStart, out.MetricsWindowEnd = &asOf, &ws, &we
	}
	return out
}
