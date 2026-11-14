// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Per-campaign targeting (#552). Targeting is the caller's own targetingCriteria, in LinkedIn's
// shape: every Include entry is one clause of include.and whose facets are ORed, and Exclude is
// exclude.or. The facet keys and the value URN shapes are LinkedIn's (Targeting Criteria Facet
// URNs, li-lms-2026-10:
// https://learn.microsoft.com/en-us/linkedin/shared/references/v2/ads/targeting-criteria-facet-urns).
//
// When a CampaignInput carries Targeting it REPLACES the profile clause (TargetingProfile and the
// runtime config's TargetingProfiles are not read) and the hard-coded jobFunctions and
// seniorityExclusions. The runtime config's EmployerExclusions are still merged into
// exclude.employers, so the staff exclusions keep applying. The dispatcher builds that runtime
// list from the request's own linkedInConfig.employerExclusions (which the self-serve BFF fills
// from its ConfigMap) and nothing else, so its synchronous pre-flight and CreateCampaign validate
// the same list. Without Targeting, nothing changes.

// Targeting is linkedInConfig.targeting.
type Targeting struct {
	Include []map[string][]string `json:"include"`
	Exclude map[string][]string   `json:"exclude,omitempty"`
}

// ErrTargetingInvalid marks a Targeting refused by ValidateTargeting. Every refusal is
// service-authored apart from the quoted caller value, which goes through domain.CallerValue.
var ErrTargetingInvalid = errors.New("linkedin-ads: invalid targeting")

const (
	// MaxTargetingClauses bounds targeting.include. A local bound, not a documented LinkedIn
	// limit: ten facet clauses is already far narrower than any audience LinkedIn will serve.
	MaxTargetingClauses = 10
	// maxFacetValues is LinkedIn's per-facet cap ("The number of entities per facet is capped to
	// 100 for nearly all facets"); maxEmployerFacetValues is the documented exception for
	// employers.
	maxFacetValues         = 100
	maxEmployerFacetValues = 200
)

// Segment facets name an ad segment (a matched or retargeting audience) on the ad account.
const (
	FacetAudienceMatchingSegments = "audienceMatchingSegments"
	FacetDynamicSegments          = "dynamicSegments"
)

var (
	numericIDTail = `[1-9][0-9]{0,18}`
	// staffCountRanges are an enumerated set: LinkedIn's nine company-size buckets.
	staffCountRangeValues = []string{
		"urn:li:staffCountRange:(1,1)",
		"urn:li:staffCountRange:(2,10)",
		"urn:li:staffCountRange:(11,50)",
		"urn:li:staffCountRange:(51,200)",
		"urn:li:staffCountRange:(201,500)",
		"urn:li:staffCountRange:(501,1000)",
		"urn:li:staffCountRange:(1001,5000)",
		"urn:li:staffCountRange:(5001,10000)",
		"urn:li:staffCountRange:(10001,2147483647)",
	}
)

// targetingFacet describes one accepted facet key.
type targetingFacet struct {
	// pattern is the full value shape.
	pattern *regexp.Regexp
	// enum, when set, is the closed value set instead of pattern.
	enum []string
	// includeOnly facets cannot appear in exclude (LinkedIn's "Special Note").
	includeOnly bool
	// max is the per-facet value cap.
	max int
	// shape is the value shape named in a refusal.
	shape string
}

var targetingFacets = map[string]targetingFacet{
	"skills":           {pattern: regexp.MustCompile(`^urn:li:skill:` + numericIDTail + `$`), max: maxFacetValues, shape: "urn:li:skill:<id>"},
	"groups":           {pattern: regexp.MustCompile(`^urn:li:group:` + numericIDTail + `$`), includeOnly: true, max: maxFacetValues, shape: "urn:li:group:<id>"},
	"industries":       {pattern: regexp.MustCompile(`^urn:li:industry:` + numericIDTail + `$`), max: maxFacetValues, shape: "urn:li:industry:<id>"},
	"titles":           {pattern: regexp.MustCompile(`^urn:li:title:` + numericIDTail + `$`), max: maxFacetValues, shape: "urn:li:title:<id>"},
	"seniorities":      {pattern: regexp.MustCompile(`^urn:li:seniority:` + numericIDTail + `$`), max: maxFacetValues, shape: "urn:li:seniority:<id>"},
	"jobFunctions":     {pattern: regexp.MustCompile(`^urn:li:function:` + numericIDTail + `$`), max: maxFacetValues, shape: "urn:li:function:<id>"},
	"employers":        {pattern: regexp.MustCompile(`^urn:li:(organization|company):` + numericIDTail + `$`), max: maxEmployerFacetValues, shape: "urn:li:organization:<id> or urn:li:company:<id>"},
	"staffCountRanges": {enum: staffCountRangeValues, max: maxFacetValues, shape: "one of LinkedIn's staffCountRange values, e.g. urn:li:staffCountRange:(201,500)"},
	"interfaceLocales": {pattern: regexp.MustCompile(`^urn:li:locale:[a-z]{2}_[A-Z]{2}$`), includeOnly: true, max: maxFacetValues, shape: "urn:li:locale:<ll>_<CC>"},
	// Member interests (#585). Not on LinkedIn's include-only list, so it may be excluded too.
	"interests":                   {pattern: regexp.MustCompile(`^urn:li:interest:` + numericIDTail + `$`), max: maxFacetValues, shape: "urn:li:interest:<id>"},
	FacetAudienceMatchingSegments: {pattern: regexp.MustCompile(`^urn:li:adSegment:` + numericIDTail + `$`), max: maxFacetValues, shape: "urn:li:adSegment:<id>"},
	FacetDynamicSegments:          {pattern: regexp.MustCompile(`^urn:li:adSegment:` + numericIDTail + `$`), max: maxFacetValues, shape: "urn:li:adSegment:<id>"},
}

// TargetingFacetKeys lists the accepted facet keys, sorted, for messages and docs.
func TargetingFacetKeys() []string {
	keys := make([]string, 0, len(targetingFacets))
	for k := range targetingFacets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func targetingInvalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrTargetingInvalid, fmt.Sprintf(format, args...))
}

// quoteValue renders a caller value for a refusal through domain.CallerValue, which every
// caller value in a PreflightRefusal must pass through.
func quoteValue(v string) string { return domain.CallerValue(v) }

// cleanFacet validates and de-duplicates one facet list. where names the list in a refusal.
func cleanFacet(key string, values []string, where string, exclude bool) ([]string, error) {
	f, ok := targetingFacets[key]
	if !ok {
		return nil, targetingInvalid("%s: unknown facet %s; accepted: %s", where, quoteValue(key), strings.Join(TargetingFacetKeys(), ", "))
	}
	if exclude && f.includeOnly {
		return nil, targetingInvalid("%s: LinkedIn accepts %s only under include", where, key)
	}
	if len(values) == 0 {
		return nil, targetingInvalid("%s.%s is empty; omit the facet instead", where, key)
	}
	// A set, and a stop at the first value past the cap, keep this linear in the input however
	// long the caller's list is: it runs in the synchronous pre-flight.
	//
	// Employers are de-duplicated by employerKey, keeping the first URN: urn:li:company:<n> and
	// urn:li:organization:<n> are one employer to mergeEmployersCapped and to the wire, so the cap
	// counts identities, not spellings (#557 review).
	out := make([]string, 0, min(len(values), f.max+1))
	seen := make(map[string]struct{}, min(len(values), f.max+1))
	for _, v := range values {
		if strings.TrimSpace(v) == "" {
			return nil, targetingInvalid("%s.%s holds a blank value", where, key)
		}
		if v != strings.TrimSpace(v) {
			return nil, targetingInvalid("%s.%s value %s has surrounding whitespace", where, key, quoteValue(v))
		}
		if f.enum != nil {
			if !slices.Contains(f.enum, v) {
				return nil, targetingInvalid("%s.%s value %s is not %s", where, key, quoteValue(v), f.shape)
			}
		} else if !f.pattern.MatchString(v) {
			return nil, targetingInvalid("%s.%s value %s is not %s", where, key, quoteValue(v), f.shape)
		}
		k := v
		if key == "employers" {
			k = employerKey(v)
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, v)
		if len(out) > f.max {
			return nil, targetingInvalid("%s.%s holds more than %d distinct values; LinkedIn allows at most %d", where, key, f.max, f.max)
		}
	}
	return out, nil
}

// ValidateTargeting checks t and returns its normalised copy (duplicates within a list
// dropped). It makes no request, so the dispatcher's pre-flight and CreateCampaign share it.
//
// employerExclusions is the config's employerExclusions list, which buildCustomTargetingCriteria
// merges into exclude.employers. It is checked HERE, merged, so nothing the build adds can break
// a rule after the campaign group exists: its values must be well formed, the merged employers
// list must fit LinkedIn's cap, and no included employer may be in it. Employers are compared by
// id, so urn:li:company:<n> and urn:li:organization:<n> are the same employer.
//
// Besides the per-value shapes it refuses the facet combinations LinkedIn documents as invalid:
// titles with seniorities or jobFunctions, and employers with industries or staffCountRanges,
// when both are INCLUDED — wherever they sit. The docs forbid only ANDing them (separate
// clauses); refusing them inside one ORed clause too is deliberately stricter until a live probe
// shows that form is accepted. It also refuses staffCountRanges both included and excluded
// ("targeted either inclusively or exclusively, but not both"), and any value both included
// and excluded under the same facet.
func ValidateTargeting(t *Targeting, employerExclusions []string) (*Targeting, error) {
	if t == nil {
		return nil, nil
	}
	if len(t.Include) == 0 {
		return nil, targetingInvalid("include needs at least one clause")
	}
	if len(t.Include) > MaxTargetingClauses {
		return nil, targetingInvalid("include holds %d clauses; at most %d", len(t.Include), MaxTargetingClauses)
	}
	out := &Targeting{Include: make([]map[string][]string, 0, len(t.Include))}
	included := map[string][]string{}
	for i, clause := range t.Include {
		where := "include[" + strconv.Itoa(i) + "]"
		if len(clause) == 0 {
			return nil, targetingInvalid("%s is empty; every clause needs at least one facet", where)
		}
		clean := make(map[string][]string, len(clause))
		for key, values := range clause {
			vs, err := cleanFacet(key, values, where, false)
			if err != nil {
				return nil, err
			}
			clean[key] = vs
			included[key] = append(included[key], vs...)
		}
		out.Include = append(out.Include, clean)
	}
	if len(t.Exclude) > 0 {
		out.Exclude = make(map[string][]string, len(t.Exclude))
		for key, values := range t.Exclude {
			vs, err := cleanFacet(key, values, "exclude", true)
			if err != nil {
				return nil, err
			}
			out.Exclude[key] = vs
		}
	}
	has := func(k string) bool { return len(included[k]) > 0 }
	if has("titles") && (has("seniorities") || has("jobFunctions")) {
		return nil, targetingInvalid("LinkedIn does not accept included titles together with included seniorities or jobFunctions")
	}
	if has("employers") && (has("industries") || has("staffCountRanges")) {
		return nil, targetingInvalid("LinkedIn does not accept included employers together with included industries or staffCountRanges")
	}
	if has("staffCountRanges") && len(out.Exclude["staffCountRanges"]) > 0 {
		return nil, targetingInvalid("staffCountRanges can be included or excluded, not both")
	}
	for _, key := range sortedKeys(out.Exclude) {
		for _, v := range out.Exclude[key] {
			if facetContains(key, included[key], v) {
				return nil, targetingInvalid("%s value %s is both included and excluded", key, quoteValue(v))
			}
		}
	}
	configured, err := validFacets("employer-exclusions", employerExclusions)
	if err != nil {
		return nil, targetingInvalid("employerExclusions: %v", err)
	}
	includedEmployers := make(map[string]struct{}, len(included["employers"]))
	for _, v := range included["employers"] {
		includedEmployers[employerKey(v)] = struct{}{}
	}
	for _, v := range configured {
		if _, ok := includedEmployers[employerKey(v)]; ok {
			return nil, targetingInvalid("employers value %s is included but is in employerExclusions", quoteValue(v))
		}
	}
	if _, over := mergeEmployersCapped(out.Exclude["employers"], configured, maxEmployerFacetValues); over {
		return nil, targetingInvalid("exclude.employers with employerExclusions holds more than %d employers; LinkedIn allows at most %d", maxEmployerFacetValues, maxEmployerFacetValues)
	}
	return out, nil
}

// employerKey is the id an employer URN names; company and organization URNs of one id are the
// same employer.
func employerKey(v string) string { return trailingID(v) }

// facetContains reports whether list holds v, comparing employers by id.
func facetContains(key string, list []string, v string) bool {
	if key != "employers" {
		return slices.Contains(list, v)
	}
	for _, x := range list {
		if employerKey(x) == employerKey(v) {
			return true
		}
	}
	return false
}

// mergeEmployers appends extra to base, skipping an employer already present by id.
func mergeEmployers(base, extra []string) []string {
	out, _ := mergeEmployersCapped(base, extra, -1)
	return out
}

// mergeEmployersCapped is mergeEmployers with a set of ids, so it is linear, and an optional cap
// (negative for none): it stops and reports over as soon as the merged list passes it.
func mergeEmployersCapped(base, extra []string, limit int) (out []string, over bool) {
	seen := make(map[string]struct{}, len(base))
	add := func(v string) bool {
		k := employerKey(v)
		if _, dup := seen[k]; dup {
			return true
		}
		seen[k] = struct{}{}
		out = append(out, v)
		return limit < 0 || len(out) <= limit
	}
	for _, v := range base {
		if !add(v) {
			return nil, true
		}
	}
	for _, v := range extra {
		if !add(v) {
			return nil, true
		}
	}
	return out, false
}

// SegmentFacetsSet reports whether t names an ad segment anywhere (include or exclude). The
// create path gates those on global_marketing_ops: a segment is a resource of the ad account.
func (t *Targeting) SegmentFacetsSet() bool {
	if t == nil {
		return false
	}
	for _, clause := range t.Include {
		if len(clause[FacetAudienceMatchingSegments]) > 0 || len(clause[FacetDynamicSegments]) > 0 {
			return true
		}
	}
	return len(t.Exclude[FacetAudienceMatchingSegments]) > 0 || len(t.Exclude[FacetDynamicSegments]) > 0
}

func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func facetURN(key string) string { return "urn:li:adTargetingFacet:" + key }

// buildCustomTargetingCriteria renders a validated Targeting as the campaign's
// targetingCriteria: the geo clause first, then one include.and clause per Include entry, and
// exclude.or from Exclude plus the runtime config's employer exclusions. The caller has already
// run ValidateTargeting with the same employer exclusions.
func (c *Client) buildCustomTargetingCriteria(t *Targeting, geoURNs []string) (map[string]any, error) {
	employerExclusions, err := validFacets("employer-exclusions", c.cfg.EmployerExclusions)
	if err != nil {
		return nil, err
	}
	if geoURNs == nil {
		geoURNs = []string{}
	}
	and := []any{map[string]any{"or": map[string]any{facetURN("locations"): geoURNs}}}
	for _, clause := range t.Include {
		or := make(map[string]any, len(clause))
		for _, key := range sortedKeys(clause) {
			or[facetURN(key)] = clause[key]
		}
		and = append(and, map[string]any{"or": or})
	}
	excl := map[string][]string{}
	for k, v := range t.Exclude {
		excl[k] = append([]string(nil), v...)
	}
	if merged := mergeEmployers(excl["employers"], employerExclusions); len(merged) > 0 {
		excl["employers"] = merged
	}
	criteria := map[string]any{"include": map[string]any{"and": and}}
	if len(excl) > 0 {
		or := make(map[string]any, len(excl))
		for _, key := range sortedKeys(excl) {
			or[facetURN(key)] = excl[key]
		}
		criteria["exclude"] = map[string]any{"or": or}
	}
	return map[string]any{"targetingCriteria": criteria}, nil
}
