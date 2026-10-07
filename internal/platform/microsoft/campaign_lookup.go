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
// CAMPAIGN TYPE. The read asks for CampaignType "Search", the only type this service creates, so
// the adopted row's single slot is a Search slot. A campaign of another type is not returned in
// the slot; queryCampaignByID reports that as an unexplained null slot, i.e. unverifiable — the
// same verdict Google's adoption gives a campaign type it has no slot for.
func (c *Client) GetCampaign(ctx context.Context, campaignID string) (*CampaignRef, error) {
	if err := ValidateCampaignID(campaignID); err != nil {
		return nil, err
	}
	camp, id, err := c.queryCampaignByIDGuarded(ctx, campaignID, "identity", "", identityjson.Check)
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
	switch status {
	case StatusActive, StatusPaused, StatusBudgetPaused, StatusBudgetAndManualPaused, StatusSuspended:
		return &CampaignRef{ID: id, Name: name, Status: status}, nil
	case StatusDeleted:
		// The id names a real record, but not one a brief can be bound to — the verdict Google's
		// lookup reaches for a REMOVED campaign. A deleted campaign cannot spend, so reporting it
		// absent cannot license a duplicate of anything that is serving.
		return nil, nil
	default:
		return nil, fmt.Errorf("microsoft-ads campaign lookup: campaign %s reports a Status this client does not recognise, so whether it exists as a live campaign cannot be established", id)
	}
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
