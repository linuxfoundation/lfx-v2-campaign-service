// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// Picker lookups (#552): the ad segments linkedInConfig.targeting names and the conversion rules
// linkedInConfig.conversions names, read from the ad account a create uses. Both are offset-paged
// finders (li-lms-2026-10):
//
//	GET /rest/adSegments?q=accounts&accounts=List(urn%3Ali%3AsponsoredAccount%3A<a>)&start=<n>&count=<k>
//	  (Matched Audiences › Ad Segments › Find Ad Segments by Ad Account)
//	GET /rest/conversions?q=account&account=urn%3Ali%3AsponsoredAccount%3A<a>&start=<n>&count=<k>
//	  (Conversion Tracking › Find Conversions by Ad Account)
//
// Neither walk truncates: a repeated page, an element naming another account, an unusable id, or
// more than MaxLookupRows entries fails the read, because a picker shown a partial list reads it
// as the whole account.

const (
	lookupPageSize = 100
	// MaxLookupRows bounds one lookup. A local bound: an event team's account holds tens of
	// segments and conversion rules, not thousands.
	MaxLookupRows = 1000
)

// ErrLookupTooLarge marks a lookup refused because the account holds more than MaxLookupRows
// entries. Permanent while the account holds that many.
var ErrLookupTooLarge = errors.New("linkedin-ads: the ad account holds more entries than one lookup returns")

// Segment facets for an ad segment's type: RETARGETING segments are dynamicSegments, the other
// documented types audienceMatchingSegments (Targeting Criteria Facet URNs: "Website and
// engagement retargeting ad segments are categorized as dynamicSegments facet. All other types of
// audience matching segments are of type audienceMatchingSegments").
var segmentFacetByType = map[string]string{
	"RETARGETING":                       "dynamicSegments",
	"BULK":                              "audienceMatchingSegments",
	"MARKET_AUTOMATION":                 "audienceMatchingSegments",
	"BUSINESS_OBJECTIVE_BASED_AUDIENCE": "audienceMatchingSegments",
}

// unusableSegmentStatuses are the AdSegmentStatus values a campaign cannot target with: an
// EXPIRED segment was purged, a FAILED one never built. ARCHIVED stays listed: LinkedIn documents
// it as still usable until it expires.
var unusableSegmentStatuses = map[string]struct{}{"EXPIRED": {}, "FAILED": {}}

// AdSegment is one ad segment on the ad account.
type AdSegment struct {
	ID     string
	Name   string
	Type   string
	Status string
	// Facet is the linkedInConfig.targeting key the segment goes under; empty for a type this
	// service does not recognise.
	Facet string
	// ApproximateMemberCount is nil when LinkedIn reported none.
	ApproximateMemberCount *int64
}

// URN is the value linkedInConfig.targeting takes.
func (s AdSegment) URN() string { return "urn:li:adSegment:" + s.ID }

// ConversionRule is one conversion rule on the ad account.
type ConversionRule struct {
	ID      string
	Name    string
	Type    string
	Enabled bool
}

// URN is the conversion's urn:lla form; linkedInConfig.conversions takes the bare ID.
func (r ConversionRule) URN() string { return "urn:lla:llaPartnerConversion:" + r.ID }

// offsetWalk pages one finder by start/count and hands each element to visit. It stops on a
// short page with no advertised next page token, and fails on a short page that carries one, a
// repeated first id (start ignored), an element without a usable id, an element naming another
// account, or more than MaxLookupRows elements.
func (c *Client) offsetWalk(ctx context.Context, path, what string, params func(start int) map[string]string, accountID string, visit func(el responseElement, id string)) error {
	wantAccount := accountURN(accountID)
	seen := make(map[string]struct{})
	total := 0
	for start := 0; ; start += lookupPageSize {
		resp, err := c.doRequest(ctx, http.MethodGet, path, nil, params(start), nil)
		if err != nil {
			return fmt.Errorf("list linkedin %s: %w", what, err)
		}
		if resp.Elements == nil {
			return fmt.Errorf("list linkedin %s: a 2xx response with no elements field", what)
		}
		page := *resp.Elements
		for i, el := range page {
			id := strings.TrimSpace(el.ID.String())
			if !isPositiveLong(id) {
				return fmt.Errorf("list linkedin %s: an element with an unusable id", what)
			}
			if _, dup := seen[id]; dup {
				if i == 0 {
					return fmt.Errorf("list linkedin %s: page at start=%d repeats an earlier page; the finder may be ignoring start", what, start)
				}
				return fmt.Errorf("list linkedin %s: id %s listed twice", what, id)
			}
			seen[id] = struct{}{}
			// Ad Segments documents `account` as optional and the Conversions finder's response
			// may omit it, so the guard fires only when LinkedIn names an account; the request's
			// own account filter is the scoping otherwise. Refusing an absent value is a change
			// to make once a live probe shows both finders always send it.
			if el.Account != "" && el.Account != wantAccount {
				return fmt.Errorf("list linkedin %s: an element belongs to another ad account", what)
			}
			total++
			if total > MaxLookupRows {
				return fmt.Errorf("%w: more than %d %s", ErrLookupTooLarge, MaxLookupRows, what)
			}
			visit(el, id)
		}
		if len(page) < lookupPageSize {
			// A short page ends an offset walk only when LinkedIn does not also advertise a
			// continuation: a non-empty metadata.nextPageToken says more results exist, and this
			// walk pages by start/count, not by cursor, so returning here would hand back a
			// partial list as the whole of it.
			if resp.Metadata != nil && strings.TrimSpace(resp.Metadata.NextPageToken) != "" {
				return fmt.Errorf("list linkedin %s: a short page at start=%d advertises a next page token; the list may be incomplete", what, start)
			}
			return nil
		}
	}
}

// ListAdSegments lists the ad segments on the client's ad account that a campaign can target,
// sorted by name (ignoring case).
func (c *Client) ListAdSegments(ctx context.Context) ([]AdSegment, error) {
	accountID, err := c.resolveAccountID("")
	if err != nil {
		return nil, err
	}
	accounts := "List(" + encodeURNForPath(accountURN(accountID)) + ")"
	out := []AdSegment{}
	err = c.offsetWalk(ctx, "adSegments", "ad segments", func(start int) map[string]string {
		return map[string]string{"q": "accounts", "accounts": accounts, "start": strconv.Itoa(start), "count": strconv.Itoa(lookupPageSize)}
	}, accountID, func(el responseElement, id string) {
		if _, skip := unusableSegmentStatuses[el.Status]; skip {
			return
		}
		out = append(out, AdSegment{
			ID: id, Name: strings.TrimSpace(el.Name), Type: el.Type, Status: el.Status,
			Facet: segmentFacetByType[el.Type], ApproximateMemberCount: el.ApproximateMemberCount,
		})
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// ListConversionRules lists the conversion rules on the client's ad account, enabled or not,
// sorted by name (ignoring case).
func (c *Client) ListConversionRules(ctx context.Context) ([]ConversionRule, error) {
	accountID, err := c.resolveAccountID("")
	if err != nil {
		return nil, err
	}
	account := accountURN(accountID)
	out := []ConversionRule{}
	err = c.offsetWalk(ctx, "conversions", "conversion rules", func(start int) map[string]string {
		return map[string]string{"q": "account", "account": account, "start": strconv.Itoa(start), "count": strconv.Itoa(lookupPageSize)}
	}, accountID, func(el responseElement, id string) {
		out = append(out, ConversionRule{ID: id, Name: strings.TrimSpace(el.Name), Type: el.Type, Enabled: el.Enabled != nil && *el.Enabled})
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}
