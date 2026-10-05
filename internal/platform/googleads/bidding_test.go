// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// biddingTestCustomer is the account every test here preflights against. It must match the
// customer id a bare conversion-action id is qualified with.
const biddingTestCustomer = "1234567890"

func biddingClient() *Client {
	return &Client{account: AccountConfig{CustomerID: biddingTestCustomer}}
}

// strategyKeys marshals a biddingFields and reports which strategy keys the payload carries.
// Asserting on the MARSHALLED object rather than on the struct is the point: the oneof lives
// in the JSON Google receives, and a pointer field whose tag lost its omitempty would still
// look like a correct struct while sending `"targetSpend":null` beside the real strategy.
func strategyKeys(t *testing.T, f biddingFields) []string {
	t.Helper()
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal bidding fields: %v", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode bidding fields: %v", err)
	}
	var keys []string
	for _, k := range []string{"manualCpc", "targetSpend", "maximizeConversions", "maximizeConversionValue"} {
		if _, ok := obj[k]; ok {
			keys = append(keys, k)
		}
	}
	return keys
}

// A campaign carries EXACTLY ONE bidding strategy. Google rejects a payload naming two, and
// the rejection lands after the budget mutate — so this is the invariant that costs money
// when it breaks, and it is checked over every strategy rather than over the one a happy-path
// test happens to use.
func TestBiddingPlan_SendsExactlyOneStrategy(t *testing.T) {
	cases := []struct {
		strategy string
		want     string
		in       CampaignInput
	}{
		{biddingManualCPC, "manualCpc", CampaignInput{}},
		{biddingMaximizeClicks, "targetSpend", CampaignInput{}},
		{biddingMaximizeConversions, "maximizeConversions", CampaignInput{}},
		{biddingTargetCPA, "maximizeConversions", CampaignInput{TargetCPA: 25}},
		{biddingMaximizeConversionValue, "maximizeConversionValue", CampaignInput{}},
		{biddingTargetROAS, "maximizeConversionValue", CampaignInput{TargetROAS: 4}},
	}
	for _, tc := range cases {
		t.Run(tc.strategy, func(t *testing.T) {
			in := tc.in
			in.BiddingStrategy = tc.strategy
			plan, err := validateBiddingPlan(campaignKindSearch, biddingTestCustomer, in)
			if err != nil {
				t.Fatalf("validateBiddingPlan: %v", err)
			}
			got := strategyKeys(t, plan.fields())
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("payload carries %v, want exactly [%s]", got, tc.want)
			}
		})
	}
}

// The DEFAULT payload must be byte-identical to what each channel sent before the strategy
// was selectable. Every Google campaign this service has created was made with one of these
// two, and a changed default would silently re-bid them all — a regression no error message
// would ever announce.
func TestBiddingPlan_DefaultsAreTheLegacyPayloads(t *testing.T) {
	cases := []struct {
		kind string
		want string
	}{
		{campaignKindSearch, `{"manualCpc":{}}`},
		{campaignKindDemandGen, `{"targetSpend":{}}`},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			plan, err := validateBiddingPlan(tc.kind, biddingTestCustomer, CampaignInput{})
			if err != nil {
				t.Fatalf("validateBiddingPlan: %v", err)
			}
			raw, err := json.Marshal(plan.fields())
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(raw) != tc.want {
				t.Errorf("default payload = %s, want %s", raw, tc.want)
			}
		})
	}
}

// The default reaches the real create payload, not just the plan. A preflight that resolved
// the right strategy into a campaignCreate that still hard-coded manualCpc would pass every
// test above.
func TestCampaignCreatePayload_CarriesTheResolvedStrategy(t *testing.T) {
	c := biddingClient()
	in := sampleInput()
	in.BiddingStrategy = biddingTargetCPA
	in.TargetCPA = 12.5
	pf, err := c.preflightCampaignKind(campaignKindSearch, in)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	raw, err := json.Marshal(campaignCreate{biddingFields: pf.bidding.fields()})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := obj["manualCpc"]; ok {
		t.Error("the create payload still carries manualCpc under a target-CPA plan")
	}
	got, ok := obj["maximizeConversions"]
	if !ok {
		t.Fatalf("the create payload carries no maximizeConversions: %s", raw)
	}
	// 12.5 in the account currency is 12_500_000 micros. Asserted exactly because a
	// truncating conversion would bill one micro less on every value float64 cannot hold
	// precisely, which is the bug validateCPCBid's math.Round comment describes.
	if string(got) != `{"targetCpaMicros":12500000}` {
		t.Errorf("maximizeConversions = %s, want {\"targetCpaMicros\":12500000}", got)
	}
}

// An omitted target must be OMITTED, not sent as zero. The two say different things to
// Google — "bid for as many conversions as the budget allows" versus a target of nothing —
// and only omitempty keeps them apart.
func TestBiddingPlan_OmittedTargetsAreAbsentNotZero(t *testing.T) {
	for _, strategy := range []string{biddingMaximizeConversions, biddingMaximizeConversionValue} {
		t.Run(strategy, func(t *testing.T) {
			plan, err := validateBiddingPlan(campaignKindSearch, biddingTestCustomer, CampaignInput{BiddingStrategy: strategy})
			if err != nil {
				t.Fatalf("validateBiddingPlan: %v", err)
			}
			raw, err := json.Marshal(plan.fields())
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(raw), "0") {
				t.Errorf("payload %s carries a zero target; an omitted target must not be sent", raw)
			}
		})
	}
}

// The target-bearing spellings require their target, and the maximize- ones do not. Both
// halves matter: requiring it on `maximize-conversions` would refuse a create Google accepts,
// and accepting its absence on `target-cpa` would create a campaign named after a number the
// caller never gave.
func TestBiddingPlan_TargetRequiredOnlyWhereTheLabelIsTheTarget(t *testing.T) {
	cases := []struct {
		name    string
		in      CampaignInput
		wantErr string
	}{
		{"target-cpa with no CPA", CampaignInput{BiddingStrategy: biddingTargetCPA}, "requires a target CPA"},
		{"target-roas with no ROAS", CampaignInput{BiddingStrategy: biddingTargetROAS}, "requires a target ROAS"},
		{"maximize-conversions with no CPA", CampaignInput{BiddingStrategy: biddingMaximizeConversions}, ""},
		{"maximize-conversion-value with no ROAS", CampaignInput{BiddingStrategy: biddingMaximizeConversionValue}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateBiddingPlan(campaignKindSearch, biddingTestCustomer, tc.in)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected refusal: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("accepted, want a refusal mentioning %q", tc.wantErr)
			case tc.wantErr != "" && err != nil && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("error %v does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// A target the chosen strategy cannot bid to is REFUSED, not dropped. Dropping it is the
// LFXV2-3283 defect in its worst location: the operator reads "campaign created" and believes
// Google is holding their cost per conversion to a number it has never seen.
func TestBiddingPlan_RefusesATargetTheStrategyCannotCarry(t *testing.T) {
	cases := []struct {
		name string
		in   CampaignInput
	}{
		{"CPA under manual-cpc", CampaignInput{BiddingStrategy: biddingManualCPC, TargetCPA: 20}},
		{"CPA under maximize-clicks", CampaignInput{BiddingStrategy: biddingMaximizeClicks, TargetCPA: 20}},
		{"CPA under a ROAS strategy", CampaignInput{BiddingStrategy: biddingTargetROAS, TargetROAS: 4, TargetCPA: 20}},
		{"ROAS under manual-cpc", CampaignInput{BiddingStrategy: biddingManualCPC, TargetROAS: 4}},
		{"ROAS under a CPA strategy", CampaignInput{BiddingStrategy: biddingTargetCPA, TargetCPA: 20, TargetROAS: 4}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := validateBiddingPlan(campaignKindSearch, biddingTestCustomer, tc.in); err == nil {
				t.Error("accepted a target the strategy cannot bid to; it would be silently discarded")
			}
		})
	}
}

// A CPC bid under an automated strategy is refused — including a bid that appears ONLY on a
// per-group override. Checking the campaign-level field alone would accept exactly the
// multi-ad-group create whose bids are the ones Google keeps and never bids.
func TestBiddingPlan_RefusesACPCBidUnderAnAutomatedStrategy(t *testing.T) {
	campaignLevel := CampaignInput{BiddingStrategy: biddingMaximizeConversions, CPCBid: 2.5}
	if _, err := validateBiddingPlan(campaignKindSearch, biddingTestCustomer, campaignLevel); err == nil {
		t.Error("accepted a campaign-level CPC bid under maximize-conversions")
	}
	groupLevel := CampaignInput{
		BiddingStrategy: biddingMaximizeConversions,
		AdGroups:        []AdGroupSpec{{Name: "a"}, {Name: "b", CPCBid: 2.5}},
	}
	if _, err := validateBiddingPlan(campaignKindSearch, biddingTestCustomer, groupLevel); err == nil {
		t.Error("accepted a per-ad-group CPC bid under maximize-conversions")
	}
	// The same bid under the manual strategy is the whole point of the field and must
	// still be accepted — this is the over-refusal the guard above must not commit.
	manual := CampaignInput{BiddingStrategy: biddingManualCPC, CPCBid: 2.5}
	if _, err := validateBiddingPlan(campaignKindSearch, biddingTestCustomer, manual); err != nil {
		t.Errorf("refused a CPC bid under manual CPC, which is what it is for: %v", err)
	}
}

// Demand Gen accepts exactly one strategy, on recorded live-API evidence. A strategy that is
// VALID but wrong for the channel gets a different error from an unknown name, because the
// two need different fixes.
func TestBiddingPlan_DemandGenAcceptsOnlyMaximizeClicks(t *testing.T) {
	for strategy := range searchBiddingStrategies {
		in := CampaignInput{BiddingStrategy: strategy}
		switch strategy {
		case biddingTargetCPA:
			in.TargetCPA = 20
		case biddingTargetROAS:
			in.TargetROAS = 4
		}
		_, err := validateBiddingPlan(campaignKindDemandGen, biddingTestCustomer, in)
		if strategy == biddingMaximizeClicks {
			if err != nil {
				t.Errorf("%s refused on Demand Gen, but it is the channel default: %v", strategy, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s accepted on Demand Gen; the live API rejected it AFTER the budget mutate", strategy)
			continue
		}
		if !strings.Contains(err.Error(), "not supported on") {
			t.Errorf("%s: error %v reads as a typo rather than a channel mismatch", strategy, err)
		}
	}
}

// An unknown name lists what IS supported, sorted. Sorted because map iteration order is
// randomized, and a supported-values list that reshuffles between two identical calls reads
// as two different errors to anyone diffing logs.
func TestBiddingPlan_UnknownStrategyListsTheSupportedSetInOrder(t *testing.T) {
	_, err := validateBiddingPlan(campaignKindSearch, biddingTestCustomer, CampaignInput{BiddingStrategy: "maximise-conversions"})
	if err == nil {
		t.Fatal("accepted an unknown bidding strategy")
	}
	want := strings.Join([]string{
		biddingManualCPC, biddingMaximizeClicks, biddingMaximizeConversionValue,
		biddingMaximizeConversions, biddingTargetCPA, biddingTargetROAS,
	}, ", ")
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %v does not list the supported set as %q", err, want)
	}
}

// Case and surrounding whitespace are the caller's, not the contract's: a strategy arrives
// from a form or a JSON config and " Maximize-Conversions " is the same request.
func TestBiddingPlan_NormalizesCaseAndWhitespace(t *testing.T) {
	plan, err := validateBiddingPlan(campaignKindSearch, biddingTestCustomer, CampaignInput{BiddingStrategy: "  Maximize-Conversions "})
	if err != nil {
		t.Fatalf("validateBiddingPlan: %v", err)
	}
	if plan.strategy != biddingMaximizeConversions {
		t.Errorf("strategy = %q, want %q", plan.strategy, biddingMaximizeConversions)
	}
}

// NaN and Inf are rejected before any ordered comparison. Every comparison against NaN is
// false, so a NaN target slips past BOTH bounds and then rounds to a garbage int64 — the same
// trap validateCPCBid documents.
func TestBiddingPlan_RejectsNonFiniteTargets(t *testing.T) {
	inf := math.Inf(1)
	nan := math.NaN()
	for _, v := range []float64{nan, inf, -inf} {
		if _, err := validateTargetCPA(v); err == nil {
			t.Errorf("validateTargetCPA(%v) accepted a non-finite target", v)
		}
		if _, err := validateTargetROAS(v); err == nil {
			t.Errorf("validateTargetROAS(%v) accepted a non-finite target", v)
		}
	}
}

// The bounds are GOOGLE's documented ones, reproduced rather than tightened. 400 is a
// plausible mis-spelling of 400% but it is also a ratio Google accepts, so refusing it would
// refuse a create upstream would have taken — the over-refusal these guards must never
// commit. Only a value above the documented ceiling can be refused on evidence.
func TestValidateTargetROAS_Bounds(t *testing.T) {
	if _, err := validateTargetROAS(400); err != nil {
		t.Errorf("refused 400, a ratio inside Google's documented range: %v", err)
	}
	if _, err := validateTargetROAS(maxTargetROAS + 1); err == nil {
		t.Error("accepted a ratio above Google's documented ceiling")
	}
	if _, err := validateTargetROAS(minTargetROAS / 2); err == nil {
		t.Error("accepted a ratio below Google's documented floor")
	}
	if got, err := validateTargetROAS(4); err != nil || got != 4 {
		t.Errorf("validateTargetROAS(4) = (%v, %v), want (4, nil) — it is a ratio and must pass through unscaled", got, err)
	}
	if got, err := validateTargetROAS(0); err != nil || got != 0 {
		t.Errorf("validateTargetROAS(0) = (%v, %v), want (0, nil): 0 is UNSET, not an error", got, err)
	}
}

// Both spellings of a conversion action resolve to the same resource name, so a caller can
// paste an id off the Google Ads UI or feed back what ListConversionActions returned.
func TestValidateConversionActions_AcceptsBothSpellings(t *testing.T) {
	got, err := validateConversionActions(campaignKindSearch, biddingTestCustomer, []string{
		"  987654321 ",
		"customers/" + biddingTestCustomer + "/conversionActions/111222333",
	})
	if err != nil {
		t.Fatalf("validateConversionActions: %v", err)
	}
	want := []string{
		"customers/" + biddingTestCustomer + "/conversionActions/987654321",
		"customers/" + biddingTestCustomer + "/conversionActions/111222333",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("action %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A resource name owned by ANOTHER customer is refused, not rewritten. Rewriting it would
// point the campaign at an id that does not exist on this account, and Google would say so
// only after the budget mutate.
func TestValidateConversionActions_RefusesAForeignAccountsAction(t *testing.T) {
	_, err := validateConversionActions(campaignKindSearch, biddingTestCustomer, []string{"customers/9999999999/conversionActions/1"})
	if err == nil {
		t.Fatal("accepted a conversion action belonging to another customer")
	}
	if !strings.Contains(err.Error(), "9999999999") {
		t.Errorf("error %v does not name the foreign customer", err)
	}
}

// Duplicates are DEDUPLICATED, not refused: naming the same conversion twice is a harmless
// paste, while Google rejects a selective_optimization list containing a duplicate — after
// the budget mutate.
func TestValidateConversionActions_DeduplicatesAcrossSpellings(t *testing.T) {
	got, err := validateConversionActions(campaignKindSearch, biddingTestCustomer, []string{
		"555",
		"customers/" + biddingTestCustomer + "/conversionActions/555",
		"555",
	})
	if err != nil {
		t.Fatalf("validateConversionActions: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %v, want one entry: the same action named three ways is one action", got)
	}
}

func TestValidateConversionActions_RefusesMalformedAndEmpty(t *testing.T) {
	cases := [][]string{
		{""},
		{"   "},
		{"not-an-id"},
		{"customers/123/conversionActions/"},
		{"customers/" + biddingTestCustomer + "/conversionActions/abc"},
		{"customers/" + biddingTestCustomer + "/campaigns/555"},
	}
	for _, tc := range cases {
		if _, err := validateConversionActions(campaignKindSearch, biddingTestCustomer, tc); err == nil {
			t.Errorf("accepted %q, which cannot address a conversion action", tc)
		}
	}
	over := make([]string, maxConversionActions+1)
	for i := range over {
		over[i] = "1000" + strings.Repeat("0", i%3) + string(rune('0'+i%10))
	}
	if _, err := validateConversionActions(campaignKindSearch, biddingTestCustomer, over); err == nil {
		t.Errorf("accepted %d conversion actions, over the %d cap", len(over), maxConversionActions)
	}
}

// Refused on Demand Gen rather than dropped. The channel does not take
// campaign.selective_optimization, so accepting the list would validate every name and then
// discard the lot — leaving the operator believing the campaign bids toward the conversions
// they chose. Same rule every other Search-only input in this preflight follows.
func TestValidateConversionActions_RefusedOnDemandGen(t *testing.T) {
	_, err := validateConversionActions(campaignKindDemandGen, biddingTestCustomer, []string{"555"})
	if err == nil {
		t.Fatal("accepted conversion actions on Demand Gen, where they would be silently dropped")
	}
	// An EMPTY list must still be fine there: refusing it would refuse every Demand Gen
	// create that predates this field.
	if _, err := validateConversionActions(campaignKindDemandGen, biddingTestCustomer, nil); err != nil {
		t.Errorf("refused an absent conversion-action list on Demand Gen: %v", err)
	}
}

// selective_optimization is OMITTED when no actions were named, not sent empty. An empty list
// means "optimize toward nothing" and would stop the campaign bidding at all; an absent field
// means "inherit the account's conversion goals", which is what every campaign created before
// this field existed does.
func TestBiddingPlan_NoConversionActionsOmitsSelectiveOptimization(t *testing.T) {
	plan, err := validateBiddingPlan(campaignKindSearch, biddingTestCustomer, CampaignInput{})
	if err != nil {
		t.Fatalf("validateBiddingPlan: %v", err)
	}
	raw, err := json.Marshal(plan.fields())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "selectiveOptimization") {
		t.Errorf("payload %s carries selectiveOptimization with no actions named", raw)
	}
}

// The whole preflight refuses before anything is paid for — the same guarantee
// ValidateCampaignInputKind relies on, so adoption refuses exactly what create refuses.
func TestPreflight_RefusesABadBiddingPlanBeforeAnyMutate(t *testing.T) {
	c := biddingClient()
	in := sampleInput()
	in.BiddingStrategy = biddingTargetCPA // no TargetCPA supplied
	if _, err := c.preflightCampaignKind(campaignKindSearch, in); err == nil {
		t.Fatal("the preflight accepted target-cpa with no target")
	}
	if err := c.ValidateCampaignInputKind(campaignKindSearch, in); err == nil {
		t.Error("the adoption guard accepted what create refuses")
	}
}
