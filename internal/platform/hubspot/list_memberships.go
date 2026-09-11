// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// List membership + legacy list names (LFXV2-2770)
//
// Two reads the audience builder needs and the rest of this client does not.
//
// Memberships answer "how many DISTINCT people would this reach" — the sum of
// several lists' sizes over-counts by every contact on more than one of them, and
// overlap between an event's registrant and speaker lists is the normal case, not
// the exception. Only the ids are collected: computing a union needs identity, and
// pulling contact properties for tens of thousands of records to count them would
// move real personal data through this service for no reason.
//
// Legacy names exist because a list built years ago can still be REFERENCED by a
// current list's filters while being invisible to the v3 API. QA reads the names of
// everything a list excludes to decide whether suppression is applied, so a name it
// cannot read is a suppression it cannot credit.
// ---------------------------------------------------------------------------

const legacyListPath = "/contacts/v1/lists"

// membershipPageSize is how many records each membership page requests. 250 is the
// endpoint's documented maximum; a smaller page only multiplies round-trips.
const membershipPageSize = 250

// membershipMaxPages bounds the walk at 100 pages — 25,000 records, matching the
// audience builder's exact-count cap. Past that the union is not computed at all, so
// there is nothing to fetch the pages for.
const membershipMaxPages = 100

// ListMembershipIDs returns the record ids on a list, and whether the walk stopped
// at its bound before reaching the end.
//
// `truncated` is the important half of the return, and callers must not discard it. A
// truncated membership makes a UNION an UNDER-count, and understating how many people
// an email reaches is the one error direction that must never be presented as exact.
// Read-only (idempotent).
func (c *Client) ListMembershipIDs(ctx context.Context, listID string) (ids []string, truncated bool, err error) {
	if listID = strings.TrimSpace(listID); listID == "" {
		return nil, false, fmt.Errorf("hubspot: ListMembershipIDs requires a non-empty list id")
	}

	out := make([]string, 0, membershipPageSize)
	after := ""
	for page := 0; page < membershipMaxPages; page++ {
		q := url.Values{}
		q.Set("limit", strconv.Itoa(membershipPageSize))
		if after != "" {
			q.Set("after", after)
		}
		path := listsPath + "/" + url.PathEscape(listID) + "/memberships?" + q.Encode()
		raw, reqErr := c.doRequest(ctx, http.MethodGet, path, nil, true)
		if reqErr != nil {
			return nil, false, fmt.Errorf("hubspot: list %s memberships: %w", listID, reqErr)
		}
		var resp struct {
			Results []struct {
				RecordID json.Number `json:"recordId"`
			} `json:"results"`
			Paging *paging `json:"paging"`
		}
		if jsonErr := json.Unmarshal(raw, &resp); jsonErr != nil {
			return nil, false, fmt.Errorf("hubspot: decode list %s memberships: %w", listID, jsonErr)
		}
		// A malformed 2xx body (`{}` or `null`) decodes with Results==nil, while an
		// empty list returns `{"results":[]}`. Treating nil as "the end" on a LATER
		// page would silently return a short membership as if it were complete —
		// which is exactly the under-count this function's `truncated` flag exists to
		// prevent, arriving with the flag unset.
		if resp.Results == nil {
			return nil, false, fmt.Errorf("hubspot: list %s memberships returned a 2xx with no results array (malformed response)", listID)
		}
		for _, r := range resp.Results {
			if id := strings.TrimSpace(r.RecordID.String()); id != "" {
				out = append(out, id)
			}
		}
		if resp.Paging == nil || resp.Paging.Next == nil || strings.TrimSpace(resp.Paging.Next.After) == "" {
			return out, false, nil
		}
		next := strings.TrimSpace(resp.Paging.Next.After)
		if next == after {
			// A cursor that does not advance would loop until the page bound and
			// report the same records repeatedly. Reported as truncated rather than
			// as an error: the records already collected are real, and the caller's
			// degraded path is the honest answer.
			return out, true, nil
		}
		after = next
	}
	return out, true, nil
}

// LegacyListName returns the display name of a list from the legacy v1 API, or ""
// when it does not exist there.
//
// A 404 is a NORMAL answer, not a failure: lists created through v3 are simply absent
// from v1. So is any other error — this is a best-effort name lookup used to enrich a
// QA report, and failing the whole report because one referenced list could not be
// named would replace a partial answer with none. The caller distinguishes "" from a
// name; it never sees why.
func (c *Client) LegacyListName(ctx context.Context, listID string) string {
	if listID = strings.TrimSpace(listID); listID == "" {
		return ""
	}
	raw, err := c.doRequest(ctx, http.MethodGet, legacyListPath+"/"+url.PathEscape(listID), nil, true)
	if err != nil {
		return ""
	}
	var resp struct {
		Name string `json:"name"`
		List *struct {
			Name string `json:"name"`
		} `json:"list"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ""
	}
	if resp.List != nil && strings.TrimSpace(resp.List.Name) != "" {
		return strings.TrimSpace(resp.List.Name)
	}
	return strings.TrimSpace(resp.Name)
}

// IsNotFound reports whether err is a definite 404 from HubSpot.
//
// Separate from IsDefiniteRejection because the two mean different things to a
// caller resolving a list id an operator typed or a filter referenced: a 404 means
// "no such list", which is an answer to show them, while any other 4xx means the
// request itself was wrong and the list's existence is still unknown.
func IsNotFound(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.StatusCode == http.StatusNotFound
}
