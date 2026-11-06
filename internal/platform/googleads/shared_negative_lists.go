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
// was refused. Verified live on 2026-11-06 (#536, v23): CreateCampaign attached two
// NEGATIVE_KEYWORDS sets to paused Search campaign 24433949469 through the exact request below,
// one campaignSharedSets:mutate carrying both operations. Google answered with
// <campaignId>~<sharedSetId> results in operation order, the positional check passed, and GAQL
// read back both campaign_shared_set rows ENABLED, along with the sets' BROAD, PHRASE and EXACT
// keyword criteria. A shared set of another type is NOT refused by Google. validateOnly on the
// same day accepted a NEGATIVE_PLACEMENTS and a BRANDS set on a Search campaign. No such set has
// been attached for real, so its effect on a Search campaign is unobserved.
//
// So this client refuses one itself (#548, owner decision 2026-11-06). A resource name says
// nothing about its set's type, so the Search create reads shared_set.type and .status for every
// named list in ONE read-only GAQL query on the connection's account, before the budget mutate
// (checkSharedNegativeKeywordListTypes). A set that is not NEGATIVE_KEYWORDS, is not ENABLED, or
// is not in the account is refused there, naming it. A read that fails, or answers with rows
// that cannot be trusted, stops the create too: the lists are never attached unchecked.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
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
				"customer %q, not the campaign's account %q; a shared set cannot be attached "+
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

// The SharedSetTypeEnum and SharedSetStatusEnum values (v23) the type check names. Only
// sharedSetTypeNegativeKeywords is accepted; the others are named in the refusal so the operator
// knows which list they picked. A value outside this vocabulary is upstream text and is never
// echoed: the refusal says the type is not recognised instead.
const (
	sharedSetTypeNegativeKeywords = "NEGATIVE_KEYWORDS"
	sharedSetStatusEnabled        = "ENABLED"
	sharedSetStatusRemoved        = "REMOVED"
)

// sharedSetTypesNamed are the other SharedSetTypeEnum values a refusal may name verbatim.
var sharedSetTypesNamed = map[string]bool{
	"NEGATIVE_PLACEMENTS":             true,
	"ACCOUNT_LEVEL_NEGATIVE_KEYWORDS": true,
	"BRANDS":                          true,
	"WEBPAGES":                        true,
}

// sharedSetRow is one row of checkSharedNegativeKeywordListTypes' query.
type sharedSetRow struct {
	SharedSet struct {
		ResourceName string `json:"resourceName"`
		Type         string `json:"type"`
		Status       string `json:"status"`
	} `json:"sharedSet"`
}

// checkSharedNegativeKeywordListTypes reads every validated list's type and status in ONE GAQL
// query on the connection's account and refuses, before any write, a list that is not an
// ENABLED NEGATIVE_KEYWORDS shared set of that account (#548). lists is
// validateSharedNegativeKeywordLists' output: trimmed, de-duplicated, canonical, and every one
// already in this account, so each is safe to interpolate as a GAQL string literal.
//
// Outcomes:
//
//   - a refusal of the caller's input (a wrong type, a set not ENABLED, no row for a set) is a
//     domain.PreflightRefusal naming the list and its id: a read made it, and no
//     write has been sent;
//   - a read that failed is returned as it is, so the dispatch layer's mapping reports the
//     status (a 5xx or 429 as "answered HTTP n before anything was created") and the claim is
//     released;
//   - rows that cannot be trusted (undecodable, not requested, repeated) fail the create the
//     same way, as a plain error: the answer is not one a list can be attached on.
func (c *Client) checkSharedNegativeKeywordListTypes(ctx context.Context, lists []string) error {
	if len(lists) == 0 {
		return nil
	}
	literals := make([]string, len(lists))
	position := make(map[string]int, len(lists))
	for i, name := range lists {
		literals[i] = "'" + name + "'"
		position[name] = i
	}
	query := "SELECT shared_set.resource_name, shared_set.type, shared_set.status FROM shared_set " +
		"WHERE shared_set.resource_name IN (" + strings.Join(literals, ", ") + ")"
	rows, err := c.gaqlSearch(ctx, query)
	if err != nil {
		return fmt.Errorf("google-ads shared negative keyword lists could not be checked "+
			"(the shared set read failed; nothing was created): %w", err)
	}
	found := make([]*sharedSetRow, len(lists))
	for _, raw := range rows {
		var row sharedSetRow
		if uErr := json.Unmarshal(raw, &row); uErr != nil {
			return fmt.Errorf("google-ads shared negative keyword lists could not be checked "+
				"(a shared set row did not decode; nothing was created): %w", uErr)
		}
		i, ok := position[row.SharedSet.ResourceName]
		if !ok {
			return fmt.Errorf("google-ads shared negative keyword lists could not be checked " +
				"(the read returned a shared set that was not asked for; nothing was created)")
		}
		if found[i] != nil {
			return fmt.Errorf("google-ads shared negative keyword lists could not be checked "+
				"(the read returned shared set %q twice; nothing was created)", resourceID(lists[i]))
		}
		found[i] = &row
	}
	for i, name := range lists {
		if err := sharedSetRefusal(c.account.CustomerID, name, found[i]); err != nil {
			return domain.WrapQuotedPreflightRefusal(err)
		}
	}
	return nil
}

// sharedSetRefusal is the refusal for the named list, or nil when row is an ENABLED NEGATIVE_KEYWORDS
// set. Every caller value and the stored account id are quoted (%q), as the foreign-account
// refusals quote theirs (#528); the type and status are named only from this file's fixed
// vocabulary, never echoed from the response.
func sharedSetRefusal(customerID, name string, row *sharedSetRow) error {
	id := resourceID(name)
	if row == nil {
		return fmt.Errorf("google-ads: shared negative keyword list %q (shared set %q) was not "+
			"found in account %q; name an existing negative keyword list of that account, or omit "+
			"it", name, id, customerID)
	}
	switch t := row.SharedSet.Type; {
	case t == sharedSetTypeNegativeKeywords:
	case sharedSetTypesNamed[t]:
		return fmt.Errorf("google-ads: shared negative keyword list %q (shared set %q) is a shared "+
			"set of type %s, not %s; sharedNegativeKeywordLists attaches only negative keyword lists, "+
			"so omit it", name, id, t, sharedSetTypeNegativeKeywords)
	default:
		return fmt.Errorf("google-ads: shared negative keyword list %q (shared set %q) is a "+
			"shared set of a type this client does not recognise, not %s; "+
			"sharedNegativeKeywordLists attaches only negative keyword lists, so omit it",
			name, id, sharedSetTypeNegativeKeywords)
	}
	switch s := row.SharedSet.Status; s {
	case sharedSetStatusEnabled:
		return nil
	case sharedSetStatusRemoved:
		return fmt.Errorf("google-ads: shared negative keyword list %q (shared set %q) is %s in "+
			"account %q; name an ENABLED negative keyword list, or omit it",
			name, id, sharedSetStatusRemoved, customerID)
	default:
		return fmt.Errorf("google-ads: shared negative keyword list %q (shared set %q) is not "+
			"ENABLED in account %q; name an ENABLED negative keyword list, or omit it",
			name, id, customerID)
	}
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
