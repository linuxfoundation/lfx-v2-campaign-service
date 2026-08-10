// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
// dispatch's adoption path is INTENDED to call it — that wiring is a follow-up, and no
// production caller exists yet. The outcomes:
//
//   - exactly one live match  -> (id, nil)
//   - no live match           -> ("", nil)   — a clean, trustworthy absence
//   - more than one match     -> ("", error) — AMBIGUOUS, never a silent pick
//   - anything unverifiable   -> ("", error)
//
// The distinction between the second and third cases is the whole point, and why this errors
// rather than taking the first hit. Both callers act destructively on an absence: create takes
// ("", nil) as licence to create, adoption as licence to report nothing to adopt — so a false
// absence produces a duplicate paid campaign, and an arbitrary pick among same-name campaigns
// binds a brief to the wrong one. Real spend either way. Two live campaigns sharing a name is
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
		// The same raw-bytes guard GetCampaign applies, and for a reason that is easy to
		// argue away: the echo-check below looks like it already covers this, since a
		// substituted U+FFFD would make the decoded name differ from `lookup` and error.
		// It covers it for every name EXCEPT one that already contains U+FFFD — and that
		// is a legal campaign name. Ask for `bad�name`, receive a row whose raw bytes
		// are `"bad\xffname"` (or `"bad\uD800name"`), and encoding/json substitutes the
		// very rune that was asked for: the echo-check passes on a value it never saw
		// intact, and an ID is returned from a response nothing verified. That an adoption
		// binds a paid campaign to that ID is what makes the narrow case worth closing.
		//
		// Checked on the raw bytes, not by hunting U+FFFD in the decoded value, precisely
		// because U+FFFD is legal in a name. See hasUnpairedSurrogateEscape for why byte
		// validity alone is not enough.
		if !utf8.Valid(raw) || hasUnpairedSurrogateEscape(raw) {
			return "", fmt.Errorf("google-ads campaign lookup: exact-match query for %q returned a row whose name cannot survive JSON decoding intact (malformed UTF-8 bytes, or an unpaired surrogate escape); decoding it would substitute U+FFFD and could echo back the requested name from a value that never matched it", lookup)
		}
		// A duplicate key inside the row is a self-disagreement the decoder resolves
		// silently in favour of the last value. See hasDuplicateKeys.
		if hasDuplicateKeys(raw) {
			return "", fmt.Errorf("google-ads campaign lookup: exact-match query for %q returned a row that declares the same JSON key twice; the row identifies more than one campaign and no reading of it can be trusted", lookup)
		}
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

		id, live, err := c.campaignRowIdentity(row, fmt.Sprintf("campaign named %q", lookup))
		if err != nil {
			return "", err
		}
		if !live {
			continue
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

// campaignRowIdentity answers "which campaign is this row, and is it adoptable" for ONE
// row of a campaign lookup, and is the single place that question is answered.
//
// It exists because there are now two ways to reach a campaign — by name and by id — and
// they must agree about what counts as a trustworthy row. Duplicating the checks would let
// the by-id path become the lenient one, which is the worse direction to drift: a caller
// that hands over an id is binding a brief to a campaign it has already named, so a row
// this function accepts is one about to have real spend attached to it. `describe` is the
// caller's phrase for the campaign ("campaign named %q", "campaign id %s"), used only to
// keep the error messages readable.
//
// live is false ONLY for a REMOVED campaign, the one state a caller may skip — but the
// verdict is safe to act on only BECAUSE identity is established first. "A tombstone is
// unadoptable however it arrived" is a claim about a campaign, so it means nothing until
// the row has said which campaign it is; a malformed, cross-customer or self-disagreeing
// row is rejected before its status is ever read. Every other unrecognised status is an
// ERROR, not a skip — Google can return
// UNSPECIFIED or UNKNOWN, and an omitted field decodes to "", so treating one as live
// returns the id of a campaign whose serving state was never established, while treating it
// as a skip reduces an unverifiable response to a clean absence and licenses a create.
func (c *Client) campaignRowIdentity(row campaignLookupRow, describe string) (id string, live bool, err error) {
	// IDENTITY IS ESTABLISHED BEFORE STATUS, and the order carries weight.
	//
	// "A tombstone is unadoptable however it arrived, so dropping it can only ever be
	// correct" is true only once we know WHICH campaign the tombstone is for. Judging
	// status first grants that premise without earning it: a REMOVED row whose resource
	// name is malformed or belongs to another customer would return a clean not-live
	// verdict, and the by-id caller would report a campaign it never identified as
	// absent — the licence-to-create value, handed out on evidence this function exists
	// to reject. Establishing identity first means a row must say who it is before its
	// status is allowed to mean anything.
	//
	// The resource name is validated WHENEVER it is present, not only as a fallback for a
	// missing campaign.id: both fields were selected, so both are evidence of what this row
	// IS. A malformed or cross-customer resource name beside a plausible id means the row
	// identifies no single campaign in THIS account, and validating only in the fallback
	// makes the check reachable exactly when the row is least suspicious.
	//
	// NOT trimmed — see canonicalCampaignID. The id is identity evidence, so a padded value
	// is a malformed row, not a value to normalise into a match. Same for the resource name:
	// TrimSpace here would fold a whitespace-only one into "field absent" and let the row
	// fall through to the id alone, so a row carrying id "4242" beside resourceName "   "
	// would be adopted as campaign 4242 despite one of its two identity fields being
	// malformed. Absent and present-but-garbage are exactly the distinction this makes.
	id = row.Campaign.ID
	fromName := c.campaignIDFromResourceName(row.Campaign.ResourceName)
	if row.Campaign.ResourceName != "" && fromName == "" {
		return "", false, fmt.Errorf("google-ads campaign lookup: %s has resource name %q, which is malformed or scoped to another customer; refusing to adopt it", describe, row.Campaign.ResourceName)
	}
	switch {
	case id == "":
		// v23 returns campaign.id as a string, but an int64 field can arrive absent. The
		// fallback validated the FULL shape above, not the trailing segment: bare
		// resourceID would read "garbage/4242" as campaign 4242.
		id = fromName
	case fromName != "" && fromName != id:
		return "", false, fmt.Errorf("google-ads campaign lookup: %s reports id %q but resource name %q; its two identity fields disagree, refusing to adopt it", describe, id, row.Campaign.ResourceName)
	}
	// The fail-closed case linkedin's findMatch documents: the server says the campaign
	// exists, but we cannot name it. Not reportable as an absence.
	if id == "" {
		return "", false, fmt.Errorf("google-ads campaign lookup: %s has no usable id (resourceName %q); aborting rather than report it absent", describe, row.Campaign.ResourceName)
	}
	if canonicalCampaignID(id) == "" {
		// Two reasons, and both matter. The id is interpolated into resource paths by every
		// later call, so a non-numeric one must never leave this function. And it is the
		// answer to "which campaign is this", so "0", an out-of-range value and a
		// non-canonical spelling are all unusable even though they are all digits.
		return "", false, fmt.Errorf("google-ads campaign lookup: %s returned id %q, which is not the canonical spelling of a positive int64 campaign id", describe, id)
	}

	// Only now, with the row's identity established, may its status be read. The id is
	// returned for a REMOVED row too, so a caller can tell "the campaign you asked about is
	// a tombstone" from "some other campaign's tombstone came back", which are the same
	// value to a caller handed only `live`.
	switch row.Campaign.Status {
	case StatusRemoved:
		return id, false, nil
	case StatusEnabled, StatusPaused:
		// Live: the only two states a real, adoptable campaign can be in.
	default:
		return "", false, fmt.Errorf("google-ads campaign lookup: %s has unrecognised status %q (want %s, %s or %s); refusing to treat it as live", describe, row.Campaign.Status, StatusEnabled, StatusPaused, StatusRemoved)
	}
	return id, true, nil
}

// CampaignRef is what a caller learns about a campaign it is considering binding a brief
// to. The name and status are here because the decision is a human one: an operator who
// supplied an id is shown the campaign that id resolves to, in this account, before any
// binding is written — which is the whole point of verifying before binding rather than
// storing the id and discovering at dispatch time that it names something else.
type CampaignRef struct {
	ID     string
	Name   string
	Status string // StatusEnabled or StatusPaused — a live campaign is never anything else here
}

// ErrNotACampaignID reports that the caller's id could not name a campaign at all, so no
// query was issued. It is a PERMANENT input fault, distinct from every unreachable-platform
// error: the adopt handler maps it to 400 rather than telling the caller to retry forever.
var ErrNotACampaignID = errors.New("google-ads: not a campaign id")

// GetCampaign returns the live campaign with this id in this account, or (nil, nil) when
// no such campaign exists.
//
// It is the by-id counterpart of FindCampaignByName and carries the SAME fail-closed
// contract, because callers make the same decision from the result:
//
//   - one live campaign     -> (ref, nil)
//   - no live campaign      -> (nil, nil)   — a clean, trustworthy absence
//   - anything unverifiable -> (nil, error)
//
// A REMOVED campaign reads as an absence, exactly as it does by name: the id names a real
// record, but not one a brief can be bound to, and "you cannot adopt this" is what the
// caller needs to hear either way.
//
// Note what this does NOT do: it does not tell the caller whether the campaign is already
// bound to some other brief. That is this service's own state, not Google's, and answering
// it from here would be answering a database question with an ad-platform call.
func (c *Client) GetCampaign(ctx context.Context, campaignID string) (*CampaignRef, error) {
	if err := c.validateAccountIDs(); err != nil {
		return nil, err
	}
	// The id is validated before it is interpolated, and validated as an IDENTITY rather
	// than merely as safe text. canonicalCampaignID rejects "0", a value past
	// math.MaxInt64 and the leading-zero spelling "007" — all of which are digits, none of
	// which names a campaign this client can adopt. Rejecting here rather than querying is
	// not just an optimisation: "007" would match campaign 7 server-side and then fail the
	// echo check below as a disagreement, reporting a confusing conflict for what is really
	// a malformed request.
	if canonicalCampaignID(campaignID) == "" {
		return nil, fmt.Errorf("%w: %q (want the canonical base-10 spelling of a positive int64)", ErrNotACampaignID, campaignID)
	}

	// campaign.id is an int64 in GAQL, so it is compared UNQUOTED — quoting it would make
	// this a string comparison against a numeric field. No escaping question arises: the
	// value has already been proven to be nothing but digits.
	query := "SELECT campaign.id, campaign.name, campaign.status, campaign.resource_name " +
		"FROM campaign WHERE campaign.id = " + campaignID +
		" AND campaign.status != '" + StatusRemoved + "'"

	rows, err := c.gaqlSearch(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("google-ads campaign lookup: %w", err)
	}

	describe := fmt.Sprintf("campaign id %s", campaignID)
	var found *CampaignRef
	var removedRef *CampaignRef
	for _, raw := range rows {
		// A JSON document must be UTF-8 (RFC 8259 s8.1), and encoding/json does NOT enforce
		// it: malformed bytes inside a string are silently replaced with U+FFFD and no error
		// is returned. FindCampaignByName runs the same guard for a narrower reason: its
		// echo-check against the requested name turns most substitutions into a loud
		// filter-not-honoured error, but not when the requested name itself contains
		// U+FFFD. This path has no expected name to compare against at all — the name IS
		// the answer the operator confirms against — so a substituted name would be
		// returned as a successful CampaignRef under a name the campaign does not have.
		// Checked on the RAW bytes rather than by hunting U+FFFD in the decoded value,
		// because U+FFFD is itself a legal character in a campaign name.
		//
		// Two ways in, and byte validity catches only the first. `"bad\xffname"` is a
		// malformed BYTE, which utf8.Valid sees. `"bad\uD800name"` is not: every byte of
		// that document is ASCII, so the bytes are perfectly valid UTF-8, and the escape
		// only becomes a problem when encoding/json resolves it — an unpaired surrogate is
		// not a Unicode scalar, so it is substituted with U+FFFD, again with no error. That
		// route lands in exactly the value the guard below deliberately admits, and would
		// have produced the silently rewritten name this check exists to prevent.
		if !utf8.Valid(raw) || hasUnpairedSurrogateEscape(raw) {
			return nil, fmt.Errorf("google-ads campaign lookup: campaign id %s was returned in a row whose name cannot survive JSON decoding intact (malformed UTF-8 bytes, or an unpaired surrogate escape); decoding it would substitute U+FFFD and offer a name this campaign does not have", campaignID)
		}
		// A duplicate key inside the row is a self-disagreement the decoder resolves
		// silently in favour of the last value. See hasDuplicateKeys.
		if hasDuplicateKeys(raw) {
			return nil, fmt.Errorf("google-ads campaign lookup: a result row declares the same JSON key twice; the row identifies more than one campaign and no reading of it can be trusted")
		}
		var row campaignLookupRow
		if err := json.Unmarshal(raw, &row); err != nil {
			// A 2xx row we cannot decode is NOT a non-match; reporting it as one would
			// tell the caller this campaign does not exist when it may.
			return nil, fmt.Errorf("google-ads campaign lookup: decoding result row: %w", err)
		}

		// The id filter is re-checked client-side for the same reason the name filter is
		// in FindCampaignByName: it makes a filter regression LOUD instead of silent. A row
		// for a DIFFERENT campaign means the WHERE clause was not honoured, which
		// invalidates the whole response — so this errors on the response rather than
		// skipping the row, because skipping would reduce an unhonoured query to "no rows
		// matched", i.e. the clean absence a caller is entitled to trust.
		//
		// It runs below campaignRowIdentity — which supplies the id — but ABOVE the
		// tombstone skip, and that second position is the load-bearing one. A check placed
		// after a `continue` on the not-live verdict never runs on a REMOVED row, so a
		// REMOVED row for a DIFFERENT campaign would leave through the skip untested and a
		// response made only of such rows would return (nil, nil): a response honouring
		// NEITHER predicate, since the query names one id AND excludes REMOVED, reported as
		// the trustworthy absence a caller acts on by creating a second campaign against the
		// same budget. campaignRowIdentity establishing identity before status is what makes
		// this placement possible — `id` is populated for a tombstone too, so the filter can
		// be checked on every row rather than only the live ones.
		id, live, err := c.campaignRowIdentity(row, describe)
		if err != nil {
			return nil, err
		}
		if id != campaignID {
			return nil, fmt.Errorf("google-ads campaign lookup: query for campaign id %s returned campaign %s; the id filter was not honoured, refusing to trust this response", campaignID, id)
		}

		// A tombstone for the campaign actually asked about is the one case a skip can only
		// be right: the id names a real record, but not one a brief can be bound to. That is
		// the verdict FindCampaignByName reaches for the same row, and it reads as an absence
		// here for the same reason.
		if !live {
			// Tombstones are held to the same agreement rule as live rows, for the same
			// reason: a response that answers one id with two different campaigns has
			// contradicted itself, and an absence derived from a self-contradicting
			// response is not the trustworthy absence a caller may act on by creating.
			// Identical duplicates stay tolerated — GAQL can return one campaign on
			// several rows — and the name is compared unvalidated here on purpose: a
			// tombstone's name is never surfaced, so an empty one is not a defect, but
			// TWO names for one id still is.
			ref := &CampaignRef{ID: id, Name: row.Campaign.Name, Status: row.Campaign.Status}
			if removedRef != nil && *removedRef != *ref {
				// Quoted field by field rather than %+v'd as a struct, matching how
				// metrics.go renders upstream values. Both refs carry the id already
				// checked against campaignID, so only the name and status can differ —
				// and the name here is the one value in this function that reaches an
				// error UNVALIDATED, since a tombstone deliberately skips
				// returnedCampaignName. A raw one could carry newlines or terminal
				// controls straight into a log line.
				return nil, fmt.Errorf("google-ads campaign lookup: campaign id %s was returned as %s twice with different details (name %q status %q vs name %q status %q); refusing to trust this response", campaignID, StatusRemoved, removedRef.Name, removedRef.Status, ref.Name, ref.Status)
			}
			removedRef = ref
			continue
		}

		// The name is not decoration. An operator confirms a binding by reading the name
		// this call returns, so a row that cannot supply one cannot be confirmed, and
		// Campaign.name is a required field Google populates for every campaign — an empty
		// one in a response that SELECTed it is a truncated answer, not a nameless campaign.
		// Failing here costs a retry; returning it would bind real spend to a campaign
		// nobody could identify.
		if err := returnedCampaignName(row.Campaign.Name, campaignID); err != nil {
			return nil, err
		}

		// Duplicate rows for the same campaign are tolerated (GAQL can return one campaign
		// on several rows when a query joins a repeated resource) as long as they agree —
		// and they must agree on the NAME too, not only the id, since the name is what an
		// operator will read before confirming the binding.
		ref := &CampaignRef{ID: id, Name: row.Campaign.Name, Status: row.Campaign.Status}
		if found != nil && *found != *ref {
			// Quoted for the same reason as the tombstone case above. These names DID pass
			// returnedCampaignName, which rejects what Campaign.name forbids — NUL, LF and
			// CR — and deliberately permits everything else, TAB and the Cf category
			// included. Permitted is not printable: quoting is what keeps a legal name from
			// controlling the rendered error.
			return nil, fmt.Errorf("google-ads campaign lookup: campaign id %s was returned twice with different details (name %q status %q vs name %q status %q); refusing to trust this response", campaignID, found.Name, found.Status, ref.Name, ref.Status)
		}
		found = ref
	}

	// One campaign cannot be live and removed at once, so a response asserting both has
	// contradicted itself and none of it is trustworthy — least of all the live row, which
	// is the one a caller would bind real spend to. Checked after the loop rather than on
	// sight because the rows arrive in no guaranteed order, and a mixture must fail the
	// same way whichever row came first; a leading live row cannot buy trust for what
	// follows it.
	//
	// A response of tombstones ALONE is a different thing and stays an absence: that is the
	// campaign asked about, reported as unadoptable, which is what a caller needs to hear.
	if removedRef != nil && found != nil {
		return nil, fmt.Errorf("google-ads campaign lookup: campaign id %s was returned both live and %s in one response; refusing to trust this response", campaignID, StatusRemoved)
	}
	return found, nil
}

// returnedCampaignName checks that a name Google returned is a name Campaign.name can
// actually hold. It is the RESPONSE-side counterpart to the input validation the by-name
// lookup does, and the disposition is deliberately different: an unusable name errors on
// the whole response rather than being skipped, because a skip would reduce it to zero
// matches — (nil, nil), the clean absence a caller acts on by CREATING a second campaign
// against the same budget. Erroring costs a retry.
//
// What it rejects is exactly what the upstream field forbids, and nothing more. Over-
// rejection here is not the conservative choice it looks like: adoption targets records
// this service never created and never sanitized, so refusing a legal-but-unusual name
// answers "cannot trust this" about a campaign sitting right there. Google Ads v23
// prohibits NUL, LF and CR in Campaign.name and caps it at maxCampaignNameRunes
// CHARACTERS; TAB, U+2028/U+2029 and format characters (ZWJ, variation selectors) are all
// legal, which is why this is an explicit three-rune check and not unicode.IsControl.
//
// A value outside those bounds is not an unusual campaign name — it is data Google could
// not have stored, so the response carrying it has already gone wrong somewhere.
func returnedCampaignName(name, campaignID string) error {
	// Blank first, and by TrimSpace: Campaign.name is required and populated for every
	// campaign, so an empty (or whitespace-only) one in a response that SELECTed it is a
	// truncated answer rather than a nameless campaign. The name is not decoration — an
	// operator confirms a binding by reading it, so a ref without one asks for a
	// confirmation that cannot be given.
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("google-ads campaign lookup: campaign id %s was returned with no usable name (%q); refusing to offer a campaign an operator cannot confirm", campaignID, name)
	}
	if n := utf8.RuneCountInString(name); n > maxCampaignNameRunes {
		return fmt.Errorf("google-ads campaign lookup: campaign id %s was returned with a %d-character name, past the %d Campaign.name allows; refusing to trust this response", campaignID, n, maxCampaignNameRunes)
	}
	if strings.ContainsAny(name, "\x00\n\r") {
		return fmt.Errorf("google-ads campaign lookup: campaign id %s was returned with a name containing NUL, LF or CR, which Campaign.name cannot hold; refusing to trust this response", campaignID)
	}
	return nil
}

// hasDuplicateKeys reports whether any JSON object in b declares the same key twice, or
// twice in two spellings encoding/json resolves to the SAME struct field.
//
// encoding/json does not treat a repeated key as an error: it decodes both and the LAST
// one wins. For an ordinary payload that is a harmless quirk of RFC 8259, which leaves
// duplicate-key behaviour undefined. Here it is a false-identity hole. A row reading
// `{"campaign":{"id":"999","id":"555","resourceName":"customers/1/campaigns/555"}}`
// decodes as campaign 555, agrees with its own resource name, passes the id filter, and
// is returned as a confirmed lookup — while the same bytes also identify campaign 999,
// and a reader applying the first-wins convention gets the other answer. Every other
// identity guard in this file exists to refuse a row that disagrees with itself; a
// duplicate key is that disagreement expressed in a form the decoder silently resolves.
//
// The whole row is walked, not just the fields this client reads. A duplicate anywhere is
// evidence the producer or an intermediary is not emitting what we think it is, and the
// selected-field set changes over time — a guard scoped to today's fields would quietly
// stop covering tomorrow's.
//
// Keys are compared under the decoder's own notion of sameness, not byte equality, because
// byte equality is not what decides which value lands in the struct. encoding/json prefers
// an exact tag match and otherwise falls back to a CASE-INSENSITIVE one, so
// `{"id":"999","ID":"555"}` assigns the field twice and leaves 555 — two contradictory ids,
// no repeated key, and a row that agrees with resource name 555 while also saying 999. A
// guard comparing spellings would miss exactly the collision the decoder acts on. foldKey
// reproduces that fold, including the two non-ASCII cases the decoder special-cases. Folding
// cannot over-reject here: Google's JSON is lowerCamelCase throughout, so no legitimate
// object carries two keys that differ only in case.
//
// Malformed JSON returns false: this runs BEFORE Unmarshal, whose error is the better
// diagnostic for that case, and reporting a parse failure as a duplicate key would send
// an operator looking for the wrong thing.
func hasDuplicateKeys(b []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	// Each element is the set of keys seen so far in one still-open object; nil marks an
	// open ARRAY, which has no keys of its own but must occupy a stack slot so the
	// matching delimiter pops the right frame.
	var stack []map[string]struct{}
	expectKey := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return false // EOF or malformed; Unmarshal reports the latter properly.
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, map[string]struct{}{})
				expectKey = true
			case '[':
				stack = append(stack, nil)
				expectKey = false
			case '}', ']':
				if len(stack) == 0 {
					return false
				}
				stack = stack[:len(stack)-1]
				expectKey = len(stack) > 0 && stack[len(stack)-1] != nil
			}
		case string:
			if expectKey {
				seen := stack[len(stack)-1]
				k := foldKey(t)
				if _, dup := seen[k]; dup {
					return true
				}
				seen[k] = struct{}{}
				expectKey = false
				continue
			}
			expectKey = len(stack) > 0 && stack[len(stack)-1] != nil
		default:
			expectKey = len(stack) > 0 && stack[len(stack)-1] != nil
		}
	}
}

// foldKey maps a JSON object key onto the form encoding/json compares field names by, so two
// spellings the decoder would resolve to one field also collide here.
//
// The decoder's fallback match is a case-insensitive comparison, and its fold is not plain
// lower-casing: it also equates KELVIN SIGN (U+212A) with 'k' and LATIN SMALL LETTER LONG S
// (U+017F) with 's', because both simple-fold to the ASCII letter. `{"id":…,"Kid":…}` is
// not a collision and `{"key":…,"Key":…}` is; leaving the two runes out would let a
// producer spell a second copy of a field in a way the decoder honours and this guard does
// not. Everything else is ordinary Unicode lower-casing, which is a superset of what the
// decoder folds — stricter, and safe here for the reason given at hasDuplicateKeys.
func foldKey(k string) string {
	return strings.ToLower(strings.Map(func(r rune) rune {
		switch r {
		case 'K': // KELVIN SIGN
			return 'k'
		case 'ſ': // LATIN SMALL LETTER LONG S
			return 's'
		}
		return r
	}, k))
}

// hasUnpairedSurrogateEscape reports whether b contains a \uD800-\uDFFF JSON escape that is not
// part of a valid high+low pair.
//
// utf8.Valid answers a question about BYTES, and a `\uD800` escape is six ASCII bytes — valid
// UTF-8 by every measure, and still not a name any campaign can have. encoding/json resolves it
// to U+FFFD, silently and without error, because an unpaired surrogate is not a Unicode scalar
// value. On a path whose whole purpose is to hand an operator a name to confirm a binding
// against, that substitution is the failure: the name offered is not the campaign's.
//
// Scanning the raw row rather than the decoded string is what makes the two cases separable. A
// campaign whose name genuinely contains U+FFFD arrives as the encoded bytes EF BF BD or as a
// `\ufffd` escape, neither of which this matches; a substituted one arrives as a surrogate
// escape, which nothing else can produce. Rejecting costs no reachable lookup — Google Ads
// cannot store an unpaired surrogate, so a row carrying one has already gone wrong upstream.
func hasUnpairedSurrogateEscape(b []byte) bool {
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' {
			continue
		}
		// A doubled backslash is literal data: it consumes the next byte, and neither of the
		// two begins an escape. Without this, `\\uD800` — a backslash followed by the TEXT
		// uD800 — would be read as a surrogate escape it is not.
		if i+1 < len(b) && b[i+1] == '\\' {
			i++
			continue
		}
		if i+5 >= len(b) || b[i+1] != 'u' {
			continue
		}
		r, ok := hex4(b[i+2 : i+6])
		if !ok {
			// Malformed hex is not this function's finding: json.Unmarshal errors on it,
			// and the caller turns that into a refusal of its own.
			continue
		}
		switch {
		case r >= 0xD800 && r <= 0xDBFF:
			// A high surrogate is legal only when a low one follows IMMEDIATELY.
			if i+11 >= len(b) || b[i+6] != '\\' || b[i+7] != 'u' {
				return true
			}
			lo, ok := hex4(b[i+8 : i+12])
			if !ok || lo < 0xDC00 || lo > 0xDFFF {
				return true
			}
			i += 11
		case r >= 0xDC00 && r <= 0xDFFF:
			return true // a low surrogate with no high one before it
		default:
			i += 5
		}
	}
	return false
}

// hex4 parses exactly four hex digits, the fixed width of a JSON \u escape.
func hex4(b []byte) (uint32, bool) {
	if len(b) != 4 {
		return 0, false
	}
	var v uint32
	for _, c := range b {
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | uint32(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | uint32(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | uint32(c-'A'+10)
		default:
			return 0, false
		}
	}
	return v, true
}
