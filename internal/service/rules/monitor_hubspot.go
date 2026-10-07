// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"fmt"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The HubSpot email monitor's rule set (LFXV2-2665). Not a port — the BFF never monitored email —
// and not a pacing engine: an email has no budget and no spend, so there is no ladder to place it
// on and no pacing row. What it shares with the six ad-platform engines is the finding shape
// (model.AccountMonitorActionItem), the HIGH/MED/LOW scale and its ordering (sortByPriority, from
// monitor_shared.go — do not copy it here).
//
// Every threshold below is a HEURISTIC, not a HubSpot limit or a contractual figure: they are
// common deliverability rules of thumb, chosen so a finding means "look at this email", and they
// are named constants so a change is one edit with its reason beside it. Rates are fractions of
// the counter named on each, computed by HubSpotRates AFTER the counters are read (never taken
// from HubSpot's own `ratios`, so every consumer gets one definition), and a rate whose
// denominator is zero is unknown — no rule fires on it.
//
// HubSpot's counters are each email's totals to the moment of the read, so a finding about an
// email sent minutes ago can describe a send still in progress. The zero-delivery rule says so in
// its own text, since it is the one that can fire on a send that has simply not finished.
const (
	// hsMinVolume is the delivered (or, for bounces, sent) count BELOW which a rate is noise and
	// no rate rule fires: one bounce in 20 sends is 5%, and says nothing about the list.
	hsMinVolume = 100
	// hsBounceRateHigh / hsBounceRateMed: bounces / sent. Above ~2% a list is usually stale or
	// unverified; above ~5% mailbox providers start treating the sender as a list-quality risk.
	hsBounceRateHigh = 0.05
	hsBounceRateMed  = 0.02
	// hsSpamRateHigh / hsSpamRateMed: spam reports / delivered. Gmail's and Yahoo's bulk-sender
	// guidance names 0.3% as the rate never to reach and asks senders to stay under 0.1%.
	hsSpamRateHigh = 0.003
	hsSpamRateMed  = 0.001
	// hsUnsubscribeRateMed: unsubscribes / delivered. Typical marketing sends lose well under
	// 0.5% of recipients; above 1% the audience or the frequency is a poor fit.
	hsUnsubscribeRateMed = 0.01
	// hsOpenRateLow: opens / delivered. Below ~15% the subject line, sender or audience is
	// underperforming. Opens are INFLATED by privacy features that pre-fetch images (Apple Mail
	// Privacy Protection), so this is a floor that can only under-report a problem, never invent
	// one.
	hsOpenRateLow = 0.15
	// hsClickRateLow: clicks / delivered. Below ~1% the content is not earning action.
	hsClickRateLow = 0.01
)

// hsRate returns num/den as a fraction, or nil when den is zero.
func hsRate(num, den int64) *float64 {
	if den <= 0 {
		return nil
	}
	r := float64(num) / float64(den)
	return &r
}

// HubSpotRates derives an email's (or a sum of emails') rates from its counters. Call it on
// SUMMED counters for a total — averaging per-email rates would weight a 50-recipient test send
// the same as a 50,000-recipient newsletter.
func HubSpotRates(c model.HubSpotEmailCounters) model.HubSpotEmailRates {
	return model.HubSpotEmailRates{
		OpenRate:        hsRate(c.Opens, c.Delivered),
		ClickRate:       hsRate(c.Clicks, c.Delivered),
		BounceRate:      hsRate(c.Bounces, c.Sent),
		UnsubscribeRate: hsRate(c.Unsubscribes, c.Delivered),
	}
}

func hsPct(f float64) string { return fmt.Sprintf("%.1f%%", f*100) }

// EvaluateHubSpotMonitor returns the findings for the emails the monitor read, HIGH first. Every
// rule is evaluated independently per email; an email can carry several findings.
func EvaluateHubSpotMonitor(emails []model.HubSpotMonitorEmail) []model.AccountMonitorActionItem {
	items := make([]model.AccountMonitorActionItem, 0)
	for _, e := range emails {
		items = append(items, hubspotEmailItems(e)...)
	}
	sortByPriority(items)
	return items
}

func hubspotEmailItems(e model.HubSpotMonitorEmail) []model.AccountMonitorActionItem {
	var items []model.AccountMonitorActionItem
	add := func(priority model.MonitorPriority, issue, action string) {
		items = append(items, model.AccountMonitorActionItem{
			CampaignID: e.EmailID, CampaignName: e.Name,
			Priority: priority, Issue: issue, Action: action,
		})
	}
	c := e.Counters
	r := HubSpotRates(c)

	// HIGH — sent, but nothing arrived. Not volume-gated: any send with zero deliveries is either
	// still in progress or failed outright, and both are worth a look.
	if c.Sent > 0 && c.Delivered == 0 {
		add(model.MonitorPriorityHigh,
			fmt.Sprintf("Sent to %d recipients but none delivered", c.Sent),
			"If the send finished more than an hour ago, check the email's bounce and suppression details in HubSpot and the sending domain's authentication (SPF, DKIM, DMARC)")
	}

	if r.BounceRate != nil && c.Sent >= hsMinVolume {
		switch {
		case *r.BounceRate > hsBounceRateHigh:
			add(model.MonitorPriorityHigh,
				fmt.Sprintf("Bounce rate is %s (%d of %d sent) — above the 5%% level that puts sender reputation at risk", hsPct(*r.BounceRate), c.Bounces, c.Sent),
				"Remove hard-bounced and unverified contacts from the send list before the next send, and check the list's source")
		case *r.BounceRate > hsBounceRateMed:
			add(model.MonitorPriorityMed,
				fmt.Sprintf("Bounce rate is %s (%d of %d sent) — above the 2%% healthy-list level", hsPct(*r.BounceRate), c.Bounces, c.Sent),
				"Review the send list for stale or unverified contacts")
		}
	}

	if c.Delivered >= hsMinVolume {
		spam := hsRate(c.SpamReports, c.Delivered)
		switch {
		case spam != nil && *spam >= hsSpamRateHigh:
			add(model.MonitorPriorityHigh,
				fmt.Sprintf("Spam complaint rate is %s (%d reports) — at or above the 0.3%% mailbox providers enforce", hsPct(*spam), c.SpamReports),
				"Pause similar sends and confirm every recipient opted in; tighten the audience before sending again")
		case spam != nil && *spam > hsSpamRateMed:
			add(model.MonitorPriorityMed,
				fmt.Sprintf("Spam complaint rate is %s (%d reports) — above the 0.1%% target", hsPct(*spam), c.SpamReports),
				"Check that recipients expected this email and that the unsubscribe link is prominent")
		}
		if r.UnsubscribeRate != nil && *r.UnsubscribeRate > hsUnsubscribeRateMed {
			add(model.MonitorPriorityMed,
				fmt.Sprintf("Unsubscribe rate is %s (%d of %d delivered)", hsPct(*r.UnsubscribeRate), c.Unsubscribes, c.Delivered),
				"Check the audience's fit for this content and the send frequency")
		}
		if r.OpenRate != nil && *r.OpenRate < hsOpenRateLow {
			add(model.MonitorPriorityMed,
				fmt.Sprintf("Open rate is %s (%d of %d delivered) — below the 15%% benchmark", hsPct(*r.OpenRate), c.Opens, c.Delivered),
				"Test a different subject line, preview text or sender name, and check the audience is current")
		}
		if r.ClickRate != nil && *r.ClickRate < hsClickRateLow {
			add(model.MonitorPriorityLow,
				fmt.Sprintf("Click rate is %s (%d of %d delivered) — below the 1%% benchmark", hsPct(*r.ClickRate), c.Clicks, c.Delivered),
				"Make the call to action more prominent and check every link resolves")
		}
	}
	return items
}
