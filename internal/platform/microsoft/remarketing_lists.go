// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Remarketing-list picker read (#563, gap 4 follow-up): every remarketing list on the client's
// account, for the microsoftConfig.audienceIds / excludedAudienceIds picker.
//
// Microsoft Advertising Campaign Management v13 reference (learn.microsoft.com, read
// 2026-11-14), GetAudiencesByIds (POST Audiences/QueryByIds): AudienceIds is optional, and when
// it is nil or empty the operation returns every audience of the requested Type on the account.
// The answer is Audiences (RemarketingList entries carry Id, Name, Description,
// MembershipDuration, Scope and SearchSize among others) plus PartialErrors. There is no paging.
//
// It is the same call CheckRemarketingLists makes before a create, with no ids, so an id it lists
// is one that check accepts. System-generated lists (All Visitors, All Converters, Smart lists)
// are not returned by GetAudiencesByIds, so they are not listed either.
//
// VERIFICATION LEVEL: none of this has been exercised against a live account, which is why the
// dispatcher keeps it behind MICROSOFT_REMARKETING_LISTS_ENABLED.
// ---------------------------------------------------------------------------

// maxRemarketingListRows bounds the decoded list, so an answer that never ends cannot hold the
// process. Far above the lists one account holds in practice; more is refused as unbelievable.
const maxRemarketingListRows = 5000

// RemarketingList is one remarketing list on the account. MembershipDuration (days) and
// SearchSize are nil when Microsoft sent none, which is not a value of 0.
type RemarketingList struct {
	ID                 string
	Name               string
	Description        string
	Scope              string
	MembershipDuration *int
	SearchSize         *int64
}

type listRemarketingListsRequest struct {
	// AudienceIds is sent as null: "all audiences of the type".
	AudienceIds []json.Number `json:"AudienceIds"`
	Type        string        `json:"Type"`
}

type msRemarketingList struct {
	Id          *json.Number `json:"Id"`
	Type        string       `json:"Type"`
	Name        string       `json:"Name"`
	Description string       `json:"Description"`
	Scope       string       `json:"Scope"`
	// MembershipDuration (an int) and SearchSize (a long) are decoded raw: Microsoft's REST JSON
	// sends a long as a quoted string ("12000"), and may quote either, so both spellings are
	// accepted and validated by wholeNumberField rather than failing the whole answer.
	MembershipDuration json.RawMessage `json:"MembershipDuration"`
	SearchSize         json.RawMessage `json:"SearchSize"`
}

// wholeNumberField reads an optional non-negative whole number sent as a JSON number (12000) or a
// quoted one ("12000"), at most max. Absent or null is (nil, true); anything else that is not
// such a number (a fraction, a negative, text, an overflow) is (nil, false).
func wholeNumberField(raw json.RawMessage, max int64) (*int64, bool) {
	if len(raw) == 0 || isJSONNullRaw(raw) {
		return nil, true
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return nil, false
	}
	s := strings.TrimSpace(n.String())
	if s == "" || strings.HasPrefix(s, "+") {
		return nil, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 || v > max {
		return nil, false
	}
	return &v, true
}

type listRemarketingListsResponse struct {
	Audiences     []*msRemarketingList `json:"Audiences"`
	PartialErrors indexedErrors        `json:"PartialErrors"`
}

// ErrRemarketingListsMalformed marks a list answer this client cannot believe. The service maps
// it, like any upstream failure it cannot classify, to a 503 with nothing partial.
var ErrRemarketingListsMalformed = errors.New("microsoft-ads remarketing list read could not be confirmed")

// ListRemarketingLists reads every remarketing list on the client's account with ONE
// GetAudiencesByIds call (no ids, Type RemarketingList). A pure read, retried on 429 like every
// read here. The rows are in Microsoft's order; the caller sorts.
//
// An answer it cannot believe is ErrRemarketingListsMalformed with no rows: no Audiences array (a
// 2xx without it describes nothing), a null entry, an id that is not a positive decimal or
// repeats, an entry of another audience type (the Type filter was not applied), more than
// maxRemarketingListRows entries, or any partial error (an incomplete list must not pass for a
// complete one).
func (c *Client) ListRemarketingLists(ctx context.Context) ([]RemarketingList, error) {
	if err := c.validateAccountIDs(); err != nil {
		return nil, err
	}
	raw, err := c.doRequest(ctx, http.MethodPost, "Audiences/QueryByIds", listRemarketingListsRequest{Type: audienceTypeRemarketingList}, true /* a read */)
	if err != nil {
		return nil, fmt.Errorf("microsoft-ads remarketing list read failed: %w", err)
	}
	var probe map[string]json.RawMessage
	var resp listRemarketingListsResponse
	if json.Unmarshal(raw, &probe) != nil || json.Unmarshal(raw, &resp) != nil {
		return nil, fmt.Errorf("%w: the response is not the documented shape", ErrRemarketingListsMalformed)
	}
	if aud, ok := probe["Audiences"]; !ok || isJSONNullRaw(aud) {
		return nil, fmt.Errorf("%w: the response carries no Audiences array", ErrRemarketingListsMalformed)
	}
	if resp.PartialErrors.Truncated {
		return nil, fmt.Errorf("%w: it reported errors", ErrRemarketingListsMalformed)
	}
	for _, it := range resp.PartialErrors.Items {
		if present, _ := audienceErrorCode(it.ErrorCode, it.Code); present {
			return nil, fmt.Errorf("%w: it reported errors", ErrRemarketingListsMalformed)
		}
	}
	if len(resp.Audiences) > maxRemarketingListRows {
		return nil, fmt.Errorf("%w: it returned more than %d lists", ErrRemarketingListsMalformed, maxRemarketingListRows)
	}
	out := make([]RemarketingList, 0, len(resp.Audiences))
	seen := make(map[string]struct{}, len(resp.Audiences))
	for i, a := range resp.Audiences {
		if a == nil {
			return nil, fmt.Errorf("%w: position %d is empty", ErrRemarketingListsMalformed, i+1)
		}
		id := numberID(a.Id)
		if !canonicalPositiveID(id) {
			return nil, fmt.Errorf("%w: position %d has no usable id", ErrRemarketingListsMalformed, i+1)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("%w: position %d repeats an id", ErrRemarketingListsMalformed, i+1)
		}
		seen[id] = struct{}{}
		if a.Type != audienceTypeRemarketingList {
			return nil, fmt.Errorf("%w: position %d is not a remarketing list", ErrRemarketingListsMalformed, i+1)
		}
		size, ok := wholeNumberField(a.SearchSize, math.MaxInt64)
		if !ok {
			return nil, fmt.Errorf("%w: position %d has an unreadable SearchSize", ErrRemarketingListsMalformed, i+1)
		}
		dur64, ok := wholeNumberField(a.MembershipDuration, math.MaxInt32)
		if !ok {
			return nil, fmt.Errorf("%w: position %d has an unreadable MembershipDuration", ErrRemarketingListsMalformed, i+1)
		}
		var dur *int
		if dur64 != nil {
			d := int(*dur64)
			dur = &d
		}
		out = append(out, RemarketingList{
			ID: id, Name: a.Name, Description: a.Description, Scope: a.Scope,
			MembershipDuration: dur, SearchSize: size,
		})
	}
	return out, nil
}
