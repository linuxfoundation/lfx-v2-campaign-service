// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// Ad-group EXCLUDED keywords: add (LFXV2-2665)
//
// WHERE A REDDIT NEGATIVE KEYWORD LIVES. In the same ad-group `targeting` object as the positive
// keywords, as the member `excluded_keywords`. Reddit's v3 OpenAPI document defines it as
// {"description": "The keywords to exclude from targeting.", "type": ["array","null"],
// "minItems": 0, "maxItems": 2000, "items": {"type": "string"}} — a plain string list, with NO
// match type and no id per entry (https://ads-api.reddit.com/api/v3/openapi.json, read from the
// SHA-256-pinned byte-for-byte copy in goncaloaguer/unofficial-reddit-ads-mcp,
// openapi/reddit-ads-v3.json, because ads-api.reddit.com refuses automated fetches; consulted
// 2026-10-13). The positive `keywords` member is capped at 1000 in the same schema.
//
// The write is the keyword removal's: one PATCH of the whole targeting object, every other member
// sent back exactly as read (keyword_targeting.go explains why), with only `excluded_keywords`
// changed — the elements already there kept byte for byte, the new strings appended.
// ---------------------------------------------------------------------------

// MaxExcludedKeywords is Reddit's maxItems for targeting.excluded_keywords.
const MaxExcludedKeywords = 2000

// excludedKeywordsMember is the targeting member that holds an ad group's negative keywords.
const excludedKeywordsMember = "excluded_keywords"

// ExcludedKeywords returns the ad group's excluded keywords as read, in Reddit's order; empty
// when the member is absent or null. A member that is not an array of strings is
// ErrTargetingUnreadable — decided only when asked, so the positive-keyword read is unaffected
// by a shape only the negative-keyword write depends on.
func (t *AdGroupTargeting) ExcludedKeywords() ([]string, error) {
	values, _, err := decodeStringArray(t.members[excludedKeywordsMember])
	return values, err
}

// ExcludedKeywordElements is the number of ELEMENTS in `excluded_keywords` as read, a null one
// included: a write keeps every element it read, so this — not the count of keywords — is what
// Reddit's maxItems is measured against.
func (t *AdGroupTargeting) ExcludedKeywordElements() (int, error) {
	_, elems, err := decodeStringArray(t.members[excludedKeywordsMember])
	return len(elems), err
}

// FingerprintExceptExcluded fingerprints every targeting member except `excluded_keywords`.
func (t *AdGroupTargeting) FingerprintExceptExcluded() (string, error) {
	return t.FingerprintExcept(excludedKeywordsMember)
}

// AddAdGroupExcludedKeywords writes base's targeting back with add appended to
// `excluded_keywords`, via PATCH /ad_accounts/{account}/ad_groups/{id} with
// {"data":{"targeting":{...}}}, and returns the excluded keywords the write leaves. Every other
// targeting member, and every excluded element already there (a null one included), is sent
// exactly as base read it. base MUST be the read of this same ad group the caller validated, and
// add must hold only keywords base does not already exclude — the caller decides ALREADY_PRESENT;
// an empty add, an add naming a keyword already excluded, or an array that would exceed
// MaxExcludedKeywords ELEMENTS (null elements kept from the read count) is refused before any
// request.
//
// The PATCH is sent ONCE and a 429 is NOT retried, for RemoveAdGroupKeywords' reason: the write
// replaces the whole targeting from a pre-read snapshot. A 429, transport failure, 3xx or 5xx is
// UNCONFIRMED (IsOutcomeUnconfirmed); any other 4xx is a definite refusal. A 2xx echo naming
// another ad group, or an excluded list other than the one written, is UNCONFIRMED.
func (c *Client) AddAdGroupExcludedKeywords(ctx context.Context, adGroupID string, base *AdGroupTargeting, add []string) ([]string, error) {
	path, adGroupID, err := c.adGroupBidPath(adGroupID)
	if err != nil {
		return nil, err
	}
	if base == nil || base.members == nil || base.AdGroupID != adGroupID {
		return nil, fmt.Errorf("reddit: add ad group %s excluded keywords: no targeting read of this ad group to write back: %w", adGroupID, ErrTargetingUnreadable)
	}
	existing, elems, err := decodeStringArray(base.members[excludedKeywordsMember])
	if err != nil {
		return nil, fmt.Errorf("reddit: add ad group %s excluded keywords: %w", adGroupID, err)
	}
	if len(add) == 0 {
		return nil, fmt.Errorf("reddit: add ad group %s excluded keywords: nothing to add", adGroupID)
	}
	present := make(map[string]bool, len(existing))
	for _, k := range existing {
		present[k] = true
	}
	for _, k := range add {
		if present[k] {
			return nil, fmt.Errorf("reddit: add ad group %s excluded keywords: a keyword to add is already excluded", adGroupID)
		}
		present[k] = true
	}
	// Counted in ELEMENTS, not keywords: the write keeps every element read, a null one included,
	// so the array Reddit receives is len(elems)+len(add) long.
	if n := len(elems) + len(add); n > MaxExcludedKeywords {
		return nil, fmt.Errorf("reddit: add ad group %s excluded keywords: %d would exceed Reddit's %d", adGroupID, n, MaxExcludedKeywords)
	}

	kept := make([]json.RawMessage, 0, len(elems)+len(add))
	kept = append(kept, elems...)
	for _, k := range add {
		e, eerr := encodeNoEscape(k)
		if eerr != nil {
			return nil, fmt.Errorf("reddit: add ad group %s excluded keywords: %w", adGroupID, eerr)
		}
		kept = append(kept, e)
	}
	written := append(append(make([]string, 0, len(existing)+len(add)), existing...), add...)
	member, err := encodeNoEscape(kept)
	if err != nil {
		return nil, fmt.Errorf("reddit: add ad group %s excluded keywords: %w", adGroupID, err)
	}
	targeting := make(map[string]json.RawMessage, len(base.members)+1)
	for k, v := range base.members {
		targeting[k] = v
	}
	targeting[excludedKeywordsMember] = member
	encoded, err := encodeNoEscape(map[string]any{"data": map[string]any{"targeting": targeting}})
	if err != nil {
		return nil, fmt.Errorf("reddit: add ad group %s excluded keywords: %w", adGroupID, err)
	}

	resp, err := c.requestNoThrottleRetry(ctx, http.MethodPatch, path, preEncodedBody(encoded))
	if err != nil {
		return nil, fmt.Errorf("reddit: add ad group %s excluded keywords: %w", adGroupID, err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return written, nil
	}
	var echo adGroupTargetingWire
	if jerr := json.Unmarshal(resp.Data, &echo); jerr != nil {
		return nil, &transportError{Method: http.MethodPatch, Path: "ad group targeting", Err: fmt.Errorf("decode 2xx targeting update response for ad group %s: not an ad group object", adGroupID)}
	}
	if got := strings.TrimSpace(echo.ID); got != "" && got != adGroupID {
		return nil, &transportError{Method: http.MethodPatch, Path: "ad group targeting", Err: fmt.Errorf("targeting update for ad group %s was acknowledged for ad group %s", adGroupID, got)}
	}
	if t := bytes.TrimSpace(echo.Targeting); len(t) > 0 && string(t) != "null" {
		members, _, _, derr := decodeTargeting(t)
		var got []string
		if derr == nil {
			got, _, derr = decodeStringArray(members[excludedKeywordsMember])
		}
		if derr != nil || !SameKeywords(got, written) {
			return nil, &transportError{Method: http.MethodPatch, Path: "ad group targeting", Err: fmt.Errorf("targeting update for ad group %s was acknowledged with an excluded keyword list other than the one sent", adGroupID)}
		}
	}
	return written, nil
}
