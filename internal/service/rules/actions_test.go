// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package rules

import (
	"strings"
	"testing"
)

func rulesFired(items []ActionItem) []string {
	out := make([]string, 0, len(items))
	for _, i := range items {
		out = append(out, i.Rule)
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// Zero delivery needs BOTH no impressions and no spend. Either alone is an artefact — an
// unbilled serve, or a billing entry with no serve — and flagging those trains operators to
// ignore the rule.
func TestEvaluate_ZeroDeliveryNeedsBothSignals(t *testing.T) {
	base := Input{CampaignID: "c1", Platform: "reddit-ads", Status: "created"}

	if got := rulesFired(Evaluate(base)); !contains(got, "zero_delivery") {
		t.Errorf("no impressions and no spend did not fire zero_delivery; fired %v", got)
	}

	withImpressions := base
	withImpressions.Impressions = 500
	if got := rulesFired(Evaluate(withImpressions)); contains(got, "zero_delivery") {
		t.Error("zero_delivery fired on a campaign that IS serving, just not billing")
	}

	withSpend := base
	withSpend.Spend = 12
	if got := rulesFired(Evaluate(withSpend)); contains(got, "zero_delivery") {
		t.Error("zero_delivery fired on a campaign with recorded spend")
	}
}

// Only a campaign the service believes exists upstream can be failing to deliver. A pending
// claim has not necessarily reached the platform, so a zero-delivery item would report a
// dispatch problem as a targeting one.
func TestEvaluate_ZeroDeliveryOnlyForLiveStatuses(t *testing.T) {
	fires := map[string]bool{
		"created":          true,
		"created_degraded": true, // it IS delivering; some variants failed
		"pending":          false,
		"unconfirmed":      false,
		"group_created":    false,
		"deleted":          false,
	}
	for status, want := range fires {
		t.Run(status, func(t *testing.T) {
			got := contains(rulesFired(Evaluate(Input{CampaignID: "c1", Status: status})), "zero_delivery")
			if got != want {
				t.Errorf("status %q fired=%v, want %v", status, got, want)
			}
		})
	}
}

// An incomputable pacing must not produce a pacing item. Pct is zero there because nothing was
// measured, so firing "underspending at 0%" would report the absence of a budget as a finding
// about spend.
func TestEvaluate_NoPacingItemWhenPacingIsIncomputable(t *testing.T) {
	// The label carries a REAL band while Computable is false — the shape a caller produces by
	// copying a label across without the flag. Using PacingUnknown here would prove nothing:
	// it matches no switch arm, so the guard could be deleted and the test would still pass.
	in := Input{
		CampaignID: "c1", Status: "created", Impressions: 5000, Spend: 100,
		Pacing: Pacing{Pct: 0, Label: PacingUnderspending, Computable: false},
	}
	for _, r := range rulesFired(Evaluate(in)) {
		if r == "underspending" || r == "budget_constrained" {
			t.Errorf("%s fired on an incomputable pacing — that is the absence of a budget, not a spend finding", r)
		}
	}
}

func TestEvaluate_PacingItems(t *testing.T) {
	cases := map[string]struct {
		label PacingLabel
		want  string
	}{
		"underspending": {PacingUnderspending, "underspending"},
		"constrained":   {PacingConstrained, "budget_constrained"},
		"overspending":  {PacingOverspending, "budget_constrained"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			in := Input{
				CampaignID: "c1", Status: "created", Impressions: 5000, Spend: 100,
				Pacing: Pacing{Pct: 42, Label: tc.label, Computable: true},
			}
			if got := rulesFired(Evaluate(in)); !contains(got, tc.want) {
				t.Errorf("label %q fired %v, want %q", tc.label, got, tc.want)
			}
		})
	}
	// A healthy campaign raises nothing.
	healthy := Input{
		CampaignID: "c1", Status: "created", Impressions: 5000, Clicks: 100, CTRPct: 2,
		Spend: 100, Pacing: Pacing{Pct: 95, Label: PacingNormal, Computable: true},
	}
	if got := Evaluate(healthy); len(got) != 0 {
		t.Errorf("a campaign on plan with healthy CTR raised %v", rulesFired(got))
	}
}

// Low CTR needs enough impressions to mean anything. Three impressions and no clicks is a 0%
// CTR that says nothing about the creative.
func TestEvaluate_LowCTRNeedsDelivery(t *testing.T) {
	thin := Input{CampaignID: "c1", Status: "created", Impressions: 50, Clicks: 0, CTRPct: 0, Spend: 1}
	if got := rulesFired(Evaluate(thin)); contains(got, "low_ctr") {
		t.Error("low_ctr fired on 50 impressions; the figure is not yet meaningful")
	}

	delivered := Input{CampaignID: "c1", Status: "created", Impressions: 20000, Clicks: 20, CTRPct: 0.1, Spend: 100}
	if got := rulesFired(Evaluate(delivered)); !contains(got, "low_ctr") {
		t.Errorf("low_ctr did not fire on 0.1%% across 20k impressions; fired %v", got)
	}

	// And a healthy CTR at the same delivery must NOT fire, or the impressions floor alone
	// would satisfy the test.
	healthy := delivered
	healthy.CTRPct = 1.5
	if got := rulesFired(Evaluate(healthy)); contains(got, "low_ctr") {
		t.Error("low_ctr fired on a healthy 1.5% CTR")
	}
}

// Every item names the campaign and carries a stable rule token, so a consumer can group or
// link without parsing prose that is free to be reworded.
func TestEvaluate_ItemsCarryIdentityAndAStableToken(t *testing.T) {
	in := Input{
		CampaignID: "c-42", Platform: "meta-ads", Status: "created",
		Impressions: 20000, CTRPct: 0.05, Spend: 10,
		Pacing: Pacing{Pct: 10, Label: PacingUnderspending, Computable: true},
	}
	items := Evaluate(in)
	if len(items) < 2 {
		t.Fatalf("expected both a pacing and a CTR item, got %v", rulesFired(items))
	}
	for _, item := range items {
		if item.CampaignID != "c-42" || item.Platform != "meta-ads" {
			t.Errorf("item %q lost its campaign identity: %+v", item.Rule, item)
		}
		if item.Rule == "" || strings.Contains(item.Rule, " ") {
			t.Errorf("rule token %q is not a stable identifier", item.Rule)
		}
		if item.Issue == "" || item.Action == "" {
			t.Errorf("item %q has no issue or no action for the operator", item.Rule)
		}
	}
}

// Both low_ctr boundaries. Neither constant had a test at its edge, so either could move by one
// without failing anything — and both are deliberate judgements the comments describe at length.
func TestEvaluate_LowCTRBoundaries(t *testing.T) {
	base := Input{CampaignID: "c1", Platform: "google-ads", Status: "created", Spend: 100}

	t.Run("impressions floor", func(t *testing.T) {
		// One impression below the floor the figure means nothing; at the floor it does.
		below := base
		below.Impressions, below.CTRPct = minImpressionsForCTR-1, 0.1
		if contains(rulesFired(Evaluate(below)), "low_ctr") {
			t.Errorf("low_ctr fired at %d impressions, one below the floor", below.Impressions)
		}
		at := base
		at.Impressions, at.CTRPct = minImpressionsForCTR, 0.1
		if !contains(rulesFired(Evaluate(at)), "low_ctr") {
			t.Errorf("low_ctr did not fire at exactly %d impressions, the floor", at.Impressions)
		}
	})

	t.Run("ctr threshold", func(t *testing.T) {
		// The threshold is the bottom of the ACCEPTABLE band: exactly at it is not low.
		at := base
		at.Impressions, at.CTRPct = 20000, LowCTRThresholdPct
		if contains(rulesFired(Evaluate(at)), "low_ctr") {
			t.Errorf("low_ctr fired at exactly %.2f%%, which is the threshold, not below it", LowCTRThresholdPct)
		}
		below := base
		below.Impressions, below.CTRPct = 20000, LowCTRThresholdPct-0.01
		if !contains(rulesFired(Evaluate(below)), "low_ctr") {
			t.Errorf("low_ctr did not fire at %.2f%%, below the %.2f%% threshold", below.CTRPct, LowCTRThresholdPct)
		}
	})
}
