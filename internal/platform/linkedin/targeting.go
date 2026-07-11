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
			// The TypeScript treats a missing cloud-native fallback as empty
			// skills/groups rather than an error.
			skills = []string{}
			groups = []string{}
		} else {
			return nil, fmt.Errorf("LinkedIn targeting profile %q not found in runtime config", profile)
		}
	} else {
		skills = found.Skills
		groups = found.Groups
	}

	// Normalize nil slices to empty so the JSON encodes as [] not null, matching
	// the TypeScript spread of possibly-empty arrays.
	if skills == nil {
		skills = []string{}
	}
	if groups == nil {
		groups = []string{}
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
					"urn:li:adTargetingFacet:employers":   append([]string{}, c.cfg.EmployerExclusions...),
					"urn:li:adTargetingFacet:seniorities": seniorityExclusions,
				},
			},
		},
	}, nil
}
