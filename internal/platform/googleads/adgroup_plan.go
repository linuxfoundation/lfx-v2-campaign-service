// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

// Multiple ad groups, multiple responsive search ads (LFXV2-2665).
//
// Until this file, a Search campaign created here had exactly ONE ad group with
// exactly ONE ad, and every keyword the brief produced went into that one group.
// That is the structure Google scores worst: Ad Rank is computed per keyword
// against the ad that would serve for it, so a single group holding "kubernetes
// training" and "cloud native conference" serves one piece of copy to both and
// is judged mediocre for each. The fix is not better copy — it is more groups,
// one per theme, each with copy written for its own query cluster.
//
// The second half is more than one ad per group. Google rotates the responsive
// search ads in a group and learns which combination converts; a group with one
// ad gives it nothing to learn from, and the Google Ads UI flags it as
// "below average ad strength" for exactly that reason.
//
// Both are OPT-IN and additive: a caller that sets no AdGroups gets precisely
// the single-group, single-ad campaign this client built before, from the same
// campaign-level Headlines/Descriptions/Keywords/CPCBid fields. Nothing about
// the existing shape moves.
//
// SEARCH ONLY. Demand Gen builds its own ad group and deliberately creates no
// ad at all (see demandgen.go), so an AdGroups list on that path is refused at
// preflight rather than silently dropped.

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// maxAdGroupsPerCampaign is a SERVICE guard, not Google's limit — Google
	// allows thousands per campaign. A brief that asks for more than this many
	// themes has almost certainly been generated wrong, and each group costs a
	// mutate against a live account. Lift it when a real campaign needs more.
	maxAdGroupsPerCampaign = 20
	// maxAdsPerAdGroup is GOOGLE's limit: three enabled responsive search ads
	// per ad group. A fourth is rejected at the mutate, which on this cascade
	// happens after the campaign is already paid for, so it is refused here.
	maxAdsPerAdGroup = 3
)

// AdSpec is one responsive search ad: its own headlines and descriptions.
//
// Both are optional in the same way the campaign-level Headlines/Descriptions
// are — composeAdCopy pads missing slots with deterministic placeholders up to
// Google's minimum — so an AdSpec may be zero-valued, which produces the same
// generated copy the single-ad path produces today.
type AdSpec struct {
	Headlines    []string
	Descriptions []string
}

// AdGroupSpec is one ad group: a theme, the keywords that express it, and the
// ads written for it.
//
// Every field except Name falls back to the campaign-level equivalent when it is
// empty, so a caller that only wants to split keywords across themes writes just
// Name and Keywords and inherits the rest. That fallback is per field and not
// all-or-nothing: a group may override only its bid.
type AdGroupSpec struct {
	// Name is the theme label, appended to the campaign's composed ad group name
	// so the result is still the pipe-delimited form the rest of this client
	// reconciles by: "LFX | Ad Group | <project> | <event> | <suffix> | <Name>".
	// Required and must be distinct within the campaign — Google rejects a
	// duplicate ad group name, and discovering that mid-cascade strands the
	// groups already created.
	Name string
	// CPCBid overrides the campaign-level CPCBid for this group, in whole units
	// of the ad ACCOUNT's currency. 0 means "inherit", which is what a caller
	// that does not care about per-theme bidding wants; it does NOT mean "no
	// bid" — a campaign-level 0 is what means that, and it is inherited as such.
	CPCBid float64
	// Keywords and AudienceSegments replace the campaign-level lists for this
	// group when non-empty. Splitting keywords per theme is the whole point of
	// having more than one group; audiences more often stay shared, so leaving
	// this nil and inheriting is the common case.
	Keywords         []Keyword
	AudienceSegments []string
	// Ads are the responsive search ads for this group, up to maxAdsPerAdGroup.
	// Empty means one ad built from the campaign-level copy, which is what the
	// single-group path does.
	Ads []AdSpec
}

// adPlan is one validated ad: the copy composeAdCopy produced, padded to
// Google's minimums and ready to send.
type adPlan struct {
	headlines    []string
	descriptions []string
}

// adGroupPlan is one validated ad group and everything that hangs off it. Every
// value here is already checked and derived — by the time the cascade reads one,
// the campaign exists and there is nothing left to reject.
type adGroupPlan struct {
	name             string
	cpcBidMicros     int64
	keywords         []Keyword
	audienceSegments []string
	ads              []adPlan
}

// validateAdGroupPlans turns the caller's AdGroups into the list the cascade
// creates, or returns the single default group when none were asked for.
//
// base is the group precomputeAdGroupAdInputs already derived from the
// campaign-level fields. It is both the no-AdGroups answer and the source of
// every per-group fallback, which is what keeps "inherit" meaning exactly what
// the single-group path would have done rather than a second set of defaults.
//
// Like every other preflight validator here, this one mutates nothing and sends
// nothing: it runs BEFORE the budget mutate so a duplicate group name or a
// fourth ad in a group cannot strand a paid campaign.
func validateAdGroupPlans(kind string, in CampaignInput, base adGroupPlan) ([]adGroupPlan, error) {
	if len(in.AdGroups) == 0 {
		return []adGroupPlan{base}, nil
	}
	if kind != campaignKindSearch {
		return nil, fmt.Errorf("google-ads campaign creation aborted before any request (%d ad groups requested, but multiple ad groups are a SEARCH capability; the %s channel creates its own single ad group and no ad)", len(in.AdGroups), kind)
	}
	if len(in.AdGroups) > maxAdGroupsPerCampaign {
		return nil, fmt.Errorf("google-ads campaign creation aborted before any request (%d ad groups requested, limit %d)", len(in.AdGroups), maxAdGroupsPerCampaign)
	}

	plans := make([]adGroupPlan, 0, len(in.AdGroups))
	seen := make(map[string]struct{}, len(in.AdGroups))
	for i, spec := range in.AdGroups {
		label := sanitizeNamePart(spec.Name)
		if label == "" {
			return nil, fmt.Errorf("google-ads campaign creation aborted before any request (ad group %d has no name; each ad group needs a distinct theme label)", i+1)
		}
		name := base.name + " | " + label
		if err := validateEntityName("ad group", name, utf8.RuneCountInString(name), maxAdGroupNameRunes, "characters"); err != nil {
			return nil, err
		}
		// Case-insensitive, because Google's own duplicate check is: two groups
		// differing only in case collide at the mutate, by which point the
		// earlier groups in this list already exist.
		key := strings.ToLower(name)
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("google-ads campaign creation aborted before any request (ad group name %q is used more than once; Google rejects duplicate ad group names)", name)
		}
		seen[key] = struct{}{}

		plan := adGroupPlan{
			name:             name,
			cpcBidMicros:     base.cpcBidMicros,
			keywords:         base.keywords,
			audienceSegments: base.audienceSegments,
			ads:              base.ads,
		}
		if spec.CPCBid != 0 {
			micros, err := validateCPCBid(spec.CPCBid)
			if err != nil {
				return nil, fmt.Errorf("ad group %q: %w", name, err)
			}
			plan.cpcBidMicros = micros
		}
		if len(spec.Keywords) > 0 {
			keywords, err := validateKeywords(spec.Keywords)
			if err != nil {
				return nil, fmt.Errorf("google-ads campaign creation aborted before any request (ad group %q has invalid keyword input): %w", name, err)
			}
			plan.keywords = keywords
		}
		if len(spec.AudienceSegments) > 0 {
			segments, err := validateAudienceSegments(spec.AudienceSegments)
			if err != nil {
				return nil, fmt.Errorf("google-ads campaign creation aborted before any request (ad group %q has invalid audience segment input): %w", name, err)
			}
			plan.audienceSegments = segments
		}
		if len(spec.Ads) > 0 {
			ads, err := validateAdSpecs(name, spec.Ads, in)
			if err != nil {
				return nil, err
			}
			plan.ads = ads
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// validateAdSpecs composes and validates one group's ads.
//
// Duplicate copy across two ads in a group is NOT refused. Google accepts it,
// and a guard here that upstream would not apply is the expensive kind of wrong:
// it turns a campaign Google would have created into a failure. It is wasteful,
// not invalid.
func validateAdSpecs(adGroupName string, specs []AdSpec, in CampaignInput) ([]adPlan, error) {
	if len(specs) > maxAdsPerAdGroup {
		return nil, fmt.Errorf("google-ads campaign creation aborted before any request (ad group %q has %d ads, limit %d responsive search ads per ad group)", adGroupName, len(specs), maxAdsPerAdGroup)
	}
	ads := make([]adPlan, 0, len(specs))
	for i, spec := range specs {
		headlines, descriptions, err := composeAdCopy(spec.Headlines, spec.Descriptions, in.EventName, in.Project)
		if err != nil {
			return nil, fmt.Errorf("google-ads campaign creation aborted before any request (ad group %q ad %d has invalid ad copy): %w", adGroupName, i+1, err)
		}
		ads = append(ads, adPlan{headlines: headlines, descriptions: descriptions})
	}
	return ads, nil
}

// adGroupPlanStep renders the one-line summary the result's Steps carry for a
// multi-group campaign. It names the counts an operator would otherwise have to
// open the account to see.
func adGroupPlanStep(plans []adGroupPlan) string {
	ads, keywords := 0, 0
	for _, p := range plans {
		ads += len(p.ads)
		keywords += len(p.keywords)
	}
	return fmt.Sprintf("%d ad groups, %d ads, %d keywords", len(plans), ads, keywords)
}
