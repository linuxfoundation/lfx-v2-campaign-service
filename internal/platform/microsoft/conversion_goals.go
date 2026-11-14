// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// ---------------------------------------------------------------------------
// Conversion goals at create (#563, gap 7): EXISTING conversion goals on the shared account,
// named by microsoftConfig.conversionGoalIds and set as the new campaign's GoalIds. LFX creates
// no UET tag and no goal: marketing ops owns both, so a goal that is not already on the account
// is refused before anything is created.
//
// Microsoft Advertising Campaign Management v13 reference (learn.microsoft.com, read
// 2026-11-14):
//   - GetConversionGoalsByIds (POST ConversionGoals/QueryByIds) takes ConversionGoalIds ("A
//     maximum of 100 identifiers") and ConversionGoalTypes, both required, the types "as flags"
//     (the SOAP form is a space-delimited list, which this client sends, as the monitor does for
//     CampaignType). It answers ConversionGoals, "an array of ConversionGoal objects that
//     corresponds directly to the conversion goal identifiers that you specified": "For each
//     array index where a conversion goal was not retrieved, the corresponding element will be
//     null." PartialErrors "do not correspond directly to the list of items in the request".
//   - ConversionGoal carries Id, Name, Status (user-determined, e.g. Paused), Type and Scope
//     (Account or Customer).
//   - Campaign.GoalIds: "A list of goal IDs" (long array). The page states no Add rule.
//   - MaxConversionsBiddingScheme ("Type": "MaxConversions") takes an optional TargetCpa; the
//     TargetCpaBiddingScheme is retired for new campaigns ("Any bid strategy set to Target CPA
//     will be silently converted to Maximize Conversions using the Target CPA value"), so a
//     target CPA is sent as MaxConversions with TargetCpa.
//
// VERIFICATION LEVEL: none of this has been exercised against a live account, which is why the
// dispatcher keeps it behind MICROSOFT_CONVERSION_GOALS_ENABLED.
// ---------------------------------------------------------------------------

const (
	// FieldConversionGoalIDs is the microsoftConfig key, named in refusals.
	FieldConversionGoalIDs = "conversionGoalIds"
	// MaxConversionGoals bounds one create's goals. GetConversionGoalsByIds takes 100; a campaign
	// optimizing toward more than a handful of goals is a configuration mistake, and 20 keeps the
	// refusal readable.
	MaxConversionGoals = 20

	// conversionGoalTypesAll is every ConversionGoalType, space-delimited, so the read never
	// misses a goal for its type.
	conversionGoalTypesAll = "Url Duration PagesViewedPerVisit Event AppInstall OfflineConversion InStoreTransaction"

	// conversionGoalStatusPaused / Deleted are the statuses a goal cannot be bid toward: a paused
	// goal records no conversions, and a deleted one is gone.
	conversionGoalStatusPaused  = "Paused"
	conversionGoalStatusDeleted = "Deleted"

	// BidSchemeMaxConversions is the conversion-based campaign bid strategy, sent with an
	// optional TargetCpa.
	BidSchemeMaxConversions = "MaxConversions"

	// maxConversionGoalRows bounds the decoded picker read.
	maxConversionGoalRows = 5000
)

// ConversionGoalError is a refusal of the goal input; Field is the microsoftConfig key.
type ConversionGoalError struct {
	Field string
	Msg   string
}

func (e *ConversionGoalError) Error() string { return e.Field + ": " + e.Msg }

// ValidateConversionGoalIDs checks the ids without contacting Microsoft: each the canonical
// decimal spelling of a positive int64 (goal ids are longs), never trimmed; at most
// MaxConversionGoals; no repeat. A refusal names a 1-based position, never a value.
func ValidateConversionGoalIDs(ids []string) error {
	if len(ids) > MaxConversionGoals {
		return &ConversionGoalError{Field: FieldConversionGoalIDs, Msg: fmt.Sprintf("holds %d ids; at most %d conversion goals per campaign", len(ids), MaxConversionGoals)}
	}
	seen := make(map[string]struct{}, len(ids))
	for i, id := range ids {
		if !canonicalPositiveID(id) {
			return &ConversionGoalError{Field: FieldConversionGoalIDs, Msg: fmt.Sprintf("entry %d is not a Microsoft conversion goal id (a positive whole number, no sign or leading zero)", i+1)}
		}
		if _, dup := seen[id]; dup {
			return &ConversionGoalError{Field: FieldConversionGoalIDs, Msg: fmt.Sprintf("entry %d repeats an earlier id", i+1)}
		}
		seen[id] = struct{}{}
	}
	return nil
}

type getConversionGoalsByIDsRequest struct {
	// ConversionGoalIds null means "every goal of the types" (the picker read).
	ConversionGoalIds   []json.Number `json:"ConversionGoalIds"`
	ConversionGoalTypes string        `json:"ConversionGoalTypes"`
}

type msConversionGoal struct {
	Id     *json.Number `json:"Id"`
	Name   string       `json:"Name"`
	Status string       `json:"Status"`
	Type   string       `json:"Type"`
	Scope  string       `json:"Scope"`
}

type getConversionGoalsByIDsResponse struct {
	ConversionGoals []*msConversionGoal `json:"ConversionGoals"`
	PartialErrors   indexedErrors       `json:"PartialErrors"`
}

// errConversionGoalLookupMalformed marks a GetConversionGoalsByIds answer this client cannot
// believe.
var errConversionGoalLookupMalformed = errors.New("microsoft-ads conversion goal lookup could not be confirmed")

// CheckConversionGoals confirms, before anything is created, that every id is a conversion goal
// the client's account can use, with ONE GetConversionGoalsByIds read. An id the answer leaves
// null (not retrieved), or returns Paused or Deleted, is a domain.PreflightRefusal naming the
// field and the id. A call that fails, or an answer that cannot be believed (no ConversionGoals
// array, not index-aligned, an entry that is not the goal asked for), is a plain error. Either
// way nothing is created. A partial error alone is not attributed (Microsoft documents that they
// do not follow the request order); the null entry at that index is what refuses the id.
func (c *Client) CheckConversionGoals(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	nums := make([]json.Number, len(ids))
	for i, id := range ids {
		nums[i] = json.Number(id)
	}
	raw, err := c.doRequest(ctx, http.MethodPost, "ConversionGoals/QueryByIds", getConversionGoalsByIDsRequest{
		ConversionGoalIds: nums, ConversionGoalTypes: conversionGoalTypesAll,
	}, true /* a read */)
	if err != nil {
		return fmt.Errorf("microsoft-ads conversion goal lookup failed: %w", err)
	}
	var probe map[string]json.RawMessage
	var resp getConversionGoalsByIDsResponse
	if json.Unmarshal(raw, &probe) != nil || json.Unmarshal(raw, &resp) != nil {
		return fmt.Errorf("%w: the response is not the documented shape", errConversionGoalLookupMalformed)
	}
	if goals, ok := probe["ConversionGoals"]; !ok || isJSONNullRaw(goals) {
		return fmt.Errorf("%w: the response carries no ConversionGoals array", errConversionGoalLookupMalformed)
	}
	if len(resp.ConversionGoals) != len(ids) {
		return fmt.Errorf("%w: it returned %d entries for %d ids", errConversionGoalLookupMalformed, len(resp.ConversionGoals), len(ids))
	}
	for i, id := range ids {
		g := resp.ConversionGoals[i]
		if g == nil {
			return domain.PreflightRefusalf("invalid microsoftConfig.%s: Microsoft Advertising has no conversion goal %s on this account; marketing ops creates goals, this service only names existing ones", FieldConversionGoalIDs, domain.CallerValue(id))
		}
		if numberID(g.Id) != id {
			return fmt.Errorf("%w: position %d is not the conversion goal asked for", errConversionGoalLookupMalformed, i+1)
		}
		switch g.Status {
		case conversionGoalStatusPaused, conversionGoalStatusDeleted:
			return domain.PreflightRefusalf("invalid microsoftConfig.%s: conversion goal %s is %s, so it records no conversions to bid toward", FieldConversionGoalIDs, domain.CallerValue(id), g.Status)
		}
	}
	return nil
}

// ConversionGoal is one conversion goal on the account, for the picker.
type ConversionGoal struct {
	ID     string
	Name   string
	Status string
	Type   string
	Scope  string
}

// ErrConversionGoalsMalformed marks a picker answer this client cannot believe. The service maps
// it, like any upstream failure it cannot classify, to a 503 with nothing partial.
var ErrConversionGoalsMalformed = errors.New("microsoft-ads conversion goal read could not be confirmed")

// ListConversionGoals reads every conversion goal the client's account can use with ONE
// GetConversionGoalsByIds call (no ids, every type). It is the same call CheckConversionGoals
// makes, so an id it lists as Active is one that check accepts. A pure read, retried on 429. The
// rows are in Microsoft's order; the caller sorts.
//
// An answer it cannot believe is ErrConversionGoalsMalformed with no rows: no ConversionGoals
// array, a null entry, an id that is not a positive decimal or repeats, more than
// maxConversionGoalRows entries, or any partial error (an incomplete list must not pass for a
// complete one).
func (c *Client) ListConversionGoals(ctx context.Context) ([]ConversionGoal, error) {
	raw, err := c.doRequest(ctx, http.MethodPost, "ConversionGoals/QueryByIds", getConversionGoalsByIDsRequest{
		ConversionGoalTypes: conversionGoalTypesAll,
	}, true /* a read */)
	if err != nil {
		return nil, fmt.Errorf("microsoft-ads conversion goal read failed: %w", err)
	}
	var probe map[string]json.RawMessage
	var resp getConversionGoalsByIDsResponse
	if json.Unmarshal(raw, &probe) != nil || json.Unmarshal(raw, &resp) != nil {
		return nil, fmt.Errorf("%w: the response is not the documented shape", ErrConversionGoalsMalformed)
	}
	if goals, ok := probe["ConversionGoals"]; !ok || isJSONNullRaw(goals) {
		return nil, fmt.Errorf("%w: the response carries no ConversionGoals array", ErrConversionGoalsMalformed)
	}
	if len(resp.ConversionGoals) > maxConversionGoalRows {
		return nil, fmt.Errorf("%w: it returned more than %d conversion goals", ErrConversionGoalsMalformed, maxConversionGoalRows)
	}
	for _, it := range resp.PartialErrors.Items {
		if it.isError() {
			return nil, fmt.Errorf("%w: it reported a partial error, so the list may be incomplete", ErrConversionGoalsMalformed)
		}
	}
	if resp.PartialErrors.Truncated {
		return nil, fmt.Errorf("%w: it reported a partial error, so the list may be incomplete", ErrConversionGoalsMalformed)
	}
	out := make([]ConversionGoal, 0, len(resp.ConversionGoals))
	seen := make(map[string]struct{}, len(resp.ConversionGoals))
	for i, g := range resp.ConversionGoals {
		if g == nil {
			return nil, fmt.Errorf("%w: position %d is empty", ErrConversionGoalsMalformed, i+1)
		}
		id := numberID(g.Id)
		if !canonicalPositiveID(id) {
			return nil, fmt.Errorf("%w: position %d has no usable id", ErrConversionGoalsMalformed, i+1)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("%w: position %d repeats an id", ErrConversionGoalsMalformed, i+1)
		}
		seen[id] = struct{}{}
		out = append(out, ConversionGoal{ID: id, Name: g.Name, Status: g.Status, Type: g.Type, Scope: g.Scope})
	}
	return out, nil
}
