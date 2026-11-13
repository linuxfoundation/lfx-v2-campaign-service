// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"slices"
	"strings"
	"testing"
)

// The lists are the 2026-11-13 probe's results (#575): each value Google accepted on that
// ad type, and none it refused. A change to either is a change to what was probed.
func TestCallToActions_AreTheProbedSets(t *testing.T) {
	refusedOnBoth := []string{"Register", "Get started", "Watch now", "Buy tickets", "Join now", "Get tickets", "Enroll now", "Start now", "Play now", "Use app", "Open", "Get directions", "Automated", "LEARN_MORE"}
	cases := []struct {
		adType   string
		allowed  []string
		accepted []string
		refused  []string
	}{
		{"Display", displayCallToActions, []string{"Install", "Order now", "Try now", "Read more", "Explore"}, append([]string{"Donate now"}, refusedOnBoth...)},
		{"Demand Gen", demandGenCallToActions, []string{"Donate now"}, append([]string{"Install", "Order now", "Try now", "Read more", "Explore"}, refusedOnBoth...)},
	}
	for _, tc := range cases {
		for _, v := range append([]string{"Learn more", "Sign up", "Apply now", "Book now"}, tc.accepted...) {
			if got, err := resolveCallToActionText(tc.adType, tc.allowed, v); err != nil || got != v {
				t.Errorf("%s %q = %q, %v; want it accepted as probed", tc.adType, v, got, err)
			}
		}
		for _, v := range tc.refused {
			if _, err := resolveCallToActionText(tc.adType, tc.allowed, v); err == nil || !strings.Contains(err.Error(), "not one Google accepts") {
				t.Errorf("%s %q: err = %v, want it refused before any write", tc.adType, v, err)
			}
		}
	}
}

// Each list is pinned exactly, so dropping, misspelling or adding a value fails here; a
// change must come with a probe.
func TestCallToActions_ArePinnedExactly(t *testing.T) {
	wantDisplay := []string{"Learn more", "Sign up", "Apply now", "Book now", "Visit site", "Contact us", "Download",
		"Subscribe", "See more", "Shop now", "Get quote", "Get offer", "Call now", "Install",
		"Order now", "Try now", "Read more", "Explore"}
	wantDemandGen := []string{"Learn more", "Sign up", "Apply now", "Book now", "Visit site", "Contact us", "Download",
		"Subscribe", "See more", "Shop now", "Get quote", "Get offer", "Call now", "Donate now"}
	if !slices.Equal(displayCallToActions, wantDisplay) {
		t.Errorf("displayCallToActions = %q, want the probed %q", displayCallToActions, wantDisplay)
	}
	if !slices.Equal(demandGenCallToActions, wantDemandGen) {
		t.Errorf("demandGenCallToActions = %q, want the probed %q", demandGenCallToActions, wantDemandGen)
	}
}

// The caller's case and spacing are folded, and the PROBED spelling is what is sent:
// Google itself refused "learn more" on both ad types and "LEARN MORE" on Demand Gen.
func TestResolveCallToActionText_SendsTheProbedSpelling(t *testing.T) {
	for _, in := range []string{"learn more", "LEARN MORE", "  Learn   More "} {
		got, err := resolveCallToActionText("Demand Gen", demandGenCallToActions, in)
		if err != nil || got != "Learn more" {
			t.Errorf("%q = %q, %v; want \"Learn more\"", in, got, err)
		}
	}
	if got, err := resolveCallToActionText("Display", displayCallToActions, "   "); err != nil || got != "" {
		t.Errorf("blank = %q, %v; want empty and no error (Google picks the button)", got, err)
	}
}

// Both creative validators route through the allowlist, so a refused value fails the pure
// preflight rather than adGroupAds:mutate after the campaign exists.
func TestCreativeValidators_RefuseAnUnacceptedCallToAction(t *testing.T) {
	dg := fullCreative()
	dg.CallToActionText = "Register"
	if _, err := validateDemandGenCreative(campaignKindDemandGen, creativeInput(dg)); err == nil || !strings.Contains(err.Error(), "Demand Gen call to action") {
		t.Errorf("Demand Gen err = %v, want the call to action refused", err)
	}
	dg.CallToActionText = "sign UP"
	plan, err := validateDemandGenCreative(campaignKindDemandGen, creativeInput(dg))
	if err != nil || plan.callToActionText != "Sign up" {
		t.Errorf("Demand Gen plan = %q, %v; want the probed spelling \"Sign up\"", plan.callToActionText, err)
	}
	disp := fullDisplayCreative()
	disp.CallToActionText = "Donate now"
	if _, err := validateDisplay(t, disp); err == nil || !strings.Contains(err.Error(), "Display call to action") {
		t.Errorf("Display err = %v, want Donate now refused (accepted on Demand Gen only)", err)
	}
}
