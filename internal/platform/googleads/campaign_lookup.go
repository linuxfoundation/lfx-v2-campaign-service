// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
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
// EVERY other GAQL query here interpolates a digits-only id or a value from a closed
// allow-list, so nothing has needed to quote free text. A campaign NAME is the first
// genuinely caller-controlled string to reach a WHERE clause and cannot be allow-listed:
// unescaped, `x' OR campaign.id > '0` closes the literal and matches the whole account.
// ORDER MATTERS — the backslash must be doubled FIRST, or the one introduced when
// escaping a quote is escaped again and the quote is released.
//
// The rejected set is EXACTLY the three characters Google Ads prohibits in Campaign.name
// — NUL, LF, CR — which cannot appear in a real name, so rejecting them costs no
// reachable lookup. Rejecting MORE is a bug: adoption targets never ran through
// sanitizeNamePart and Google accepts TAB, U+2028/U+2029 and zero-width joiners, so
// refusing one answers "no such campaign" about a campaign that exists — the false
// absence that licenses a duplicate PAID campaign. The rest is safe to pass through: the
// query rides in a JSON body and encoding/json escapes those runes on the way out.
//
// Invalid UTF-8 is refused separately, and for a different reason than the three forbidden
// runes: not because Google would reject the name, but because this process would CHANGE it.
// encoding/json substitutes U+FFFD for each malformed byte and returns no error, so a name
// carrying one is silently rewritten between this function and the wire — the query asks
// about a name the caller never passed, and its inevitable miss is reported as the clean
// ("", nil) absence that licenses a create. The range loop above cannot catch this: ranging
// a malformed byte yields utf8.RuneError, which is none of NUL, LF or CR. Nor can the
// row-level name re-check, because a query that matches nothing returns no row to check.
// Rejecting costs no reachable lookup either — Google Ads' JSON and proto surfaces both
// require valid UTF-8, so no stored campaign name can contain a malformed byte.
func gaqlStringLiteral(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("google-ads: campaign name for lookup is not valid UTF-8; encoding it would substitute U+FFFD and query a different name")
	}
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
// Deliberately stricter than the package's resourceID helper, which returns the trailing
// path segment: that is right for parsing a mutate response WE issued — the server minted
// the name for an entity we just created — but here the resource name is identity evidence
// for a campaign about to have a brief bound to it, and "after the last slash" accepts
// "garbage/4242" and another customer's campaign.
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
	return canonicalCampaignID(parts[3])
}

// canonicalCampaignID returns s when it is the canonical base-10 spelling of a positive
// int64, and "" otherwise.
//
// customerIDRE (`^[0-9]+$`) is the package's digits-only matcher and it is NOT enough
// here, because this is an IDENTITY check rather than an interpolation-safety one.
// Google exposes campaign ids as int64, so "0", a value past math.MaxInt64, and the
// leading-zero spelling "007" all match that regex and none of them names a campaign
// this client can adopt. "007" is the dangerous one: it is the same campaign as "7" to
// the server and a different string to the two-fields-disagree check below, so a
// string comparison would report a disagreement where there is none, or agreement
// between two spellings only one of which is real.
//
// Requiring the value to round-trip through ParseInt/FormatInt collapses every spelling
// to one, so the comparison compares campaigns rather than text. There is deliberately
// no TrimSpace: " 123 " is not a response this API produces, and trimming it would
// convert "this row is malformed" into "this row is campaign 123" — the exact
// substitution a fail-closed lookup exists to refuse.
func canonicalCampaignID(s string) string {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 || strconv.FormatInt(v, 10) != s {
		return ""
	}
	return s
}

// FindCampaignByName returns the numeric id of the single live campaign in this
// account whose name is exactly name.
//
// The contract mirrors the fail-closed LOGIC of the other clients' lookups (meta's
// findCampaignByName, linkedin's findMatch, twitter's and microsoft's) because callers
// make the same decision from the result. It is the first one EXPORTED, though: the
// others are called only from inside their own create path; this one is exported because
// dispatch's adoption path calls it (GoogleAdsDispatcher.Dispatch, LFXV2-3042) — and only
// when that dispatch set adoptExisting, so the caller is a deliberate act rather than
// every create. The outcomes:
//
//   - exactly one live match  -> (id, nil)
//   - no live match           -> ("", nil)   — a clean, trustworthy absence
//   - more than one match     -> ("", error) — AMBIGUOUS, never a silent pick
//   - anything unverifiable   -> ("", error)
//
// The distinction between the second and third cases is the whole point, and why this errors
// rather than taking the first hit. The production caller acts destructively on an absence:
// adopt-on-create takes ("", nil) as licence to fall through to CreateCampaign, so a false
// absence produces a SECOND paid campaign next to the one that was already there — the exact
// outcome adoption exists to prevent. (A future by-name adopt endpoint would read the same
// value as "nothing to adopt"; the cost differs, the fail-closed rule does not.) An arbitrary
// pick among same-name campaigns is the mirror fault: it binds a brief to the wrong one. Real spend either way. Two live campaigns sharing a name is
// ANOMALOUS, not routine — v23 rejects a mutate whose name another ENABLED/PAUSED campaign
// holds (DUPLICATE_CAMPAIGN_NAME) — so this branch fail-closes on a response that should not
// be possible, which is exactly when guessing is least defensible.
func (c *Client) FindCampaignByName(ctx context.Context, name string) (string, error) {
	if err := c.validateAccountIDs(); err != nil {
		return "", err
	}

	// The name is used VERBATIM — no TrimSpace. Trimming is a no-op for the create path
	// (composeName's output is already trimmed) and a silent contract change for adoption,
	// where "  foo  " would return the campaign named "foo" and hide the ambiguity if both
	// existed. TrimSpace below only DETECTS whitespace-only input.
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

	// The name filter is applied SERVER-side so a miss costs one page instead of a walk of
	// every campaign in the account, and REMOVED is excluded in the same clause so a long
	// tail of tombstones cannot page the query out. Both predicates are re-checked on the
	// rows below; see there for why. The enum is single-quoted to match the only other enum
	// predicate here (listManagerClients' `customer_client.status = 'ENABLED'`).
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

		// Re-checking both predicates client-side is what makes an escaping regression
		// LOUD: if gaqlStringLiteral ever stopped neutralising a quote, the injected
		// query would still return 2xx with rows for OTHER campaigns.
		//
		// A name mismatch is an ERROR, not a skip. Skipping would reduce an unhonoured
		// query to "no rows matched", i.e. ("", nil) — precisely the false absence this
		// method exists to prevent; a row that is not an exact match means the filter
		// did not take effect, which invalidates the WHOLE response. The comparison is
		// byte-exact, matching composeName, so a differently-normalised answer also
		// fails closed rather than silently binding.
		if row.Campaign.Name != lookup {
			return "", fmt.Errorf("google-ads campaign lookup: exact-match query for %q returned a campaign named %q; the name filter was not honoured, refusing to trust this response", lookup, row.Campaign.Name)
		}

		// Status, by contrast, IS a per-row skip when it is REMOVED — and only then. A
		// tombstone is unambiguously not adoptable no matter why it arrived, so dropping
		// it can only ever be correct, whereas a wrong NAME means the query itself failed.
		//
		// Every other status fails closed: Google can return UNSPECIFIED or UNKNOWN (an
		// omitted field decodes to ""), and treating one as live would return the id of a
		// campaign whose serving state we could not establish.
		switch row.Campaign.Status {
		case StatusRemoved:
			continue
		case StatusEnabled, StatusPaused:
			// Live: the only two states a real, adoptable campaign can be in.
		default:
			return "", fmt.Errorf("google-ads campaign lookup: campaign named %q has unrecognised status %q (want %s, %s or %s); refusing to treat it as live", lookup, row.Campaign.Status, StatusEnabled, StatusPaused, StatusRemoved)
		}

		// The resource name is validated WHENEVER it is present, not only as a fallback
		// for a missing campaign.id: both fields were selected, so both are evidence of
		// what this row IS. A malformed or cross-customer resource name beside a
		// plausible id means the row identifies no single campaign in THIS account, and
		// validating only in the fallback makes the check reachable exactly when the row
		// is least suspicious.
		// NOT trimmed — see canonicalCampaignID. The id is identity evidence, so a
		// padded value is a malformed row, not a value to normalise into a match.
		id := row.Campaign.ID
		fromName := c.campaignIDFromResourceName(row.Campaign.ResourceName)
		//
		// Tested RAW, not trimmed. TrimSpace here would fold a whitespace-only resource
		// name into "field absent" and let the row fall through to the id alone — so a
		// row carrying id "4242" beside resourceName "   " would be adopted as campaign
		// 4242 despite one of its two selected identity fields being malformed. Absent
		// and present-but-garbage are exactly the distinction this guard exists to make.
		if row.Campaign.ResourceName != "" && fromName == "" {
			return "", fmt.Errorf("google-ads campaign lookup: campaign named %q has resource name %q, which is malformed or scoped to another customer; refusing to adopt it", lookup, row.Campaign.ResourceName)
		}
		switch {
		case id == "":
			// v23 returns campaign.id as a string, but an int64 field can arrive absent.
			// The fallback validated the FULL shape above, not the trailing segment:
			// bare resourceID would read "garbage/4242" as campaign 4242.
			id = fromName
		case fromName != "" && fromName != id:
			return "", fmt.Errorf("google-ads campaign lookup: campaign named %q reports id %q but resource name %q; its two identity fields disagree, refusing to adopt it", lookup, id, row.Campaign.ResourceName)
		}
		// The fail-closed case linkedin's findMatch documents: the server says a campaign
		// with this name exists, but we cannot name it. Not reportable as an absence.
		if id == "" {
			return "", fmt.Errorf("google-ads campaign lookup: campaign named %q has no usable id (resourceName %q); aborting rather than report it absent", lookup, row.Campaign.ResourceName)
		}
		if canonicalCampaignID(id) == "" {
			// Two reasons, and both matter. The id is interpolated into resource paths
			// by every later call, so a non-numeric one must never leave this function.
			// And it is the answer to "which campaign is this", so "0", an out-of-range
			// value and a non-canonical spelling are all unusable even though they are
			// all digits.
			return "", fmt.Errorf("google-ads campaign lookup: campaign named %q returned id %q, which is not the canonical spelling of a positive int64 campaign id", lookup, id)
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
