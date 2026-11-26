// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
)

// TestCreateCampaign_TargetCost pins the automatic create with a target (#674): the unitCost is
// sent beside the unchanged automated optimizationTargetType and costType CPM — the shape of the
// KubeCon NA 2026 Campaign Manager campaigns 912330026 (MAX_CLICK, 17.88) and 873501596
// (MAX_CONVERSION, 10) — and recorded on the result; no target sends no unitCost, as before #674.
// The bid lever's predicate must still refuse each campaign: it is automatic.
func TestCreateCampaign_TargetCost(t *testing.T) {
	for _, tc := range []struct {
		name, objective, wantObjective, wantTarget, wantName string
		target                                               float64
		wantWire                                             string
	}{
		{"traffic with a target", "", ObjectiveWebsiteVisit, OptimizationTargetMaxClick, "Traffic", 17.88, "17.88"},
		{"explicit traffic with a target", ObjectiveWebsiteVisit, ObjectiveWebsiteVisit, OptimizationTargetMaxClick, "Traffic", 17.88, "17.88"},
		{"conversions with a target", ObjectiveWebsiteConversion, ObjectiveWebsiteConversion, OptimizationTargetMaxConversion, "Conversions", 10, "10.00"},
		{"traffic without a target is unchanged", "", ObjectiveWebsiteVisit, OptimizationTargetMaxClick, "Traffic", 0, ""},
		{"conversions without a target is unchanged", ObjectiveWebsiteConversion, ObjectiveWebsiteConversion, OptimizationTargetMaxConversion, "Conversions", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, body, _ := conversionObjectiveServer(t)
			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
			in := biddingCreateInput(0)
			in.Objective = tc.objective
			in.TargetCost = tc.target
			if tc.objective == ObjectiveWebsiteConversion {
				in.Conversions = []string{"104012"}
			}
			res, err := c.CreateCampaign(context.Background(), in)
			if err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			b := body()
			if b["objectiveType"] != tc.wantObjective || b["optimizationTargetType"] != tc.wantTarget || b["costType"] != "CPM" {
				t.Fatalf("campaign: objectiveType=%v optimizationTargetType=%v costType=%v, want %s/%s/CPM",
					b["objectiveType"], b["optimizationTargetType"], b["costType"], tc.wantObjective, tc.wantTarget)
			}
			unit, has := b["unitCost"].(map[string]any)
			if tc.wantWire == "" {
				if _, present := b["unitCost"]; present {
					t.Fatalf("an automatic campaign without a target must not send unitCost: %v", b["unitCost"])
				}
			} else if !has || unit["amount"] != tc.wantWire || unit["currencyCode"] != "USD" {
				t.Fatalf("unitCost = %v, want {amount %s, currencyCode USD}", b["unitCost"], tc.wantWire)
			}
			if !strings.Contains(res.CampaignName, "| NA | "+tc.wantName+" |") {
				t.Errorf("campaign name %q lacks the %s segment", res.CampaignName, tc.wantName)
			}
			if !res.BidApplied || res.TargetCost != tc.wantWire || res.OptimizationTarget != tc.wantTarget {
				t.Errorf("result BidApplied=%v TargetCost=%q OptimizationTarget=%q, want true/%q/%s", res.BidApplied, res.TargetCost, res.OptimizationTarget, tc.wantWire, tc.wantTarget)
			}
			costType, target := b["costType"].(string), b["optimizationTargetType"].(string)
			if (&CampaignSettings{CostType: &costType, OptimizationTargetType: &target}).ManualCPC() {
				t.Fatal("an automatic campaign with a target must not read as a manual max CPC")
			}
		})
	}
}

// TestCreateCampaign_TargetCostRefusalsSendNothing: a target with a manual bid, and a target
// ValidateTargetCost refuses, are refused before any POST — not even the campaign group.
func TestCreateCampaign_TargetCostRefusalsSendNothing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		bid     float64
		target  float64
		wantErr error
		want    string
	}{
		{"manual bid with a target", 4.5, 10, ErrTargetCostNotAutomatic, "applies to automatic bidding only"},
		{"sub-cent target", 0, 17.885, ErrBidAmountInvalid, "the target cost 17.885 is not a whole number of cents"},
		{"negative target", 0, -1, ErrBidAmountInvalid, "the target cost must be a finite number greater than zero"},
		{"target over the ceiling", 0, 1_000_000.01, ErrBidAmountInvalid, "exceeds the largest target cost"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, posts := biddingCreateServer(t)
			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
			in := biddingCreateInput(tc.bid)
			in.TargetCost = tc.target
			res, err := c.CreateCampaign(context.Background(), in)
			if !errors.Is(err, tc.wantErr) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %v containing %q", err, tc.wantErr, tc.want)
			}
			if res != nil {
				t.Fatalf("a pre-create refusal must return a nil result, got %+v", res)
			}
			if n := posts(); n != 0 {
				t.Fatalf("%d mutating requests were sent for a refused target, want 0", n)
			}
		})
	}
}

// TestValidateTargetCost: the bid's rules with the target cost named, and ValidateBidAmount's own
// sentences unchanged by the shared helper.
func TestValidateTargetCost(t *testing.T) {
	for _, tc := range []struct {
		amount float64
		wire   string
	}{{17.88, "17.88"}, {10, "10.00"}, {0.01, "0.01"}} {
		if got, err := ValidateTargetCost(tc.amount); err != nil || got != tc.wire {
			t.Errorf("ValidateTargetCost(%v) = %q, %v; want %q", tc.amount, got, err, tc.wire)
		}
	}
	for _, tc := range []struct {
		amount float64
		want   string
	}{
		{0, "the target cost must be a finite number greater than zero"},
		{math.NaN(), "the target cost must be a finite number greater than zero"},
		{0.004, "the target cost rounds to zero; the smallest target cost this service sets on LinkedIn is 0.01"},
		{2.345, "the target cost 2.345 is not a whole number of cents; LinkedIn target costs are set in the account currency to two decimal places"},
		{1_000_000.01, "the target cost 1000000.01 exceeds the largest target cost this service sets on LinkedIn (1000000)"},
	} {
		_, err := ValidateTargetCost(tc.amount)
		if reason, ok := BidAmountReason(err); !ok || reason != tc.want {
			t.Errorf("ValidateTargetCost(%v) reason = %q, want %q", tc.amount, reason, tc.want)
		}
	}
	for _, tc := range []struct {
		amount float64
		want   string
	}{
		{-1, "the bid must be a finite number greater than zero"},
		{0.004, "the bid rounds to zero; the smallest bid this service sets on LinkedIn is 0.01"},
		{2.345, "the bid 2.345 is not a whole number of cents; LinkedIn bids are set in the account currency to two decimal places"},
		{1_000_000.01, "the bid 1000000.01 exceeds the largest bid this service sets on LinkedIn (1000000)"},
	} {
		_, err := ValidateBidAmount(tc.amount)
		if reason, ok := BidAmountReason(err); !ok || reason != tc.want {
			t.Errorf("ValidateBidAmount(%v) reason = %q, want %q", tc.amount, reason, tc.want)
		}
	}
}

// TestWithTargetCost_DoesNotMutateThePlan: the target is added to a copy of the plan's fields, and
// a zero target returns the plan as campaignBidding built it.
func TestWithTargetCost_DoesNotMutateThePlan(t *testing.T) {
	plan, err := campaignBidding(0, "")
	if err != nil {
		t.Fatal(err)
	}
	same, err := withTargetCost(plan, 0)
	if err != nil || len(same.fields) != len(plan.fields) || same.targetCost != "" {
		t.Fatalf("withTargetCost(plan, 0) = %+v, %v; want the plan unchanged", same, err)
	}
	got, err := withTargetCost(plan, 17.88)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := plan.fields["unitCost"]; has {
		t.Fatal("withTargetCost wrote unitCost into the original plan's fields")
	}
	if got.optimizationTarget != OptimizationTargetMaxClick || got.fields["costType"] != "CPM" || got.targetCost != "17.88" {
		t.Fatalf("withTargetCost = %+v, want MAX_CLICK / CPM / 17.88", got)
	}
}
