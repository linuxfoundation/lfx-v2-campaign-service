// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Campaign lookup by name: the read half of brief-to-campaign binding.
//
// The other clients grew a find-by-name to make CREATE idempotent. This one serves
// that too, but exists now for ADOPTION: binding a brief to a campaign that already
// exists on the platform and that this service never created. Both uses want the
// same query and the same fail-closed semantics, so they share one method.
// ---------------------------------------------------------------------------

// gaqlStringLiteral renders s as a single-quoted GAQL string literal, escaping the
// two characters that can terminate or extend one.
//
// EVERY other GAQL query here interpolates a digits-only id (customerIDRE) or a value
// from a closed allow-list (validMetricsWindows), so nothing has needed to quote free
// text. A campaign NAME is the first genuinely caller-controlled string to reach a
// WHERE clause and cannot be allow-listed. Unescaped, `x' OR campaign.id > '0` closes
// the literal and turns an exact-match lookup into a match on the whole account.
//
// ORDER MATTERS: backslash must be doubled FIRST, or the backslash introduced when
// escaping a quote is escaped a second time and the quote is released.
//
// The rejected set is EXACTLY the three characters Google Ads prohibits in
// Campaign.name — NUL, LF and CR. Rejecting them costs no reachable lookup, since such
// a name cannot be real. Rejecting MORE is a bug: adoption targets never ran through
// sanitizeNamePart, and Google accepts TAB, U+2028/U+2029 and zero-width joiners, so
// refusing one answers "no such campaign" about a campaign that exists — the false
// absence that licenses a duplicate PAID campaign. Passing the rest through is safe:
// the query rides in a JSON body and encoding/json escapes those runes on the way out.
func gaqlStringLiteral(s string) (string, error) {
	for _, r := range s {
		if r == '\x00' || r == '\n' || r == '\r' {
			return "", fmt.Errorf("google-ads: campaign name contains %U, which Google Ads forbids in a campaign name, so it cannot match a real campaign", r)
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

// campaignIDFromResourceName extracts a campaign id from a resource name, accepting
// ONLY the exact shape Google documents and only for THIS client's account:
//
//	customers/{this account's customer id}/campaigns/{digits}
//
// It returns "" for anything else, which the caller turns into a fail-closed error.
//
// Deliberately stricter than the package's resourceID helper, which returns the
// trailing path segment. resourceID is right for parsing a mutate response WE issued —
// the server minted that name for the entity we just created. Here the resource name is
// identity evidence for a campaign about to have a brief bound to it, so "after the last
// slash" is not enough: it accepts "garbage/4242" and another customer's campaign.
func (c *Client) campaignIDFromResourceName(resourceName string) string {
	parts := strings.Split(resourceName, "/")
	if len(parts) != 4 || parts[0] != "customers" || parts[2] != "campaigns" {
		return ""
	}
	// The account segment must be THIS account. A cross-customer resource name is not
	// a campaign this client may adopt.
	if parts[1] != c.account.CustomerID {
		return ""
	}
	// customerIDRE is the package's digits-only matcher; campaign ids are int64s in
	// the same textual form, and the caller re-checks the result against it too.
	if !customerIDRE.MatchString(parts[3]) {
		return ""
	}
	return parts[3]
}

// FindCampaignByName returns the numeric id of the single live campaign in this
// account whose name is exactly name.
//
// The contract mirrors the fail-closed LOGIC of the other clients' lookups (meta's
// findCampaignByName, linkedin's findMatch, twitter's and microsoft's), because callers
// make the same decision from the result. It is the first one EXPORTED, though: the
// others are called only from inside their own create path, this one also from the
// dispatch layer for adoption.
//
// The outcomes:
//
//   - exactly one live match  -> (id, nil)
//   - no live match           -> ("", nil)   — a clean, trustworthy absence
//   - more than one match     -> ("", error) — AMBIGUOUS, never a silent pick
//   - anything unverifiable   -> ("", error)
//
// The distinction between the second and third cases is the whole point, and why this
// errors rather than taking the first hit. Both callers act destructively on an absence:
// create takes ("", nil) as licence to create, adoption as licence to report nothing to
// adopt. A false absence therefore produces a duplicate paid campaign, and an arbitrary
// pick among same-name campaigns binds a brief to the wrong one — real spend either way.
// Every uncertain outcome fails closed.
//
// Google Ads permits duplicate campaign names within an account (unlike the budget
// name, which the create path already handles as a duplicate error), so the
// multiple-match case is genuinely reachable and not a defensive branch.
func (c *Client) FindCampaignByName(ctx context.Context, name string) (string, error) {
	if err := c.validateAccountIDs(); err != nil {
		return "", err
	}

	// The name is used VERBATIM — no TrimSpace. Trimming is a no-op for the create path
	// (composeName's output is already trimmed) and a silent contract change for adoption,
	// where a request for "  foo  " would return the campaign named "foo" and hide the
	// ambiguity if both existed. TrimSpace below only DETECTS whitespace-only input.
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("google-ads: cannot look up a campaign by an empty name")
	}
	lookup := name
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
		// server-side-only filter would hand one of them back as an exact match.
		//
		// A name mismatch is an ERROR, not a skip. Skipping would reduce an injected or
		// otherwise unhonoured query to "no rows matched", i.e. ("", nil) — precisely the
		// false absence this method exists to prevent, and the caller would then create a
		// duplicate paid campaign. The server was asked for an exact match; a row that is
		// not one means the filter did not take effect, which invalidates the WHOLE
		// response, not just this row. So fail closed on the response as a whole.
		//
		// The comparison is byte-exact, matching how composeName builds the stored name.
		// If Google ever answered case-insensitively or with different Unicode
		// normalisation, this would fail closed with the error below rather than silently
		// binding — visible and safe, which is the point.
		if row.Campaign.Name != lookup {
			return "", fmt.Errorf("google-ads campaign lookup: exact-match query for %q returned a campaign named %q; the name filter was not honoured, refusing to trust this response", lookup, row.Campaign.Name)
		}

		// Status, by contrast, IS a per-row skip when it is REMOVED — and only then. A
		// tombstone is unambiguously not adoptable no matter why it arrived, so dropping
		// it can only ever be correct, whereas a wrong NAME means the query itself failed.
		//
		// Every other status fails closed. Google can return UNSPECIFIED or UNKNOWN (and
		// an omitted field decodes to ""), and treating an unrecognised status as live
		// would return the id of a campaign whose serving state we could not establish.
		switch row.Campaign.Status {
		case StatusRemoved:
			continue
		case StatusEnabled, StatusPaused:
			// Live: the only two states a real, adoptable campaign can be in.
		default:
			return "", fmt.Errorf("google-ads campaign lookup: campaign named %q has unrecognised status %q (want %s, %s or %s); refusing to treat it as live", lookup, row.Campaign.Status, StatusEnabled, StatusPaused, StatusRemoved)
		}

		// The resource name is validated WHENEVER it is present, not only as a fallback
		// for a missing campaign.id. Both fields were selected, so both are evidence of
		// what this row IS: a resource name that is malformed, or scoped to a different
		// customer, sitting beside a plausible-looking id means the row does not identify
		// a single campaign in THIS account. Preferring the id there would bind a brief
		// to a campaign whose ownership we just failed to verify. Validating only in the
		// fallback makes the check reachable exactly when the row is least suspicious.
		id := strings.TrimSpace(row.Campaign.ID)
		fromName := c.campaignIDFromResourceName(row.Campaign.ResourceName)
		if rn := strings.TrimSpace(row.Campaign.ResourceName); rn != "" && fromName == "" {
			return "", fmt.Errorf("google-ads campaign lookup: campaign named %q has resource name %q, which is malformed or scoped to another customer; refusing to adopt it", lookup, rn)
		}
		switch {
		case id == "":
			// v23 returns campaign.id as a string, but an int64 field can arrive absent.
			// The fallback validated the FULL shape above rather than taking the trailing
			// segment: bare resourceID would read "garbage/4242" as campaign 4242.
			id = fromName
		case fromName != "" && fromName != id:
			return "", fmt.Errorf("google-ads campaign lookup: campaign named %q reports id %q but resource name %q; its two identity fields disagree, refusing to adopt it", lookup, id, row.Campaign.ResourceName)
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
