// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"fmt"
	"regexp"
	"strings"
)

// orgIDRE matches a valid LinkedIn organization id: a non-empty run of ASCII
// digits. The organization URN is built as "urn:li:organization:<id>", so a
// non-numeric/malformed id would produce an invalid URN that LinkedIn rejects
// only after a permanent resource already exists. Validating the shape up front
// preserves the configuration invariant the source client relies on.
var orgIDRE = regexp.MustCompile(`^[0-9]+$`)

// accountIDRE matches a valid LinkedIn ad-account id (digit-only), mirroring the
// invariant the source config parser enforces, so a malformed id can't be
// interpolated into a request URN.
var accountIDRE = regexp.MustCompile(`^[0-9]+$`)

// geoURNRE matches a LinkedIn geo URN (urn:li:geo:<digits>), used to reject a
// caller-supplied GeoTarget with a malformed URN before any campaign is created.
var geoURNRE = regexp.MustCompile(`^urn:li:geo:[0-9]+$`)

// imageURNRE matches a LinkedIn digital-asset URN as accepted by createDarkPost,
// which sends a variant's ImageURN verbatim as the article thumbnail. LinkedIn
// image assets are addressed as either urn:li:image:<id> or
// urn:li:digitalmediaAsset:<id>. ImageURN is optional (an empty value is
// allowed), but a non-empty malformed value is rejected up front — otherwise it
// reaches LinkedIn only AFTER the campaign group and campaign already exist.
//
// The id portion is constrained to LinkedIn's realistic asset-id charset —
// alphanumerics plus '-'/'_' (e.g. "C4E10AQabc_1-2") — rather than a loose ".+".
// A ".+" tail accepted values that would build a malformed thumbnail URN, such as
// "urn:li:image: " (a trailing space) or a value carrying URL delimiters like
// "urn:li:image:a/b"; the tightened class rejects both while still matching every
// legitimate asset id.
var imageURNRE = regexp.MustCompile(`^urn:li:(image|digitalmediaAsset):[A-Za-z0-9_-]+$`)

// facetURNRE matches a LinkedIn facet member id after its namespace prefix.
// Skills, groups, and organizations are addressed by NUMERIC LinkedIn entity
// ids, so a value like urn:li:skill:abc (non-numeric) is rejected.
var facetURNRE = regexp.MustCompile(`^[0-9]+$`)

// facetNamespace is the required urn:li:<type>: prefix for each facet kind, so a
// value from the wrong namespace (e.g. an organization URN under skills) is
// rejected rather than silently sent under the wrong facet.
var facetNamespace = map[string]string{
	"skills":              "urn:li:skill:",
	"groups":              "urn:li:group:",
	"employer-exclusions": "urn:li:organization:",
}

// validFacets returns the non-blank entries of in and an error naming the first
// entry that is non-blank but not a well-formed facet URN in the namespace
// required for kind. Used to fail fast before any permanent resource is created.
func validFacets(kind string, in []string) ([]string, error) {
	prefix, ok := facetNamespace[kind]
	if !ok {
		return nil, fmt.Errorf("unknown LinkedIn facet kind %q", kind)
	}
	out := nonBlankFacets(in)
	for _, v := range out {
		id, found := strings.CutPrefix(v, prefix)
		if !found || !facetURNRE.MatchString(id) {
			return nil, fmt.Errorf("malformed LinkedIn %s facet %q — expected a %s<id> value", kind, v, prefix)
		}
	}
	return out, nil
}

// nonBlankFacets returns the entries of in with surrounding whitespace trimmed,
// dropping any entry that is blank (empty or whitespace-only) after trimming. A
// targeting facet slice supplied through the injected RuntimeConfig can contain
// blank strings (e.g. []string{""} or {"  "}); such an entry is not a usable
// facet — LinkedIn would reject or silently ignore it — so it must never be sent
// on the wire nor counted toward a profile being "usable". Both the usability
// check (validatePrerequisites) and the wire builder (buildTargetingCriteria)
// funnel their skills/groups through this helper so a blank facet can neither
// make a profile look usable nor reach LinkedIn. The result is always non-nil so
// it JSON-encodes as [] rather than null.
func nonBlankFacets(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if t := strings.TrimSpace(v); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ResolveGeoTargets resolves location names to GeoTarget URNs using the static
// geoResolveMap. Mirrors the cached branch of resolveGeoTargets: names are
// lowercased and trimmed before lookup. Unknown geos are skipped (omitted from
// the result) rather than causing an error — the same graceful-degradation
// behavior the TypeScript uses when a name is neither in the map nor resolvable
// via the API. This Go port intentionally does not perform the network
// fallback (see package docs / final report).
func ResolveGeoTargets(locationNames []string) []GeoTarget {
	resolved := make([]GeoTarget, 0, len(locationNames))
	for _, name := range locationNames {
		key := strings.ToLower(strings.TrimSpace(name))
		if hit, ok := geoResolveMap[key]; ok {
			resolved = append(resolved, hit)
		}
	}
	return resolved
}

// accountURN builds the sponsored-account URN. Mirrors accountUrn().
func accountURN(accountID string) string {
	return "urn:li:sponsoredAccount:" + accountID
}

// resolveOrgID returns the organization ID paired with accountID. It prefers a
// match in the runtime config's accounts list, then falls back to the default
// org. Mirrors resolveOrgId()/getOrgId() minus the env-var branch (env is not
// consulted in this package). Returns an error when no org can be resolved.
//
// The resolved org id is validated to be numeric (see orgIDRE) before it is
// returned: checking only for an empty id let a present-but-malformed value
// (e.g. a full "urn:li:organization:123" URN, or a non-numeric string) flow
// into orgURN, which would build an invalid double-prefixed / malformed URN
// that LinkedIn rejects only after permanent resources exist. This preserves
// the configuration invariant the source client relies on.
func (c *Client) resolveOrgID(accountID string) (string, error) {
	for _, a := range c.cfg.Accounts {
		if a.AccountID == accountID {
			if a.OrgID == "" {
				return "", fmt.Errorf("LinkedIn account %q has no orgId configured", accountID)
			}
			if !orgIDRE.MatchString(a.OrgID) {
				return "", fmt.Errorf("LinkedIn account %q has a malformed orgId %q — expected a numeric organization id", accountID, a.OrgID)
			}
			return a.OrgID, nil
		}
	}
	// Not in accounts list. Only fall back to the default org when this IS the
	// default account; otherwise fail closed to avoid cross-tenant pairing
	// (mirrors getOrgId's refusal to pair an override account with the default
	// org).
	if accountID == c.cfg.DefaultAccountID {
		if c.cfg.DefaultOrgID != "" {
			if !orgIDRE.MatchString(c.cfg.DefaultOrgID) {
				return "", fmt.Errorf("LinkedIn defaultOrgId %q is malformed — expected a numeric organization id", c.cfg.DefaultOrgID)
			}
			return c.cfg.DefaultOrgID, nil
		}
		return "", fmt.Errorf("no LinkedIn org configured: provide defaultOrgId in the runtime config")
	}
	return "", fmt.Errorf("LinkedIn ad account %q is not in the configured accounts list — refusing to fall back to default org to avoid cross-tenant pairing", accountID)
}

// orgURN builds the organization URN for accountID. Mirrors orgUrn().
func (c *Client) orgURN(accountID string) (string, error) {
	orgID, err := c.resolveOrgID(accountID)
	if err != nil {
		return "", err
	}
	return "urn:li:organization:" + orgID, nil
}

// resolveAccountID resolves the effective ad-account ID. An override must exist
// in the runtime config's accounts list; when empty, the default account is
// used. Mirrors getAccountId() minus the env branch.
func (c *Client) resolveAccountID(override string) (string, error) {
	if override != "" {
		for _, a := range c.cfg.Accounts {
			if a.AccountID == override {
				if !accountIDRE.MatchString(override) {
					return "", fmt.Errorf("invalid LinkedIn ad account ID %q: must be numeric", override)
				}
				return override, nil
			}
		}
		return "", fmt.Errorf("unsupported LinkedIn ad account ID %q — not in the runtime config", override)
	}
	if c.cfg.DefaultAccountID != "" {
		// Enforce the digit-only invariant the source config parser guarantees, so
		// a malformed configured id can't be interpolated into a request URN.
		if !accountIDRE.MatchString(c.cfg.DefaultAccountID) {
			return "", fmt.Errorf("invalid default LinkedIn ad account ID %q: must be numeric", c.cfg.DefaultAccountID)
		}
		return c.cfg.DefaultAccountID, nil
	}
	return "", fmt.Errorf("no LinkedIn ad account configured: provide defaultAccountId in the runtime config")
}

// buildTargetingCriteria builds the targetingCriteria block for a campaign.
// Mirrors buildTargetingCriteria(): skills, groups, and jobFunctions go into a
// SINGLE `or` block alongside the geo `or` block (both under one `and`).
// Employer and seniority exclusions form the `exclude.or` block.
//
// The profile "custom" is treated as an alias for "cloud-native" (matching the
// TypeScript fallback). Any other profile must exist in the runtime config.
func (c *Client) buildTargetingCriteria(profile string, geoURNs []string) (map[string]any, error) {
	var skills, groups []string

	lookup := profile
	if profile == "custom" {
		lookup = "cloud-native"
	}

	var found *TargetingProfileConfig
	for i := range c.cfg.TargetingProfiles {
		if c.cfg.TargetingProfiles[i].ID == lookup {
			found = &c.cfg.TargetingProfiles[i]
			break
		}
	}

	if found == nil {
		if profile == "custom" {
			// Builder-level tolerance only: the public entry (CreateCampaign) calls
			// validatePrerequisites, which REQUIRES the aliased cloud-native profile
			// to exist even for "custom" (matching validateLinkedInPrerequisites), so
			// this empty-fallback branch isn't reached via the public flow — it just
			// keeps the low-level builder total for direct/test use.
			skills = []string{}
			groups = []string{}
		} else {
			return nil, fmt.Errorf("LinkedIn targeting profile %q not found in runtime config", profile)
		}
	} else {
		skills = found.Skills
		groups = found.Groups
	}

	// Drop blank/whitespace-only entries and trim the rest before they reach
	// LinkedIn: a config-supplied facet slice can carry blank strings (e.g.
	// []string{""} or {"  "}) that are not usable facets. nonBlankFacets also
	// normalizes a nil slice to a non-nil empty slice so the JSON encodes as []
	// not null (matching the TypeScript spread of possibly-empty arrays).
	var ferr error
	if skills, ferr = validFacets("skills", skills); ferr != nil {
		return nil, ferr
	}
	if groups, ferr = validFacets("groups", groups); ferr != nil {
		return nil, ferr
	}
	employerExclusions, ferr := validFacets("employer-exclusions", c.cfg.EmployerExclusions)
	if ferr != nil {
		return nil, ferr
	}
	if geoURNs == nil {
		geoURNs = []string{}
	}

	return map[string]any{
		"targetingCriteria": map[string]any{
			"include": map[string]any{
				"and": []any{
					map[string]any{
						"or": map[string]any{
							"urn:li:adTargetingFacet:locations": geoURNs,
						},
					},
					map[string]any{
						"or": map[string]any{
							"urn:li:adTargetingFacet:skills":       skills,
							"urn:li:adTargetingFacet:groups":       groups,
							"urn:li:adTargetingFacet:jobFunctions": jobFunctions,
						},
					},
				},
			},
			"exclude": map[string]any{
				"or": map[string]any{
					"urn:li:adTargetingFacet:employers":   employerExclusions,
					"urn:li:adTargetingFacet:seniorities": seniorityExclusions,
				},
			},
		},
	}, nil
}
