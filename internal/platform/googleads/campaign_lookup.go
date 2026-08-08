// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Campaign lookup by name: the read half of brief-to-campaign binding.
//
// The other clients (linkedin, twitter, microsoft, meta) grew a find-by-name to
// make CREATE idempotent — don't double-create when a retry lands after the first
// attempt already committed. This one exists for that too, but the reason it is
// being added now is ADOPTION: binding a brief to a campaign that already exists
// on the platform and that this service never created. Those two uses want the
// same query and the same fail-closed semantics, so they share one method.
// ---------------------------------------------------------------------------

// gaqlStringLiteral renders s as a single-quoted GAQL string literal, escaping the
// two characters that can terminate or extend one.
//
// EVERY other GAQL query in this package interpolates either a digits-only id
// (customerIDRE) or a value from a closed allow-list (validMetricsWindows in
// metrics.go), so until now nothing here has needed to quote free text. A campaign
// NAME is the first genuinely caller-controlled string to reach a WHERE clause, and
// it cannot be allow-listed — an operator may name a campaign anything Google
// accepts. Without escaping, a name containing a quote closes the literal and the
// remainder is parsed as query syntax: `x' OR campaign.id > '0` turns an exact-match
// lookup into a match on everything in the account, and the caller would then bind a
// brief to whichever unrelated campaign came back first.
//
// GAQL follows the usual backslash convention, so the escape set is exactly two
// characters and ORDER MATTERS: backslash must be doubled FIRST, or the backslash
// introduced when escaping a quote would itself be escaped a second time and the
// quote would be released.
//
// Control characters are REJECTED rather than escaped. GAQL has no portable escape
// for them, Google Ads forbids them in a campaign name outright (sanitizeNamePart
// strips them on the create path for the same reason), and a name containing one
// therefore cannot match a real campaign — so rejecting loses no reachable lookup
// while keeping a NUL or newline from ever reaching the query builder.
func gaqlStringLiteral(s string) (string, error) {
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("google-ads: campaign name contains a control character (%U) and cannot appear in a query", r)
		}
	}
	escaped := strings.ReplaceAll(s, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `'`, `\'`)
	return "'" + escaped + "'", nil
}

// campaignLookupRow is one row of the FindCampaignByName SELECT. The field set is
// the minimum the decision needs: the id to return, the name to re-verify the
// server honoured the filter, and the status to drop REMOVED campaigns.
type campaignLookupRow struct {
	Campaign struct {
		ResourceName string `json:"resourceName"`
		ID           string `json:"id"`
		Name         string `json:"name"`
		Status       string `json:"status"`
	} `json:"campaign"`
}

// StatusRemoved is Google's tombstone state. A removed campaign still matches a
// name query and can never serve or be re-enabled, so it must not be adopted or
// treated as an idempotent create hit.
const StatusRemoved = "REMOVED"

// FindCampaignByName returns the numeric id of the single non-removed campaign in
// this account whose name is exactly name.
//
// The contract mirrors the meta and linkedin lookups, because the callers make the
// same decision from the result:
//
//   - exactly one live match  -> (id, nil)
//   - no live match           -> ("", nil)   — a clean, trustworthy absence
//   - more than one match     -> ("", error) — AMBIGUOUS, never a silent pick
//   - anything unverifiable   -> ("", error)
//
// The distinction between the second and third cases is the whole point, and it is
// why this returns an error rather than the first hit. Both callers act
// destructively on an absence: the create path takes ("", nil) as licence to create
// a campaign, and the adoption path takes it as licence to report that there is
// nothing to adopt. Reporting a false absence therefore produces a duplicate paid
// campaign, and picking arbitrarily among several same-name campaigns binds a brief
// to the wrong one — with real spend attached either way. So every uncertain
// outcome fails closed.
//
// Google Ads permits duplicate campaign names within an account (unlike the budget
// name, which the create path already handles as a duplicate error), so the
// multiple-match case is genuinely reachable and not a defensive branch.
func (c *Client) FindCampaignByName(ctx context.Context, name string) (string, error) {
	if err := c.validateAccountIDs(); err != nil {
		return "", err
	}

	// Trim to match the create path: composeName runs its parts through
	// sanitizeNamePart, which trims and collapses whitespace, so the stored name never
	// carries surrounding space. Looking up the untrimmed form would miss it.
	lookup := strings.TrimSpace(name)
	if lookup == "" {
		return "", fmt.Errorf("google-ads: cannot look up a campaign by an empty name")
	}
	// Bound the name with the SAME limit and the SAME unit the create path enforces
	// (maxCampaignNameRunes, characters — see validateEntityName). A name longer than
	// Google accepts cannot identify a stored campaign, so this is an unmatchable query
	// rather than a valid miss, and rejecting it keeps an unbounded caller string out of
	// the query builder.
	if n := utf8.RuneCountInString(lookup); n > maxCampaignNameRunes {
		return "", fmt.Errorf("google-ads: campaign name for lookup exceeds %d characters (%d)", maxCampaignNameRunes, n)
	}

	literal, err := gaqlStringLiteral(lookup)
	if err != nil {
		return "", err
	}

	// The name filter is applied SERVER-side so a miss costs one page instead of a walk
	// of every campaign in the account, and REMOVED is excluded in the same clause so a
	// long tail of tombstones cannot page the query out. Both predicates are re-checked
	// on the rows below; see there for why.
	// The enum is single-quoted to match the only other enum predicate in this package
	// (listManagerClients' `customer_client.status = 'ENABLED'`); GAQL accepts either
	// form, and consistency here is worth more than brevity.
	query := "SELECT campaign.id, campaign.name, campaign.status, campaign.resource_name " +
		"FROM campaign WHERE campaign.name = " + literal +
		" AND campaign.status != '" + StatusRemoved + "'"

	rows, err := c.gaqlSearch(ctx, query)
	if err != nil {
		return "", fmt.Errorf("google-ads campaign lookup: %w", err)
	}

	var matches []string
	for _, raw := range rows {
		var row campaignLookupRow
		if err := json.Unmarshal(raw, &row); err != nil {
			// A 2xx row we cannot decode is NOT a non-match. Treating it as one would
			// let a malformed response report a false absence, and the caller would
			// create a duplicate. Fail the lookup instead.
			return "", fmt.Errorf("google-ads campaign lookup: decoding result row: %w", err)
		}

		// Re-check both predicates client-side even though the WHERE clause already
		// carries them. This is not belt-and-braces: it is what makes an escaping
		// regression LOUD. If gaqlStringLiteral ever stopped neutralising a quote, the
		// injected query would still return 2xx with rows for OTHER campaigns, and a
		// server-side-only filter would hand one of them back as an exact match. The
		// client-side equality check turns that from a silent wrong binding into a
		// visible zero-match or ambiguity error.
		if row.Campaign.Name != lookup {
			continue
		}
		if row.Campaign.Status == StatusRemoved {
			continue
		}

		id := strings.TrimSpace(row.Campaign.ID)
		if id == "" {
			// Fall back to the resource name, which is "customers/{cid}/campaigns/{id}";
			// v23 returns campaign.id as a string but an int64 field can arrive absent.
			id = resourceID(row.Campaign.ResourceName)
		}
		// A matched row with no usable id is the fail-closed case linkedin's findMatch
		// documents: the server says a campaign with this name exists, but we cannot
		// name it. That cannot be reported as an absence.
		if id == "" {
			return "", fmt.Errorf("google-ads campaign lookup: campaign named %q has no usable id (resourceName %q); aborting rather than report it absent", lookup, row.Campaign.ResourceName)
		}
		if !customerIDRE.MatchString(id) {
			// The id is interpolated into resource paths by every later call, so a
			// non-numeric one must never leave this function.
			return "", fmt.Errorf("google-ads campaign lookup: campaign named %q returned non-numeric id %q", lookup, id)
		}
		matches = append(matches, id)
	}

	// Deduplicate before counting. GAQL can return the same campaign on more than one
	// row when the query joins a repeated resource, and counting rows rather than
	// distinct campaigns would report a single campaign as ambiguous and block an
	// adoption that is in fact unambiguous.
	unique := make([]string, 0, len(matches))
	seen := make(map[string]struct{}, len(matches))
	for _, id := range matches {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}

	switch len(unique) {
	case 0:
		return "", nil
	case 1:
		return unique[0], nil
	default:
		return "", fmt.Errorf("google-ads campaign lookup: %d campaigns in this account are named %q (ids %s) — refusing to choose one; rename or specify the campaign id directly", len(unique), lookup, strings.Join(unique, ", "))
	}
}
