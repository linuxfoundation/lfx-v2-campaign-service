// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Targeting exclusions at create time (#549): the ad group's `excluded_communities` and
// `excluded_geolocations`, fields of the Reddit Ads API v3 targeting object
// (components_schema_targeting in https://ads-api.reddit.com/api/v3/openapi.json). Built from the
// published schema; no live create has sent either yet, which is why the dispatcher keeps them
// behind REDDIT_TARGETING_EXCLUSIONS_ENABLED.

const (
	// MaxExcludedSubreddits caps CampaignInput.ExcludedSubreddits. Reddit documents no maximum for
	// excluded_communities; the cap bounds the request this service builds.
	MaxExcludedSubreddits = 100
	// MaxExcludedGeoTargets caps CampaignInput.ExcludedGeoTargets.
	MaxExcludedGeoTargets = 50

	// FieldExcludedSubreddits and FieldExcludedGeoTargets are the redditConfig keys an
	// ExclusionError names.
	FieldExcludedSubreddits = "excludedSubreddits"
	FieldExcludedGeoTargets = "excludedGeoTargets"
)

// subredditNameRE is the shape of a subreddit name: 2 to 21 letters, digits or underscores.
var subredditNameRE = regexp.MustCompile(`^[A-Za-z0-9_]{2,21}$`)

// ExclusionError is a refused exclusion input. Field is the redditConfig key it belongs to and Msg
// the service-authored reason; any caller value in Msg went through domain.CallerValue.
type ExclusionError struct {
	Field string
	Msg   string
}

func (e *ExclusionError) Error() string { return e.Field + ": " + e.Msg }

// TargetingExclusions is the normalized form of the two exclusion lists, ready for the ad
// group's targeting object. A nil list means "send nothing".
type TargetingExclusions struct {
	Communities  []string
	Geolocations []string
}

// NormalizeTargetingExclusions validates and normalizes the exclusion lists against the targeting
// they would be sent with. It is the ONE rule set: CreateCampaign applies it before any upstream
// call, and the dispatcher applies it in PreflightCreate so a bad list is a synchronous 400.
//
//   - excludedSubreddits: an optional "r/" prefix is stripped; a blank entry, or a name that is
//     not 2–21 letters, digits or underscores, is refused; case-insensitive duplicates are
//     dropped (first spelling kept); a name also in subreddits is refused, because targeting and
//     excluding the same community is a contradiction Reddit would resolve silently.
//   - excludedGeoTargets: trimmed and uppercased; a blank entry or anything that is not an
//     assigned ISO 3166-1 alpha-2 code is refused; duplicates are dropped; a code also in the
//     EFFECTIVE geoTargets (the "US" default when none is given) is refused.
//
// Each list is capped (MaxExcludedSubreddits, MaxExcludedGeoTargets) on its raw length.
func NormalizeTargetingExclusions(subreddits, geoTargets, excludedSubreddits, excludedGeoTargets []string) (TargetingExclusions, error) {
	var out TargetingExclusions

	if n := len(excludedSubreddits); n > MaxExcludedSubreddits {
		return out, &ExclusionError{Field: FieldExcludedSubreddits, Msg: fmt.Sprintf("at most %d subreddits, got %d", MaxExcludedSubreddits, n)}
	}
	targeted := make(map[string]struct{}, len(subreddits))
	for _, s := range subreddits {
		name := strings.TrimSpace(stripSubredditPrefix(strings.TrimSpace(s)))
		if name != "" {
			targeted[strings.ToLower(name)] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(excludedSubreddits))
	for i, s := range excludedSubreddits {
		name := strings.TrimSpace(stripSubredditPrefix(strings.TrimSpace(s)))
		if name == "" {
			return out, &ExclusionError{Field: FieldExcludedSubreddits, Msg: fmt.Sprintf("entry %d is blank", i+1)}
		}
		if !subredditNameRE.MatchString(name) {
			return out, &ExclusionError{Field: FieldExcludedSubreddits, Msg: fmt.Sprintf("%s is not a subreddit name (2-21 letters, digits or underscores, optionally prefixed r/)", domain.CallerValue(name))}
		}
		key := strings.ToLower(name)
		if _, dup := seen[key]; dup {
			continue
		}
		if _, both := targeted[key]; both {
			return out, &ExclusionError{Field: FieldExcludedSubreddits, Msg: fmt.Sprintf("%s is also in subreddits; a community cannot be both targeted and excluded", domain.CallerValue(name))}
		}
		seen[key] = struct{}{}
		out.Communities = append(out.Communities, name)
	}

	if n := len(excludedGeoTargets); n > MaxExcludedGeoTargets {
		return out, &ExclusionError{Field: FieldExcludedGeoTargets, Msg: fmt.Sprintf("at most %d country codes, got %d", MaxExcludedGeoTargets, n)}
	}
	if len(excludedGeoTargets) == 0 {
		return out, nil
	}
	included := effectiveGeoSet(geoTargets)
	seenGeo := make(map[string]struct{}, len(excludedGeoTargets))
	for i, g := range excludedGeoTargets {
		code := strings.ToUpper(strings.TrimSpace(g))
		if code == "" {
			return out, &ExclusionError{Field: FieldExcludedGeoTargets, Msg: fmt.Sprintf("entry %d is blank", i+1)}
		}
		if !geoCodeRE.MatchString(code) || !iso3166Alpha2[code] {
			return out, &ExclusionError{Field: FieldExcludedGeoTargets, Msg: fmt.Sprintf("%s is not an ISO 3166-1 alpha-2 country code", domain.CallerValue(code))}
		}
		if _, dup := seenGeo[code]; dup {
			continue
		}
		if _, both := included[code]; both {
			return out, &ExclusionError{Field: FieldExcludedGeoTargets, Msg: fmt.Sprintf("%s is also targeted (geoTargets, or the US default when geoTargets is empty); a country cannot be both targeted and excluded", code)}
		}
		seenGeo[code] = struct{}{}
		out.Geolocations = append(out.Geolocations, code)
	}
	return out, nil
}

// effectiveGeoSet is the set of country codes CreateCampaign would target: the trimmed,
// uppercased non-blank geoTargets, or "US" when there are none. An invalid code is kept as
// spelled; CreateCampaign refuses it on its own.
func effectiveGeoSet(geoTargets []string) map[string]struct{} {
	set := make(map[string]struct{}, len(geoTargets))
	for _, g := range geoTargets {
		if code := strings.ToUpper(strings.TrimSpace(g)); code != "" {
			set[code] = struct{}{}
		}
	}
	if len(set) == 0 {
		set["US"] = struct{}{}
	}
	return set
}
