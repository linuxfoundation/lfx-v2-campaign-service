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

// imageURNRE matches a LinkedIn image-asset URN as accepted by createDarkPost,
// which sends a variant's ImageURN verbatim as the article thumbnail. At
// LinkedIn-Version 202602 the Posts API article thumbnail field requires an
// Images-API urn:li:image:<id> (per LinkedIn's Posts API docs, article thumbnails
// must reference an Images-API image URN); the legacy urn:li:digitalmediaAsset:<id>
// form belonged to the deprecated ugcPosts/shares APIs and is no longer accepted,
// so admitting it here would let a value through that fails ONLY after the campaign
// group and campaign already exist, orphaning them. ImageURN is optional (an empty
// value is allowed), but a non-empty value that isn't a well-formed image URN is
// rejected up front.
//
// The id portion is constrained to LinkedIn's realistic asset-id charset —
// alphanumerics plus '-'/'_' (e.g. "C4E10AQabc_1-2") — rather than a loose ".+".
// A ".+" tail accepted values that would build a malformed thumbnail URN, such as
// "urn:li:image: " (a trailing space) or a value carrying URL delimiters like
// "urn:li:image:a/b"; the tightened class rejects both while still matching every
// legitimate asset id.
var imageURNRE = regexp.MustCompile(`^urn:li:image:[A-Za-z0-9_-]+$`)

// facetURNRE matches a LinkedIn facet member id after its namespace prefix.
// Skills, groups, and organizations are addressed by NUMERIC LinkedIn entity
// ids, so a value like urn:li:skill:abc (non-numeric) is rejected.
var facetURNRE = regexp.MustCompile(`^[0-9]+$`)

// facetNamespace is the accepted urn:li:<type>: prefix(es) for each facet kind, so
// a value from the wrong namespace (e.g. an organization URN under skills) is
// rejected rather than silently sent under the wrong facet.
//
// employer-exclusions accepts BOTH urn:li:company:<id> and
// urn:li:organization:<id>. The documented service contract
// (docs/api-catalog.md) specifies the LF/CNCF employer exclusions as
// urn:li:company:<id>, and LinkedIn's `employers` targeting facet is addressed by
// the company namespace — so urn:li:company MUST be accepted. urn:li:organization
// (the newer Marketing-API org entity, and what earlier config used) is also kept
// so an existing runtime config that supplied organization URNs keeps working.
var facetNamespace = map[string][]string{
	"skills":              {"urn:li:skill:"},
	"groups":              {"urn:li:group:"},
	"employer-exclusions": {"urn:li:company:", "urn:li:organization:"},
}

// validFacets returns the non-blank entries of in and an error naming the first
// entry that is non-blank but not a well-formed facet URN in one of the
// namespaces accepted for kind. Used to fail fast before any permanent resource
// is created.
func validFacets(kind string, in []string) ([]string, error) {
	prefixes, ok := facetNamespace[kind]
	if !ok {
		return nil, fmt.Errorf("unknown LinkedIn facet kind %q", kind)
	}
	out := nonBlankFacets(in)
	for _, v := range out {
		if !matchesAnyFacetPrefix(v, prefixes) {
			return nil, fmt.Errorf("malformed LinkedIn %s facet %q — expected a %s<id> value", kind, v, strings.Join(prefixes, "<id> or a "))
		}
	}
	return out, nil
}

// matchesAnyFacetPrefix reports whether v is one of prefixes followed by a
// well-formed (numeric) LinkedIn entity id.
func matchesAnyFacetPrefix(v string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if id, found := strings.CutPrefix(v, prefix); found && facetURNRE.MatchString(id) {
			return true
		}
	}
	return false
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
// lowercased and trimmed before lookup. This Go port intentionally does not
// perform the network fallback (see package docs / final report).
//
// A name not present in geoResolveMap is NOT silently dropped: it is returned in
// the second result (unresolved), preserving the caller's ORIGINAL input spelling
// (not the lowercased lookup key), so the caller can surface the narrowing of the
// audience (e.g. as a Step) instead of quietly targeting fewer locations than
// requested. Names that are empty or whitespace-only after trimming are ignored
// entirely — they are not real location inputs, so they are neither resolved nor
// reported as unresolved.
func ResolveGeoTargets(locationNames []string) (resolved []GeoTarget, unresolved []string) {
	resolved = make([]GeoTarget, 0, len(locationNames))
	for _, name := range locationNames {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		if hit, ok := geoResolveMap[key]; ok {
			resolved = append(resolved, hit)
			continue
		}
		// Report the ORIGINAL spelling so the caller's surfaced message matches what
		// was requested.
		unresolved = append(unresolved, name)
	}
	return resolved, unresolved
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
//
// Rather than returning the FIRST matching Accounts entry, resolution FAILS
// CLOSED on contradictory tenant mappings: returning the first match silently
// accepted config where duplicate Accounts entries disagree on the orgId, or
// where the default account's orgId conflicts with DefaultOrgID — violating the
// advertised fail-closed / no-cross-tenant-pairing behavior. So all matching
// entries are scanned for agreement, and (for the default account) the resolved
// orgId is cross-checked against DefaultOrgID when that is set.
func (c *Client) resolveOrgID(accountID string) (string, error) {
	// Scan ALL entries so contradictory duplicate mappings are detected rather
	// than silently resolving to whichever entry appears first.
	resolved := ""
	for _, a := range c.cfg.Accounts {
		if a.AccountID != accountID {
			continue
		}
		if a.OrgID == "" {
			return "", fmt.Errorf("LinkedIn account %q has no orgId configured", accountID)
		}
		if !orgIDRE.MatchString(a.OrgID) {
			return "", fmt.Errorf("LinkedIn account %q has a malformed orgId %q — expected a numeric organization id", accountID, a.OrgID)
		}
		if resolved == "" {
			resolved = a.OrgID
		} else if resolved != a.OrgID {
			return "", fmt.Errorf("LinkedIn account %q has contradictory orgId mappings (%q vs %q) in the configured accounts list — refusing to guess which tenant to use", accountID, resolved, a.OrgID)
		}
	}
	if resolved != "" {
		// For the default account, a configured DefaultOrgID that disagrees with the
		// account's own orgId is a cross-tenant conflict; fail closed rather than
		// silently preferring one.
		if accountID == c.cfg.DefaultAccountID && c.cfg.DefaultOrgID != "" && c.cfg.DefaultOrgID != resolved {
			return "", fmt.Errorf("LinkedIn default account %q has orgId %q that conflicts with defaultOrgId %q — refusing to guess which tenant to use", accountID, resolved, c.cfg.DefaultOrgID)
		}
		return resolved, nil
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
			// This branch handles "custom" when the aliased cloud-native profile is
			// ABSENT. It is NOT reached via the public flow: CreateCampaign calls
			// validatePrerequisites, which REQUIRES cloud-native to EXIST (and to
			// have usable facets) for "custom", returning an error when it is absent
			// or empty. This absent-profile branch only keeps the low-level builder
			// total for direct/test callers that bypass validatePrerequisites.
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
