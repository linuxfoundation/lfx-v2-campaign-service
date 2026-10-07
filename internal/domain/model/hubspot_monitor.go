// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "time"

// HubSpotEmailCounters is one marketing email's statistics counters as HubSpot reports them: the
// email's totals to the moment they were read, NOT counts of events inside the monitor's window
// (HubSpot's statistics span selects WHICH emails by send date; see
// internal/platform/hubspot.GetEmailMetrics). There is no cost field on purpose: HubSpot bills
// nothing per send, so a cost here could only be invented.
type HubSpotEmailCounters struct {
	Sent         int64
	Delivered    int64
	Opens        int64
	Clicks       int64
	Bounces      int64
	Unsubscribes int64
	SpamReports  int64
}

// Add returns the field-wise sum of c and o.
func (c HubSpotEmailCounters) Add(o HubSpotEmailCounters) HubSpotEmailCounters {
	return HubSpotEmailCounters{
		Sent:         c.Sent + o.Sent,
		Delivered:    c.Delivered + o.Delivered,
		Opens:        c.Opens + o.Opens,
		Clicks:       c.Clicks + o.Clicks,
		Bounces:      c.Bounces + o.Bounces,
		Unsubscribes: c.Unsubscribes + o.Unsubscribes,
		SpamReports:  c.SpamReports + o.SpamReports,
	}
}

// HubSpotEmailRates are the rates derived from a HubSpotEmailCounters. Each is a FRACTION (0..1,
// not a percentage) and is nil when its denominator is zero: a rate over nothing is unknown, not
// 0, and a 0 would read as a measurement (rules.HubSpotRates computes them).
type HubSpotEmailRates struct {
	// OpenRate is Opens / Delivered.
	OpenRate *float64
	// ClickRate is Clicks / Delivered.
	ClickRate *float64
	// BounceRate is Bounces / Sent.
	BounceRate *float64
	// UnsubscribeRate is Unsubscribes / Delivered.
	UnsubscribeRate *float64
}

// HubSpotMonitorEmail is one marketing email the email account monitor read: an email THIS
// SERVICE created for the project (a campaign row's own email, or its recorded A/B variant), that
// HubSpot reports as sent within the monitor's span.
type HubSpotMonitorEmail struct {
	// CampaignID is this service's campaign UUID the email belongs to.
	CampaignID string
	// EmailID is the HubSpot marketing-email id.
	EmailID string
	// Name is the email's name as this service recorded it at creation.
	Name string
	// ABVariant is true for the A/B test's variant ("B") email, recorded on its parent's row.
	ABVariant bool
	Counters  HubSpotEmailCounters
}

// HubSpotEmailMonitorRead is what the email account monitor read returns (Orchestrator.
// ReadHubSpotEmailMonitor). Emails is never nil on success.
//
// The span and as-of are ZERO when HubSpot was not called at all — the project has recorded no
// HubSpot email — and set otherwise.
type HubSpotEmailMonitorRead struct {
	Emails []HubSpotMonitorEmail
	// SpanStart / SpanEnd are the first and last UTC calendar day (inclusive) of the SEND-time
	// span the emails were selected by.
	SpanStart time.Time
	SpanEnd   time.Time
	// AsOf is the instant the counters were read; they are totals to that moment.
	AsOf time.Time
	// EmailsChecked is how many of the project's recorded emails were asked about.
	EmailsChecked int
	// EmailsNotSentInWindow is how many of those HubSpot reported no send for inside the span —
	// sent outside it, never sent (a staged draft), or no longer existing; HubSpot's answer does
	// not distinguish the three.
	EmailsNotSentInWindow int
	// EmailsUnattributable is how many recorded emails could not be read safely: the row does not
	// record the portal the email was created in, records a different portal than the one the
	// project's token now reaches (an email id is meaningful only inside its portal), or carries
	// a malformed id. They are not read at all.
	EmailsUnattributable int
	// Truncated is true when the project has recorded more HubSpot campaigns than the monitor
	// reads; only the most recently recorded ones were checked.
	Truncated bool
}
