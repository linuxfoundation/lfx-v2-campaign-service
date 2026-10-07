// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// Microsoft's documented Campaign.Status values (v13 CampaignStatus,
// https://learn.microsoft.com/en-us/advertising/campaign-management-service/campaignstatus),
// beyond the Active/Paused pair the toggle writes. Every one of them names a campaign that
// EXISTS in the account except Deleted.
const (
	StatusBudgetPaused          = "BudgetPaused"
	StatusBudgetAndManualPaused = "BudgetAndManualPaused"
	StatusSuspended             = "Suspended"
	StatusDeleted               = "Deleted"
)

// The adoption read asks for allCampaignTypes (monitor.go) — the documented, space-delimited v13
// set of all eight types — so a live campaign of any type, ObjectiveBased included, is returned
// and then refused definitely when it is not Search, rather than filtered out of the answer.

// ErrNotSearchCampaign reports a DEFINITE answer: the campaign exists in this account, but it is
// not a Search campaign, the only type this service creates and so the only one with a slot to
// adopt into. The dispatcher maps it to domain.ErrAdoptionCampaignTypeUnsupported (409).
var ErrNotSearchCampaign = errors.New("microsoft-ads: the campaign exists but is not a Search campaign")

// ErrNotACampaignID reports that the caller's id could not name a Microsoft Advertising campaign
// at all, so no request was sent. A PERMANENT input fault: the adopt handler maps it to 400.
var ErrNotACampaignID = errors.New("microsoft-ads: not a campaign id")

// ValidateCampaignID reports whether campaignID is the canonical spelling of a Microsoft
// Advertising campaign id: a positive base-10 int64 (Campaign.Id is a `long`) with no leading
// zero and no surrounding whitespace.
//
// Padding is refused rather than trimmed, deliberately. " 123 " is not a spelling Microsoft
// produces, and trimming it would turn "this id is malformed" into "this id is campaign 123" one
// layer below a service that has already declined to normalise it (BriefService.AdoptCampaign).
func ValidateCampaignID(campaignID string) error {
	n := json.Number(campaignID)
	if numberID(&n) != campaignID || campaignID == "" {
		return fmt.Errorf("%w: %q (want a positive base-10 64-bit integer)", ErrNotACampaignID, clipID(campaignID))
	}
	return nil
}

// CampaignRef is what the adoption read learns about one campaign: the id Microsoft ECHOED, the
// name it holds, and its raw Campaign.Status — always one of the live values here.
type CampaignRef struct {
	ID     string
	Name   string
	Status string
}

// GetCampaign reads ONE campaign by id, under THIS client's ad account, for adoption. It is the
// adoption counterpart of GetCampaignBudget and rides the same GetCampaignsByIds read
// (queryCampaignByID), so it inherits every answer-validation rule that read applies: an
// omitted Campaigns field, a null slot Microsoft did not explain, a slot for a DIFFERENT id, or
// a PartialError of any other kind is an error, never an absence.
//
// The outcomes are the CampaignAdopter contract's:
//
//   - a live campaign                       -> (ref, nil)
//   - Microsoft affirmatively has no such   -> (nil, nil)  CampaignServiceInvalidCampaignId, as a
//     campaign in this account                             fault or as a PartialError, or a
//     campaign whose Status is Deleted
//   - anything unverifiable                 -> (nil, error)
//
// "Unverifiable" includes a retried-out 429, a 5xx or transport failure (doRequest), a body
// identityjson refuses, a Name that is absent, null, empty or not a string, and a Status outside
// the documented set. Adoption binds a paid campaign on the strength of this answer, so a value
// this client does not recognise is not a guess it may make.
//
// ACCOUNT SCOPE. The read names the account twice — AccountId in the body and CustomerAccountId
// on the request — and Microsoft resolves the id inside that account only, answering
// CampaignServiceInvalidCampaignId for a campaign that lives in some other account. Unlike Meta's
// read, the Campaign object carries no AccountId to cross-check, so the scoping of the request IS
// the provenance check; the client's AccountID is the connection's own (see
// MicrosoftDispatcher.LookupCampaign).
//
// CAMPAIGN TYPE. The read asks for EVERY documented CampaignType (allCampaignTypes), so a live
// campaign is returned whatever its type, and then only Search — the one type this service
// creates, and so the one with a slot — is adoptable. Any other type is ErrNotSearchCampaign: a
// definite refusal, never an absence and never an adoption.
func (c *Client) GetCampaign(ctx context.Context, campaignID string) (*CampaignRef, error) {
	if err := ValidateCampaignID(campaignID); err != nil {
		return nil, err
	}
	camp, id, err := c.queryCampaignByIDGuarded(ctx, campaignID, "identity", allCampaignTypes, "", identityjson.Check)
	if err != nil {
		return nil, fmt.Errorf("microsoft-ads campaign lookup: %w", err)
	}
	if camp == nil {
		return nil, nil
	}
	name, ok := rawString(camp.Name)
	if !ok || strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("microsoft-ads campaign lookup: campaign %s was returned without a usable Name, so it cannot be shown to the operator binding it", id)
	}
	status, ok := rawString(camp.Status)
	if !ok {
		return nil, fmt.Errorf("microsoft-ads campaign lookup: campaign %s was returned without a readable Status", id)
	}
	var ref *CampaignRef
	switch status {
	case StatusActive, StatusPaused, StatusBudgetPaused, StatusBudgetAndManualPaused, StatusSuspended:
		ref = &CampaignRef{ID: id, Name: name, Status: status}
	case StatusDeleted:
		// The id names a real record, but not one a brief can be bound to — the verdict Google's
		// lookup reaches for a REMOVED campaign. A deleted campaign cannot spend, so reporting it
		// absent cannot license a duplicate of anything that is serving.
		return nil, nil
	default:
		return nil, fmt.Errorf("microsoft-ads campaign lookup: campaign %s reports a Status this client does not recognise, so whether it exists as a live campaign cannot be established", id)
	}
	// TYPE, decided after liveness so a deleted campaign of any type stays an absence. The read
	// asked for every type, so a non-Search answer is a DEFINITE fact about a live campaign —
	// refused distinctly, never reported absent and never adopted into the Search slot. An
	// absent or non-string CampaignType is unverifiable, not assumed to be Search.
	campaignType, ok := rawString(camp.CampaignType)
	if !ok || strings.TrimSpace(campaignType) == "" {
		return nil, fmt.Errorf("microsoft-ads campaign lookup: campaign %s was returned without a readable CampaignType, so whether it can be adopted cannot be established", id)
	}
	if campaignType != campaignTypeSearch {
		return nil, fmt.Errorf("microsoft-ads campaign lookup: campaign %s: %w", id, ErrNotSearchCampaign)
	}
	return ref, nil
}

// rawString decodes a raw JSON value that must be a string. Absent, null, or any other kind is
// ("", false). The decode error is dropped: it would quote the upstream value.
func rawString(raw json.RawMessage) (string, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return "", false
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", false
	}
	return out, true
}
