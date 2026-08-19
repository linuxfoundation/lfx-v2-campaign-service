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
	base := Input{CampaignID: "c1", Platform: "reddit-ads", Status: "created", BillsPerDelivery: true, DeliveryExpected: true}

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
		"created_degraded": true,  // it IS delivering; some variants failed
		"active":           true,  // the RUN state a toggle sets — the clearest case of all
		"paused":           false, // deliberately stopped; zero spend is the intended outcome
		"pending":          false,
		"unconfirmed":      false,
		"group_created":    false,
		"deleted":          false,
	}
	for status, want := range fires {
		t.Run(status, func(t *testing.T) {
			got := contains(rulesFired(Evaluate(Input{CampaignID: "c1", Status: status, BillsPerDelivery: true, DeliveryExpected: true})), "zero_delivery")
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
		CampaignID: "c1", Status: "created", BillsPerDelivery: true, DeliveryExpected: true, Impressions: 5000, Spend: 100,
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
				CampaignID: "c1", Status: "created", BillsPerDelivery: true, DeliveryExpected: true, Impressions: 5000, Spend: 100,
				Pacing: Pacing{Pct: 42, Label: tc.label, Computable: true},
			}
			if got := rulesFired(Evaluate(in)); !contains(got, tc.want) {
				t.Errorf("label %q fired %v, want %q", tc.label, got, tc.want)
			}
		})
	}
	// A healthy campaign raises nothing.
	healthy := Input{
		CampaignID: "c1", Status: "created", BillsPerDelivery: true, DeliveryExpected: true, Impressions: 5000, Clicks: 100, CTRPct: 2,
		Spend: 100, Pacing: Pacing{Pct: 95, Label: PacingNormal, Computable: true},
	}
	if got := Evaluate(healthy); len(got) != 0 {
		t.Errorf("a campaign on plan with healthy CTR raised %v", rulesFired(got))
	}
}

// Low CTR needs enough impressions to mean anything. Three impressions and no clicks is a 0%
// CTR that says nothing about the creative.
func TestEvaluate_LowCTRNeedsDelivery(t *testing.T) {
	thin := Input{
		CampaignID: "c1", Status: "created", BillsPerDelivery: true, DeliveryExpected: true, Impressions: 50, Clicks: 0, CTRPct: 0, Spend: 1}
	if got := rulesFired(Evaluate(thin)); contains(got, "low_ctr") {
		t.Error("low_ctr fired on 50 impressions; the figure is not yet meaningful")
	}

	delivered := Input{
		CampaignID: "c1", Status: "created", BillsPerDelivery: true, DeliveryExpected: true, Impressions: 20000, Clicks: 20, CTRPct: 0.1, Spend: 100}
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
		CampaignID: "c-42", Platform: "meta-ads", Status: "created", BillsPerDelivery: true, DeliveryExpected: true,
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
	base := Input{CampaignID: "c1", Platform: "google-ads", Status: "created", Spend: 100, BillsPerDelivery: true, DeliveryExpected: true}

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

// A campaign that never started is trivially at 0% of plan, so the pacing item is
// arithmetically true and actively misleading: the two items carry OPPOSITE remedies —
// zero_delivery says no budget change will fix this, underspending says to adjust the budget.
func TestEvaluate_ZeroDeliverySuppressesThePacingItem(t *testing.T) {
	in := Input{
		CampaignID: "c1", Platform: "google-ads", Status: "created", BillsPerDelivery: true, DeliveryExpected: true,
		Impressions: 0, Clicks: 0, Spend: 0,
		Pacing: Pacing{Pct: 0, Label: PacingUnderspending, Computable: true},
	}
	fired := rulesFired(Evaluate(in))
	if !contains(fired, "zero_delivery") {
		t.Fatalf("zero_delivery did not fire; got %v", fired)
	}
	if contains(fired, "underspending") {
		t.Errorf("both zero_delivery and underspending fired (%v) — their remedies contradict each other", fired)
	}

	// But a DELIVERING campaign that is genuinely behind still raises the pacing item, or the
	// suppression would have disabled the rule wholesale.
	delivering := in
	delivering.Impressions, delivering.Spend = 5000, 20
	if got := rulesFired(Evaluate(delivering)); !contains(got, "underspending") {
		t.Errorf("underspending did not fire for a delivering campaign behind plan; got %v", got)
	}
}

// The email channel bills nothing per send, so "no spend" is its normal state. HubSpot also maps
// opens onto Impressions, so a delivered email nobody opened is indistinguishable from a campaign
// that never ran — and zero_delivery would tell an operator to check targeting and approval for
// an email that was delivered exactly as intended.
func TestEvaluate_ZeroDeliveryIsPaidAdsOnly(t *testing.T) {
	email := Input{
		CampaignID: "c1", Platform: "hubspot", Status: "created", BillsPerDelivery: false,
		Impressions: 0, Clicks: 0, Spend: 0,
	}
	if got := rulesFired(Evaluate(email)); contains(got, "zero_delivery") {
		t.Errorf("zero_delivery fired on an email send with no opens; got %v", got)
	}

	// The identical numbers on a paid channel DO fire, or the test above would pass for any
	// reason at all.
	paid := email
	paid.Platform, paid.BillsPerDelivery, paid.DeliveryExpected = "google-ads", true, true
	if got := rulesFired(Evaluate(paid)); !contains(got, "zero_delivery") {
		t.Errorf("zero_delivery did not fire on a paid campaign with no delivery; got %v", got)
	}
}

// A campaign that has not started has delivered nothing for the same reason it has spent
// nothing. Firing zero_delivery there tells an operator to check targeting and creative approval
// for a campaign whose only property is being early.
func TestEvaluate_ZeroDeliveryWaitsForTheFlight(t *testing.T) {
	early := Input{
		CampaignID: "c1", Platform: "google-ads", Status: "created",
		BillsPerDelivery: true, DeliveryExpected: false,
		Impressions: 0, Clicks: 0, Spend: 0,
	}
	if got := rulesFired(Evaluate(early)); contains(got, "zero_delivery") {
		t.Errorf("zero_delivery fired before the flight began; got %v", got)
	}

	// Once delivery IS expected the identical numbers fire, or the assertion above would pass
	// for any reason at all.
	started := early
	started.DeliveryExpected = true
	if got := rulesFired(Evaluate(started)); !contains(got, "zero_delivery") {
		t.Errorf("zero_delivery did not fire for a started campaign with no delivery; got %v", got)
	}
}

// Campaign.Status carries TWO kinds of value — a provisioning state stamped at create
// (pending/created/created_degraded) and a RUN state set by the toggle (active/paused). The
// pacing rules must respect the run state too, not just zero_delivery.
//
// Before this, `paused` and `pending` both raised underspending — telling an operator to fix a
// campaign they had deliberately stopped, or one that may never have reached the platform —
// while `active`, the one state where delivery is genuinely expected, raised nothing at all.
func TestEvaluate_PacingRespectsTheRunState(t *testing.T) {
	base := Input{
		CampaignID: "c1", Platform: "google-ads", BillsPerDelivery: true, DeliveryExpected: true,
		Impressions: 5000, Clicks: 100, CTRPct: 2, Spend: 10,
		Pacing: Pacing{Pct: 20, Label: PacingUnderspending, Computable: true},
	}
	wants := map[string]bool{
		"active":           true,
		"created":          true,
		"created_degraded": true,
		"paused":           false,
		"pending":          false,
		"deleted":          false,
	}
	for status, want := range wants {
		t.Run(status, func(t *testing.T) {
			in := base
			in.Status = status
			got := contains(rulesFired(Evaluate(in)), "underspending")
			if got != want {
				t.Errorf("status %q raised underspending=%v, want %v", status, got, want)
			}
		})
	}
}

// TestEvaluate_EveryRuleRespectsTheRunState pins the catalog's contract without exception:
// "Every rule is gated on the campaign's status ... a `paused` campaign raises nothing."
//
// The pre-existing run-state test used CTRPct: 2 — above LowCTRThresholdPct — so the low_ctr
// rule could not fire in it either way, and its ungated state went unnoticed. This input is
// chosen to trip EVERY rule if the gate were removed from any of them.
func TestEvaluate_EveryRuleRespectsTheRunState(t *testing.T) {
	paused := Input{
		CampaignID: "c1", Platform: "google_ads", Status: "paused",
		BillsPerDelivery: true, DeliveryExpected: true,
		Impressions: 20000, Clicks: 20, CTRPct: 0.1, Spend: 100,
		Pacing: Pacing{Computable: true, Pct: 10, Label: PacingUnderspending},
	}
	if got := Evaluate(paused); len(got) != 0 {
		for _, it := range got {
			t.Errorf("a paused campaign raised %q; the catalog says it raises nothing", it.Rule)
		}
	}

	// The mirror case: the same input on an active campaign MUST still raise, or the test
	// above would pass against a rule set that never fires at all.
	active := paused
	active.Status = "active"
	if got := Evaluate(active); len(got) == 0 {
		t.Error("the same input on an active campaign raised nothing; the gate is over-broad")
	}
}

// int64p is a local helper for building the Conversions pointer, which every test below
// needs because the field's whole purpose is to distinguish nil from a pointer to zero.
// convp is the conversions helper: the field is a float64 because Google Ads and Microsoft
// both credit fractional conversions.
func convp(v float64) *float64 { return &v }

// The rule's reason for existing: real traffic, no conversions from it.
func TestEvaluate_NoConversionsFiresOnClicksWithoutConversions(t *testing.T) {
	in := Input{
		CampaignID: "c1", Platform: "google-ads", Status: "created",
		Impressions: 40000, Clicks: 800, Conversions: convp(0),
	}
	got := rulesFired(Evaluate(in))
	if !contains(got, "no_conversions") {
		t.Errorf("800 clicks with a measured 0 conversions did not fire no_conversions; fired %v", got)
	}
}

// THE binding test for the pointer. An absent conversion count and a measured zero are the
// same `0` to any int64-shaped field, and they demand opposite responses: a measured zero on
// a converting-capable platform is the finding, while an absent one means the platform never
// reported conversions at all and there is nothing to find.
//
// A rule that fires on both would flag every Meta, X, Reddit and email campaign in the
// account forever — reporting the absence of measurement as a campaign defect.
func TestEvaluate_NoConversionsDistinguishesAbsentFromMeasuredZero(t *testing.T) {
	base := Input{
		CampaignID: "c1", Platform: "meta-ads", Status: "created",
		Impressions: 40000, Clicks: 800,
	}

	absent := base
	absent.Conversions = nil
	if got := rulesFired(Evaluate(absent)); contains(got, "no_conversions") {
		t.Errorf("no_conversions fired on a platform that reports NO conversion count; "+
			"an unmeasured campaign was reported as a failing one; fired %v", got)
	}

	measured := base
	measured.Conversions = convp(0)
	if got := rulesFired(Evaluate(measured)); !contains(got, "no_conversions") {
		t.Errorf("no_conversions did not fire on a MEASURED zero, which is the finding the "+
			"rule exists for; fired %v", got)
	}
}

// A campaign that IS converting is not a finding, however few conversions it has.
func TestEvaluate_NoConversionsSilentWhenConversionsExist(t *testing.T) {
	in := Input{
		CampaignID: "c1", Platform: "google-ads", Status: "created",
		Impressions: 40000, Clicks: 800, Conversions: convp(1),
	}
	if got := rulesFired(Evaluate(in)); contains(got, "no_conversions") {
		t.Errorf("no_conversions fired on a campaign with 1 conversion; fired %v", got)
	}
}

// A FRACTIONAL conversion count is a converting campaign, and must not raise no_conversions.
//
// Google Ads and Microsoft both credit partial conversions under data-driven, position-based
// and offline attribution, so a campaign can genuinely sit at 0.4. Only an upstream value of
// EXACTLY zero is the finding this rule reports. This is the assertion that fails if the
// comparison is ever loosened to a threshold (`< 1`) or if a round-trip through an integer is
// reintroduced anywhere between the adapter and here — either turns a converting campaign
// into a fabricated "no conversions" alert.
func TestEvaluate_NoConversionsSilentOnAFractionalConversion(t *testing.T) {
	for _, conv := range []float64{0.1, 0.4, 0.5, 0.9} {
		in := Input{
			CampaignID: "c1", Platform: "google-ads", Status: "created",
			Impressions: 40000, Clicks: 800, Conversions: convp(conv),
		}
		if got := rulesFired(Evaluate(in)); contains(got, "no_conversions") {
			t.Errorf("no_conversions fired on a campaign credited %v conversions; a partial "+
				"conversion is a conversion, and reporting it as none fabricates the finding "+
				"(fired %v)", conv, got)
		}
	}
}

// The clicks floor, the direct analogue of minImpressionsForCTR. Below it, zero conversions
// is ordinary variance rather than a broken funnel, and flagging it teaches operators to
// dismiss the rule.
//
// Asserted at the BOUNDARY on both sides so the constant is pinned rather than merely
// exercised: a test using 5 and 5000 would pass for any floor between them.
func TestEvaluate_NoConversionsClickFloor(t *testing.T) {
	base := Input{
		CampaignID: "c1", Platform: "google-ads", Status: "created",
		Impressions: 40000, Conversions: convp(0),
	}

	below := base
	below.Clicks = minClicksForConversions - 1
	if got := rulesFired(Evaluate(below)); contains(got, "no_conversions") {
		t.Errorf("no_conversions fired at %d clicks, one BELOW the floor of %d; fired %v",
			below.Clicks, minClicksForConversions, got)
	}

	at := base
	at.Clicks = minClicksForConversions
	if got := rulesFired(Evaluate(at)); !contains(got, "no_conversions") {
		t.Errorf("no_conversions did not fire AT the floor of %d clicks; the floor is "+
			"inclusive; fired %v", minClicksForConversions, got)
	}
}

// docs/api-catalog.md states the contract without exception: "Every rule is gated on the
// campaign's status ... a `paused` campaign raises nothing." A paused campaign's historical
// clicks must not raise a conversions finding against a decision the operator already made.
func TestEvaluate_NoConversionsOnlyForLiveStatuses(t *testing.T) {
	fires := map[string]bool{
		"created":          true,
		"created_degraded": true,
		"active":           true,
		"paused":           false, // deliberately stopped
		"pending":          false, // may not have reached the platform
		"unconfirmed":      false,
		"group_created":    false,
		"deleted":          false,
	}
	for status, want := range fires {
		in := Input{
			CampaignID: "c1", Platform: "google-ads", Status: status,
			Impressions: 40000, Clicks: 800, Conversions: convp(0),
		}
		got := contains(rulesFired(Evaluate(in)), "no_conversions")
		if got != want {
			t.Errorf("status %q: no_conversions fired=%v, want %v", status, got, want)
		}
	}
}

// The item must name the click volume it is reasoning about, so an operator can judge the
// finding without re-deriving it. Asserts the VALUE reaches the prose, not merely that some
// prose exists.
func TestEvaluate_NoConversionsItemShape(t *testing.T) {
	in := Input{
		CampaignID: "c-42", Platform: "linkedin-ads", Status: "active",
		Impressions: 40000, Clicks: 917, Conversions: convp(0),
	}
	var item *ActionItem
	for _, i := range Evaluate(in) {
		if i.Rule == "no_conversions" {
			candidate := i
			item = &candidate
		}
	}
	if item == nil {
		t.Fatal("no_conversions did not fire")
	}
	if item.Priority != PriorityHigh {
		t.Errorf("priority = %q, want %q: spending on traffic that never converts is a "+
			"HIGH finding", item.Priority, PriorityHigh)
	}
	if item.CampaignID != "c-42" || item.Platform != "linkedin-ads" {
		t.Errorf("item does not carry its campaign/platform: %+v", item)
	}
	if !strings.Contains(item.Issue, "917") {
		t.Errorf("issue %q does not name the 917 clicks it is reasoning about", item.Issue)
	}
	if item.Action == "" {
		t.Error("item carries no remedy")
	}
}
