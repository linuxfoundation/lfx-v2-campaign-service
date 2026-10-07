// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// CampaignSettings is one campaign's live configuration as the settings readback reads it.
//
// Every optional field is a POINTER and nil means "Microsoft did not report it" — never a zero
// value standing in for one. The readback turns a nil into an ABSENT upstream side and an
// `unknown` verdict; a zero here would read as "the campaign has no budget" or "the name is
// empty", which is a claim the platform never made.
type CampaignSettings struct {
	// CampaignID is the id Microsoft ECHOED, already checked against the requested one.
	CampaignID string
	Name       *string
	// Status is the raw Campaign.Status (Active, Paused, BudgetPaused, Deleted, ...).
	Status *string
	// BudgetType is the raw BudgetLimitType (DailyBudgetStandard, ...).
	BudgetType *string
	// DailyBudget is the DailyBudget amount as Microsoft rendered it: a plain decimal in the AD
	// ACCOUNT's currency — not micros, not minor units, the unit the create path sends.
	DailyBudget *float64
	// Shared reports whether the campaign draws on a shared Budget entity (BudgetId set). nil when
	// the BudgetId was present but unreadable — "could not establish" is not "not shared".
	Shared *bool
	// BiddingSchemeType is BiddingScheme.Type with Microsoft's "BiddingScheme" suffix stripped,
	// so it reads in the vocabulary GetCampaignBidStrategy uses.
	BiddingSchemeType *string
}

// GetCampaignSettings reads ONE campaign's live configuration for the settings readback. It
// rides the same GetCampaignsByIds read (queryCampaignByIDGuarded) as the budget, bid and
// adoption reads, so it inherits every answer-validation rule they apply — an omitted Campaigns
// field, an unexplained null slot, a slot for a DIFFERENT id, or a PartialError of any other kind
// is an error, never an absence — plus identityjson.Check over the raw body, as the adoption
// read applies, so a body encoding/json would silently rewrite (a duplicated Id or Name) is
// refused rather than read.
//
// It is a PURE READ: retried on 429, and every failure is definite. (nil, nil) means Microsoft
// affirmatively reported no such campaign in this account (CampaignServiceInvalidCampaignId).
//
// EVERY documented CampaignType is requested, as the adoption read does: a readback must report
// the campaign the row names whatever its type, and a Search-only filter could turn a live
// non-Search campaign into an apparent absence (a 404 telling the operator it was deleted).
//
// A field that is present but of the wrong JSON kind is an ERROR rather than an absence: an
// answer this client cannot decode the way Microsoft documents it is not one it may half-read
// into a comparison.
func (c *Client) GetCampaignSettings(ctx context.Context, campaignID string) (*CampaignSettings, error) {
	if err := ValidateCampaignID(campaignID); err != nil {
		return nil, err
	}
	camp, id, err := c.queryCampaignByIDGuarded(ctx, campaignID, "settings", allCampaignTypes, "", identityjson.Check)
	if err != nil {
		return nil, fmt.Errorf("microsoft-ads campaign settings read: %w", err)
	}
	if camp == nil {
		return nil, nil
	}
	out := &CampaignSettings{CampaignID: id}
	var ok bool
	if out.Name, ok = optionalRawString(camp.Name); !ok {
		return nil, fmt.Errorf("microsoft-ads campaign settings read: campaign %s reported a Name that is not a string", id)
	}
	if out.Status, ok = optionalRawString(camp.Status); !ok {
		return nil, fmt.Errorf("microsoft-ads campaign settings read: campaign %s reported a Status that is not a string", id)
	}
	if camp.BudgetType != nil {
		bt := *camp.BudgetType
		out.BudgetType = &bt
	}
	if v, isNull, scalarOK := rawScalar(camp.DailyBudget); !scalarOK {
		return nil, fmt.Errorf("microsoft-ads campaign settings read: campaign %s reported a DailyBudget that is not a number", id)
	} else if !isNull && v != "" {
		f, perr := strconv.ParseFloat(v, 64)
		if perr != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
			return nil, fmt.Errorf("microsoft-ads campaign settings read: campaign %s reported a DailyBudget that is not a finite non-negative amount", id)
		}
		out.DailyBudget = &f
	}
	// BudgetId: null, absent or 0 is Microsoft's documented "own budget"; a positive id is a
	// shared Budget. Anything else leaves Shared nil — the same three-way reading GetCampaignBudget
	// makes, so the readback and the budget write cannot disagree about whether a budget is shared.
	switch {
	case camp.BudgetId == nil:
		f := false
		out.Shared = &f
	case strings.TrimSpace(camp.BudgetId.String()) == "0":
		f := false
		out.Shared = &f
	case numberID(camp.BudgetId) != "":
		t := true
		out.Shared = &t
	}
	if raw := strings.TrimSpace(string(camp.BiddingScheme)); raw != "" && raw != "null" {
		var scheme struct {
			Type *string `json:"Type"`
		}
		if uerr := json.Unmarshal(camp.BiddingScheme, &scheme); uerr != nil {
			return nil, fmt.Errorf("microsoft-ads campaign settings read: campaign %s reported a BiddingScheme that is not an object", id)
		}
		if scheme.Type != nil {
			t := normalizeBidSchemeType(*scheme.Type)
			out.BiddingSchemeType = &t
		}
	}
	return out, nil
}

// optionalRawString decodes a raw JSON value that, when present, must be a string. Absent or
// null is (nil, true); a string is (&s, true); any other kind is (nil, false). The decode error
// is dropped: it would quote the upstream value.
func optionalRawString(raw json.RawMessage) (*string, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil, true
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, false
	}
	return &out, true
}
