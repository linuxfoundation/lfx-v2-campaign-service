// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// healthy is an email no rule fires on: 30% open, 4% click, 1% bounce, 0.2% unsubscribe, no spam.
func healthy() model.HubSpotEmailCounters {
	return model.HubSpotEmailCounters{Sent: 1000, Delivered: 990, Opens: 297, Clicks: 40, Bounces: 10, Unsubscribes: 2}
}

func email(c model.HubSpotEmailCounters) model.HubSpotMonitorEmail {
	return model.HubSpotMonitorEmail{CampaignID: "camp-1", EmailID: "4242", Name: "Newsletter", Counters: c}
}

// only returns the single finding for c, failing unless exactly one fired.
func only(t *testing.T, c model.HubSpotEmailCounters) model.AccountMonitorActionItem {
	t.Helper()
	items := EvaluateHubSpotMonitor([]model.HubSpotMonitorEmail{email(c)})
	if len(items) != 1 {
		t.Fatalf("findings = %+v, want exactly one", items)
	}
	return items[0]
}

func TestEvaluateHubSpotMonitor_HealthyEmailHasNoFindings(t *testing.T) {
	if items := EvaluateHubSpotMonitor([]model.HubSpotMonitorEmail{email(healthy())}); len(items) != 0 {
		t.Fatalf("findings on a healthy email: %+v", items)
	}
	if items := EvaluateHubSpotMonitor(nil); items == nil || len(items) != 0 {
		t.Fatalf("no emails must give an empty, non-nil list, got %#v", items)
	}
}

func TestEvaluateHubSpotMonitor_SentButNothingDeliveredIsHigh(t *testing.T) {
	// Not volume-gated: one recipient is enough.
	it := only(t, model.HubSpotEmailCounters{Sent: 1})
	if it.Priority != model.MonitorPriorityHigh || !strings.Contains(it.Issue, "none delivered") {
		t.Errorf("finding = %+v", it)
	}
	if it.CampaignID != "camp-1" || it.EmailID != "4242" || it.CampaignName != "Newsletter" {
		t.Errorf("finding must carry the service campaign id AND the HubSpot email id: %+v", it)
	}
	// Nothing sent at all is not a delivery failure.
	if items := EvaluateHubSpotMonitor([]model.HubSpotMonitorEmail{email(model.HubSpotEmailCounters{})}); len(items) != 0 {
		t.Errorf("an email with no sends produced findings: %+v", items)
	}
}

func TestEvaluateHubSpotMonitor_BounceRateBands(t *testing.T) {
	c := healthy()
	c.Bounces = 20 // exactly 2%: not above the MED threshold
	if items := EvaluateHubSpotMonitor([]model.HubSpotMonitorEmail{email(c)}); len(items) != 0 {
		t.Errorf("2.0%% bounce fired: %+v", items)
	}
	c.Bounces = 21
	if it := only(t, c); it.Priority != model.MonitorPriorityMed || !strings.Contains(it.Issue, "2.1%") {
		t.Errorf("2.1%% bounce = %+v, want MED", it)
	}
	c.Bounces = 51
	if it := only(t, c); it.Priority != model.MonitorPriorityHigh || !strings.Contains(it.Issue, "5.1%") {
		t.Errorf("5.1%% bounce = %+v, want HIGH", it)
	}
	// Below the volume floor a rate is noise: 10 of 99 sent is 10%, and fires nothing.
	small := model.HubSpotEmailCounters{Sent: 99, Delivered: 89, Opens: 30, Clicks: 5, Bounces: 10}
	if items := EvaluateHubSpotMonitor([]model.HubSpotMonitorEmail{email(small)}); len(items) != 0 {
		t.Errorf("a sub-volume send fired: %+v", items)
	}
}

func TestEvaluateHubSpotMonitor_SpamComplaintBands(t *testing.T) {
	none := func(c model.HubSpotEmailCounters, why string) {
		t.Helper()
		if items := EvaluateHubSpotMonitor([]model.HubSpotMonitorEmail{email(c)}); len(items) != 0 {
			t.Errorf("%s fired: %+v", why, items)
		}
	}
	// ONE complaint on 100 delivered is 1% — far above both rates — and is still no finding:
	// a single recipient is not a list-quality signal.
	none(model.HubSpotEmailCounters{Sent: 100, Delivered: 100, Opens: 30, Clicks: 4, SpamReports: 1}, "1 report / 100 delivered")
	c := healthy()    // 990 delivered
	c.SpamReports = 2 // 0.2%: above the MED rate, below the MED count
	none(c, "2 reports")
	c.SpamReports = 3 // 0.303%: at the HIGH rate, MED count only
	if it := only(t, c); it.Priority != model.MonitorPriorityMed {
		t.Errorf("3 reports at 0.3%% = %+v, want MED (HIGH needs 5)", it)
	}
	c.SpamReports = 4
	if it := only(t, c); it.Priority != model.MonitorPriorityMed {
		t.Errorf("4 reports = %+v, want MED", it)
	}
	c.SpamReports = 5 // 0.505%
	if it := only(t, c); it.Priority != model.MonitorPriorityHigh {
		t.Errorf("5 reports at 0.5%% = %+v, want HIGH", it)
	}
	// Enough reports but the rate is at the MED line, not above it: 3 / 3000 = exactly 0.1%.
	none(model.HubSpotEmailCounters{Sent: 3010, Delivered: 3000, Opens: 900, Clicks: 120, SpamReports: 3}, "exactly 0.1% with 3 reports")
	// Five reports but below the HIGH rate: 5 / 2000 = 0.25% → MED.
	if it := only(t, model.HubSpotEmailCounters{Sent: 2010, Delivered: 2000, Opens: 600, Clicks: 80, SpamReports: 5}); it.Priority != model.MonitorPriorityMed {
		t.Errorf("5 reports at 0.25%% = %+v, want MED", it)
	}
}

func TestEvaluateHubSpotMonitor_EngagementRules(t *testing.T) {
	c := healthy()
	c.Unsubscribes = 11 // 1.1%
	if it := only(t, c); it.Priority != model.MonitorPriorityMed || !strings.Contains(it.Issue, "Unsubscribe rate") {
		t.Errorf("unsubscribe = %+v", it)
	}
	c = healthy()
	c.Opens = 148 // 14.9%
	if it := only(t, c); it.Priority != model.MonitorPriorityMed || !strings.Contains(it.Issue, "Open rate") {
		t.Errorf("open = %+v", it)
	}
	c = healthy()
	c.Clicks = 9 // 0.9%
	if it := only(t, c); it.Priority != model.MonitorPriorityLow || !strings.Contains(it.Issue, "Click rate") {
		t.Errorf("click = %+v", it)
	}
}

func TestEvaluateHubSpotMonitor_SortsHighFirst(t *testing.T) {
	low := healthy()
	low.Clicks = 1
	items := EvaluateHubSpotMonitor([]model.HubSpotMonitorEmail{email(low), email(model.HubSpotEmailCounters{Sent: 5})})
	if len(items) != 2 || items[0].Priority != model.MonitorPriorityHigh || items[1].Priority != model.MonitorPriorityLow {
		t.Fatalf("order = %+v, want HIGH then LOW", items)
	}
}

func TestHubSpotRates_ZeroDenominatorsAreAbsent(t *testing.T) {
	r := HubSpotRates(model.HubSpotEmailCounters{})
	if r.OpenRate != nil || r.ClickRate != nil || r.BounceRate != nil || r.UnsubscribeRate != nil || r.SpamRate != nil {
		t.Fatalf("rates over zero = %+v, want all absent", r)
	}
	r = HubSpotRates(model.HubSpotEmailCounters{Sent: 10, Bounces: 10})
	if r.BounceRate == nil || *r.BounceRate != 1 || r.OpenRate != nil {
		t.Fatalf("sent but nothing delivered = %+v, want bounce 1 and delivered-based rates absent", r)
	}
	r = HubSpotRates(model.HubSpotEmailCounters{Sent: 200, Delivered: 100, Opens: 25, Clicks: 5, Bounces: 10, Unsubscribes: 1, SpamReports: 2})
	if *r.SpamRate != 0.02 {
		t.Fatalf("spam rate = %v, want 0.02", *r.SpamRate)
	}
	if *r.OpenRate != 0.25 || *r.ClickRate != 0.05 || *r.BounceRate != 0.05 || *r.UnsubscribeRate != 0.01 {
		t.Fatalf("rates = open %v click %v bounce %v unsub %v", *r.OpenRate, *r.ClickRate, *r.BounceRate, *r.UnsubscribeRate)
	}
}
