// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"fmt"
	"strings"
)

// displayCallToActions and demandGenCallToActions are the callToActionText values this
// client sends on a responsive display ad and a Demand Gen image (multi-asset) ad (#575).
//
// The field is free text in the API, but Google accepts only a fixed set of phrases and
// refuses anything else at adGroupAds:mutate with INVALID_CALL_TO_ACTION_TEXT. That
// refusal arrives AFTER the budget, campaign, ad group and image assets exist, so an
// unchecked value left a half-built paused campaign (job e51fa823, 7 Nov 2026, "Register").
// Google does not publish the set, so each list is exactly what a validateOnly
// adGroupAds:mutate on 2026-11-13 (v23, account 8666746580) accepted, copying a live
// ad and varying only this field.
//
// The two lists DIFFER, and neither is a subset of the other: Display accepted Install,
// Order now, Try now, Read more and Explore, which Demand Gen refused; Demand Gen accepted
// Donate now, which Display refused. Both refused Register, Get started, Watch now, Buy
// tickets, Join now, Get tickets, Enroll now, Start now, Play now, Use app, Open, Get
// directions and Automated, and the enum spellings (LEARN_MORE).
//
// Google's own matching is not simply case-insensitive ("learn more" was refused on both,
// "LEARN MORE" accepted on Display and refused on Demand Gen), so this client matches the
// caller's text ignoring case and runs of spaces, and SENDS the probed spelling below,
// never the caller's. Widen a list value by value, each with a probe.
var displayCallToActions = []string{
	"Learn more", "Sign up", "Apply now", "Book now", "Visit site", "Contact us", "Download",
	"Subscribe", "See more", "Shop now", "Get quote", "Get offer", "Call now", "Install",
	"Order now", "Try now", "Read more", "Explore",
}

var demandGenCallToActions = []string{
	"Learn more", "Sign up", "Apply now", "Book now", "Visit site", "Contact us", "Download",
	"Subscribe", "See more", "Shop now", "Get quote", "Get offer", "Call now", "Donate now",
}

// resolveCallToActionText returns the probed spelling from allowed that raw names, or ""
// when raw is blank (Google then supplies its own button). Anything else is refused, in
// the pure preflight, before any write. adType names the ad in the error.
func resolveCallToActionText(adType string, allowed []string, raw string) (string, error) {
	key := callToActionKey(raw)
	if key == "" {
		return "", nil
	}
	for _, v := range allowed {
		if callToActionKey(v) == key {
			return v, nil
		}
	}
	return "", fmt.Errorf("google-ads %s call to action %q is not one Google accepts (%s); omit it to let Google choose", adType, capForError(strings.TrimSpace(raw)), strings.Join(allowed, ", "))
}

// callToActionKey folds case and collapses whitespace, so "learn  MORE " and "Learn more"
// name the same value.
func callToActionKey(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}
