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
// Shared negative keyword lists at create (#563, gap 9): existing account-level
// NegativeKeywordList shared entities associated with the campaign a create confirms.
//
// Microsoft Advertising Campaign Management v13 reference (learn.microsoft.com, read
// 2026-11-14):
//   - GetSharedEntities (POST SharedEntities/Query) takes SharedEntityType ("NegativeKeywordList")
//     and SharedEntityScope ("Account"), and answers SharedEntities: every shared entity of the
//     type on the account, each with Id, Name and Type. There is no paging.
//   - SetSharedEntityAssociations (POST SharedEntityAssociations/Set) takes Associations, each
//     {EntityId, EntityType "Campaign", SharedEntityId, SharedEntityType "NegativeKeywordList"},
//     and SharedEntityScope, and answers PartialErrors: one BatchError per refused association,
//     carrying its Index.
//   - A campaign can be associated with at most 20 negative keyword lists.
//
// VERIFICATION LEVEL: none of this has been exercised against a live account, which is why the
// dispatcher keeps it behind MICROSOFT_SHARED_NEGATIVE_LISTS_ENABLED.
// ---------------------------------------------------------------------------

const (
	// FieldSharedNegativeKeywordListIDs is the microsoftConfig key, named in refusals.
	FieldSharedNegativeKeywordListIDs = "sharedNegativeKeywordListIds"
	// MaxSharedNegativeKeywordLists is Microsoft's per-campaign association limit.
	MaxSharedNegativeKeywordLists = 20

	sharedEntityTypeNegativeKeywordList = "NegativeKeywordList"
	sharedEntityScopeAccount            = "Account"
	sharedEntityAssociationCampaign     = "Campaign"
	// maxSharedEntityRows bounds the decoded pre-create read.
	maxSharedEntityRows = 5000
)

// SharedListError is a refusal of the shared-list input; Field is the microsoftConfig key.
type SharedListError struct {
	Field string
	Msg   string
}

func (e *SharedListError) Error() string { return e.Field + ": " + e.Msg }

// ValidateSharedNegativeKeywordLists checks the ids without contacting Microsoft: each the
// canonical decimal spelling of a positive int64 (shared entity ids are longs), compared exactly
// and never trimmed; at most MaxSharedNegativeKeywordLists; no repeat. A refusal names a 1-based
// position, never a value.
func ValidateSharedNegativeKeywordLists(ids []string) error {
	if len(ids) > MaxSharedNegativeKeywordLists {
		return &SharedListError{Field: FieldSharedNegativeKeywordListIDs, Msg: fmt.Sprintf("holds %d ids; a campaign can be associated with at most %d negative keyword lists", len(ids), MaxSharedNegativeKeywordLists)}
	}
	seen := make(map[string]struct{}, len(ids))
	for i, id := range ids {
		if !canonicalPositiveID(id) {
			return &SharedListError{Field: FieldSharedNegativeKeywordListIDs, Msg: fmt.Sprintf("entry %d is not a Microsoft negative keyword list id (a positive whole number, no sign or leading zero)", i+1)}
		}
		if _, dup := seen[id]; dup {
			return &SharedListError{Field: FieldSharedNegativeKeywordListIDs, Msg: fmt.Sprintf("entry %d repeats an earlier id", i+1)}
		}
		seen[id] = struct{}{}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Pre-create check: GetSharedEntities.
// ---------------------------------------------------------------------------

type getSharedEntitiesRequest struct {
	SharedEntityType  string `json:"SharedEntityType"`
	SharedEntityScope string `json:"SharedEntityScope"`
}

type msSharedEntity struct {
	Id   *json.Number `json:"Id"`
	Type string       `json:"Type"`
}

type getSharedEntitiesResponse struct {
	SharedEntities []*msSharedEntity `json:"SharedEntities"`
}

// errSharedListLookupMalformed marks a GetSharedEntities answer this client cannot believe.
var errSharedListLookupMalformed = errors.New("microsoft-ads negative keyword list lookup could not be confirmed")

// CheckSharedNegativeKeywordLists confirms, before anything is created, that every id is a
// negative keyword list on the client's account, with ONE GetSharedEntities read. An id the
// answer does not hold, or holds as another shared entity type, is a domain.PreflightRefusal
// naming the field and the id. A call that fails, or an answer that cannot be believed (no
// SharedEntities array, a null entry, an entry with no usable id, more than maxSharedEntityRows),
// is a plain error. Either way nothing is created.
func (c *Client) CheckSharedNegativeKeywordLists(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	raw, err := c.doRequest(ctx, http.MethodPost, "SharedEntities/Query", getSharedEntitiesRequest{
		SharedEntityType: sharedEntityTypeNegativeKeywordList, SharedEntityScope: sharedEntityScopeAccount,
	}, true /* a read */)
	if err != nil {
		return fmt.Errorf("microsoft-ads negative keyword list lookup failed: %w", err)
	}
	var probe map[string]json.RawMessage
	var resp getSharedEntitiesResponse
	if json.Unmarshal(raw, &probe) != nil || json.Unmarshal(raw, &resp) != nil {
		return fmt.Errorf("%w: the response is not the documented shape", errSharedListLookupMalformed)
	}
	if ents, ok := probe["SharedEntities"]; !ok || isJSONNullRaw(ents) {
		return fmt.Errorf("%w: the response carries no SharedEntities array", errSharedListLookupMalformed)
	}
	if len(resp.SharedEntities) > maxSharedEntityRows {
		return fmt.Errorf("%w: it returned more than %d shared entities", errSharedListLookupMalformed, maxSharedEntityRows)
	}
	types := make(map[string]string, len(resp.SharedEntities))
	for i, e := range resp.SharedEntities {
		if e == nil {
			return fmt.Errorf("%w: position %d is empty", errSharedListLookupMalformed, i+1)
		}
		id := numberID(e.Id)
		if !canonicalPositiveID(id) {
			return fmt.Errorf("%w: position %d has no usable id", errSharedListLookupMalformed, i+1)
		}
		types[id] = e.Type
	}
	for _, id := range ids {
		typ, ok := types[id]
		if !ok {
			return domain.PreflightRefusalf("invalid microsoftConfig.%s: Microsoft Advertising has no negative keyword list %s on this account", FieldSharedNegativeKeywordListIDs, domain.CallerValue(id))
		}
		if typ != sharedEntityTypeNegativeKeywordList {
			return domain.PreflightRefusalf("invalid microsoftConfig.%s: shared entity %s is a %s, not a negative keyword list", FieldSharedNegativeKeywordListIDs, domain.CallerValue(id), outputCode(typ))
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// After create: SetSharedEntityAssociations.
// ---------------------------------------------------------------------------

// SharedListOutcome is one list's positional outcome on the campaign: OutcomeApplied,
// OutcomeFailed or OutcomeUnconfirmed. The JSON tags are the persisted shape
// (CampaignResult.SharedNegativeKeywordLists).
type SharedListOutcome struct {
	ListID    string `json:"listId"`
	Outcome   string `json:"outcome"`
	ErrorCode string `json:"errorCode,omitempty"`
}

type msSharedEntityAssociation struct {
	EntityId         json.Number `json:"EntityId"`
	EntityType       string      `json:"EntityType"`
	SharedEntityId   json.Number `json:"SharedEntityId"`
	SharedEntityType string      `json:"SharedEntityType"`
}

type setSharedEntityAssociationsRequest struct {
	Associations      []msSharedEntityAssociation `json:"Associations"`
	SharedEntityScope string                      `json:"SharedEntityScope"`
}

type setSharedEntityAssociationsResponse struct {
	PartialErrors indexedErrors `json:"PartialErrors"`
}

// AttachCampaignSharedNegativeKeywordLists associates every list with ONE campaign in one
// SetSharedEntityAssociations call and reports one outcome per list, in request order. The ids are
// expected to have passed ValidateSharedNegativeKeywordLists and CheckSharedNegativeKeywordLists.
//
// NOT RETRIED ON 429 (idempotent=false), like every write here. A call whose outcome is
// ambiguous (a transport failure, a 429 or 5xx, a 2xx this client cannot read) leaves every list
// UNCONFIRMED; a definite refusal leaves them FAILED; a call never sent because the context ended
// is FAILED with ErrorCode NOT_SENT. In an answered call, a list a PartialErrors entry names is
// FAILED with its code, and when an error cannot be attributed (no usable index, a truncated
// array) every other list is UNCONFIRMED; otherwise it is APPLIED. The error, when non-nil,
// describes a whole-call failure in code-only text; the outcomes are always complete.
func (c *Client) AttachCampaignSharedNegativeKeywordLists(ctx context.Context, campaignID string, ids []string) ([]SharedListOutcome, error) {
	if !canonicalPositiveID(campaignID) {
		return nil, fmt.Errorf("microsoft-ads: campaign id %q is not a numeric id", truncate(campaignID, maxErrorBodyChars))
	}
	out := make([]SharedListOutcome, len(ids))
	for i, id := range ids {
		out[i] = SharedListOutcome{ListID: id}
	}
	all := func(outcome, code string) {
		for i := range out {
			out[i].Outcome, out[i].ErrorCode = outcome, code
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	if ctx.Err() != nil {
		all(OutcomeFailed, errorCodeNotSent)
		return out, errors.New("microsoft-ads negative keyword lists not sent: the context ended first")
	}
	assocs := make([]msSharedEntityAssociation, len(ids))
	for i, id := range ids {
		assocs[i] = msSharedEntityAssociation{
			EntityId: json.Number(campaignID), EntityType: sharedEntityAssociationCampaign,
			SharedEntityId: json.Number(id), SharedEntityType: sharedEntityTypeNegativeKeywordList,
		}
	}
	raw, err := c.doRequest(ctx, http.MethodPost, "SharedEntityAssociations/Set", setSharedEntityAssociationsRequest{
		Associations: assocs, SharedEntityScope: sharedEntityScopeAccount,
	}, false)
	if err != nil {
		if IsOutcomeUnconfirmed(err) {
			all(OutcomeUnconfirmed, "")
			return out, fmt.Errorf("microsoft-ads negative keyword lists UNCONFIRMED (they may be associated): %w", err)
		}
		all(OutcomeFailed, "")
		return out, fmt.Errorf("microsoft-ads negative keyword lists not associated: %w", err)
	}
	// The body must be a JSON object; PartialErrors may be absent or null, which is "no error".
	var probe map[string]json.RawMessage
	var resp setSharedEntityAssociationsResponse
	if json.Unmarshal(raw, &probe) != nil || probe == nil || json.Unmarshal(raw, &resp) != nil {
		all(OutcomeUnconfirmed, "")
		return out, errors.New("microsoft-ads negative keyword lists UNCONFIRMED: the response could not be read")
	}
	att := attribute(resp.PartialErrors, len(ids))
	for i := range out {
		switch {
		case att.errored[i]:
			code := att.byIndex[i]
			if code == "" {
				code = withheldErrorCode
			}
			out[i].Outcome, out[i].ErrorCode = OutcomeFailed, code
		case att.unattributable:
			out[i].Outcome = OutcomeUnconfirmed
		default:
			out[i].Outcome = OutcomeApplied
		}
	}
	return out, nil
}
