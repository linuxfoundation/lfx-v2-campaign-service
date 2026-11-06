// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

// Shared negative keyword lists (#477).
//
// The live KubeCon NA 2026 Search campaign has three shared negative keyword lists attached
// (16, 177 and 5 keywords). A shared list is a SharedSet of type NEGATIVE_KEYWORDS that lives
// in the account and is attached to a campaign by a CampaignSharedSet.
//
// The shape is the smallest #477 asks for: ATTACH an EXISTING list by resource name. This
// client never creates, edits or removes a shared set, and never adds a keyword to one. A list
// is maintained once in the account and attached to many campaigns; creating one per campaign
// would duplicate the 177-keyword list for every campaign and drift from the original.
//
// Each name is checked for shape AND for this account before the budget mutate. Google refuses
// a shared set under another customer id with INVALID_CUSTOMER_ID, but (the 2026-11-03 probe
// showed) only at the attach, after the budget and campaign exist — so it is refused here.
//
// Verified with validateOnly on 2026-11-03 (v23, recorded on #477): a campaignSharedSet naming
// an existing NEGATIVE_KEYWORDS shared set was accepted, and one naming another customer's set
// was refused. NOT LIVE-VERIFIED: the exact request below — campaignSharedSets:mutate carrying
// several operations — and what Google does with a shared set of a different type (for example
// NEGATIVE_PLACEMENTS) on a Search campaign. A resource name says nothing about its set's type,
// and checking it would need a read this preflight does not make.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// maxSharedNegativeKeywordLists bounds the list. It is a payload bound, not a probed Google
// limit: the live reference campaign attaches three.
const maxSharedNegativeKeywordLists = 20

// sharedSetResourceRE is the one accepted spelling: customers/<cid>/sharedSets/<id>. The regex
// is the SHAPE only. The set id must also be canonicalCampaignID's positive int64 in canonical
// form (no leading zero, no overflow), because it is compared as a string against the one
// Google echoes in the link's resource name, and an id past int64 is no set Google has; the
// customer id must equal the campaign's account exactly, as display_targeting.go's lists do.
var sharedSetResourceRE = regexp.MustCompile(`^customers/(\d+)/sharedSets/(\d+)$`)

// validateSharedNegativeKeywordLists checks each shared set resource name and returns them in
// order, de-duplicated. SEARCH ONLY: refused on every other channel, because only the Search
// cascade attaches them. The cap counts the DISTINCT lists, after trimming and de-duplication,
// because that is the number of attach operations sent. It is checked INSIDE the walk, as
// validateCreativeText's count is: the walk stops at the first distinct list past the cap, so
// an oversized input is never trimmed, matched and hashed whole, and out and seen are sized
// from the cap rather than from the caller's length.
func validateSharedNegativeKeywordLists(kind, customerID string, lists []string) ([]string, error) {
	if len(lists) == 0 {
		return nil, nil
	}
	if kind != campaignKindSearch {
		return nil, fmt.Errorf("google-ads: shared negative keyword lists are not supported on %s "+
			"(this client attaches them only on the Search cascade); omit "+
			"SharedNegativeKeywordLists, or create a Search campaign", kind)
	}
	n := min(len(lists), maxSharedNegativeKeywordLists+1)
	out := make([]string, 0, n)
	seen := make(map[string]struct{}, n)
	for i, raw := range lists {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, fmt.Errorf("google-ads: shared negative keyword list %d is empty", i+1)
		}
		m := sharedSetResourceRE.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("google-ads: shared negative keyword list %q is not a "+
				"customers/<id>/sharedSets/<id> resource name", capForError(name))
		}
		// Refused here, not left to Google: an id past int64 or 0 names no set, and a leading
		// zero may be echoed under another spelling, which the link check treats as
		// UNCONFIRMED; either way it surfaces only after the budget.
		if canonicalCampaignID(m[2]) == "" {
			return nil, fmt.Errorf("google-ads: shared negative keyword list %q is not a "+
				"customers/<id>/sharedSets/<id> resource name with a positive canonical id "+
				"(base-10, no leading zero, within int64); write it as Google returns it",
				capForError(name))
		}
		// Refused here, not left to Google: Google refuses another customer's set only at the
		// attach (INVALID_CUSTOMER_ID), after the budget and campaign exist.
		if m[1] != customerID {
			return nil, fmt.Errorf("google-ads: shared negative keyword list %q belongs to "+
				"customer %q, not the campaign's account %s; a shared set cannot be attached "+
				"across accounts", capForError(name), m[1], customerID)
		}
		// De-duplicated rather than refused, as conversion actions are: naming a list twice is
		// a harmless paste, and two link operations for one set would fail the atomic attach.
		if _, dup := seen[name]; dup {
			continue
		}
		if len(out) == maxSharedNegativeKeywordLists {
			return nil, fmt.Errorf("google-ads: more than %d distinct shared negative keyword "+
				"lists; the maximum is %d", maxSharedNegativeKeywordLists,
				maxSharedNegativeKeywordLists)
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

// campaignSharedSetCreate is a CampaignSharedSet create: the campaign and the shared set it
// attaches. Both are resource names.
type campaignSharedSetCreate struct {
	Campaign  string `json:"campaign"`
	SharedSet string `json:"sharedSet"`
}

// attachSharedNegativeKeywordLists attaches each existing shared set to the campaign in ONE
// campaignSharedSets:mutate, atomic because partialFailure is off. It returns the shared set ids
// whose link Google confirmed.
//
// Outcomes, by the partial-result contract:
//
//   - a definite refusal: no list is attached;
//   - an ambiguous failure: UNCONFIRMED, any or all of the links may exist;
//   - a 2xx that does not describe each link as the operation at its index asked: UNCONFIRMED,
//     with the ids of the links that did match, in order, up to the first that did not.
func (c *Client) attachSharedNegativeKeywordLists(
	ctx context.Context, campaignResource, campaignID string, lists []string,
) ([]string, error) {
	if len(lists) == 0 {
		return nil, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("google-ads shared negative keyword lists aborted before any "+
			"request (context already done; campaign %s has none attached): %w", campaignID, ctxErr)
	}
	ops := make([]mutateOperation, 0, len(lists))
	for _, name := range lists {
		ops = append(ops, mutateOperation{Create: campaignSharedSetCreate{
			Campaign:  campaignResource,
			SharedSet: name,
		}})
	}
	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("campaignSharedSets:mutate"),
		mutateRequest{Operations: ops}, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return nil, fmt.Errorf("google-ads shared negative keyword lists UNCONFIRMED "+
				"(campaign %s; %d list(s) may be attached — verify in Google Ads before "+
				"retrying): %w",
				campaignID, len(lists), err)
		}
		return nil, fmt.Errorf("google-ads shared negative keyword list attach failed "+
			"(campaign %s created; the %d attach(es) are one atomic request, so no list is "+
			"attached): %w",
			campaignID, len(lists), err)
	}
	var results mutateResponse
	if uErr := json.Unmarshal(resp, &results); uErr != nil || len(results.Results) != len(ops) {
		return nil, fmt.Errorf("google-ads shared negative keyword lists UNCONFIRMED "+
			"(campaign %s; 2xx with a malformed/short mutate response — the lists may be "+
			"attached — verify in Google Ads before retrying)", campaignID)
	}
	ids := make([]string, 0, len(lists))
	seen := newResultIDSet(len(results.Results))
	for i, r := range results.Results {
		want := resourceID(lists[i])
		gotCampaign, gotSet := c.campaignSharedSetID(r.ResourceName)
		// A repeated link is UNCONFIRMED (result_ids.go), checked before the positional match
		// so the error names the repeat. The lists are de-duplicated at preflight, so a repeat
		// cannot also pass the positional check; this names it rather than the mismatch.
		if gotSet != "" {
			if dup := seen.repeat(gotSet, i); dup != "" {
				return ids, fmt.Errorf("google-ads shared negative keyword lists UNCONFIRMED "+
					"(campaign %s; %s — verify in Google Ads before retrying)", campaignID, dup)
			}
		}
		if gotCampaign != campaignID || gotSet != want {
			return ids, fmt.Errorf("google-ads shared negative keyword lists UNCONFIRMED "+
				"(campaign %s; campaignSharedSet resource name %q at index %d is not the attach "+
				"of shared set %s — verify in Google Ads before retrying)",
				campaignID, r.ResourceName, i, want)
		}
		ids = append(ids, gotSet)
	}
	return ids, nil
}

// campaignSharedSetID parses a CampaignSharedSet resource name,
// customers/<cid>/campaignSharedSets/<campaignId>~<sharedSetId>, in THIS account. It returns
// two empty strings for anything else: another account, another kind, or a malformed composite.
func (c *Client) campaignSharedSetID(resourceName string) (campaignID, sharedSetID string) {
	if c.validateResourceKind("campaignSharedSets", resourceName, false) != nil {
		return "", ""
	}
	parts := strings.Split(resourceID(resourceName), "~")
	if len(parts) != 2 || !numericID(parts[0]) || !numericID(parts[1]) {
		return "", ""
	}
	return parts[0], parts[1]
}
