// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Strictly read-only. The one call in this file is a GAQL search (googleAds:search, POST
// but idempotent) and nothing here mutates anything.
//
// This file exists because CampaignInput.ConversionActions takes ids the caller has to get
// from somewhere. Without a read, the only source is the Google Ads UI, which means an
// operator hand-copying a numeric id into a form — and a mistyped id is not caught until the
// campaign create fails AFTER the budget mutate. Listing them lets the caller choose from
// what the account actually has.
//
// Creating a conversion action is deliberately NOT offered. A conversion action is only half
// a measurement setup: the other half is a tag on the advertiser's site or an app/call
// configuration, and a conversion action created without it records nothing while reporting
// as configured — an account-level setup step that belongs with whoever owns the site, not
// in a campaign broker.

// maxConversionActionRows caps what one list returns. A real account has tens of conversion
// actions; a four-figure response means the account is not what the caller thinks it is, and
// a picker cannot be built from it anyway.
const maxConversionActionRows = 500

// ConversionAction is one conversion action on the account, as much of it as a caller
// choosing what to optimize toward needs.
type ConversionAction struct {
	// ID is the numeric id, which is also the short spelling CampaignInput.ConversionActions
	// accepts. ResourceName is the fully-qualified form the same field accepts.
	ID           string `json:"id"`
	ResourceName string `json:"resourceName"`
	Name         string `json:"name"`
	// Status is ENABLED / PAUSED / REMOVED. Reported rather than filtered on, below.
	Status string `json:"status"`
	// Type is the measurement mechanism (WEBPAGE, UPLOAD_CLICKS, GOOGLE_PLAY_DOWNLOAD …)
	// and Category the business meaning (PURCHASE, SIGNUP, LEAD …). Both matter to the
	// person choosing: two actions can share a category and be measured completely
	// differently.
	Type     string `json:"type"`
	Category string `json:"category"`
	// PrimaryForGoal reports whether this action currently counts toward the account's
	// "Conversions" column and therefore drives bidding by default. An action with it
	// false is still selectable — naming it in selective_optimization is exactly how a
	// campaign bids toward a secondary conversion — so this is information, not a filter.
	PrimaryForGoal bool `json:"primaryForGoal"`
}

// gaqlConversionActionRow is one googleAds:search row for the query below. Google returns
// int64 proto fields as JSON strings, so ID is a string here and stays one all the way out
// — parsing it to a number only to re-render it would be a lossy round trip for no gain.
type gaqlConversionActionRow struct {
	ConversionAction struct {
		ID             string `json:"id"`
		ResourceName   string `json:"resourceName"`
		Name           string `json:"name"`
		Status         string `json:"status"`
		Type           string `json:"type"`
		Category       string `json:"category"`
		PrimaryForGoal bool   `json:"primaryForGoal"`
	} `json:"conversionAction"`
}

// conversionActionQuery selects every non-removed conversion action on the account.
//
// REMOVED is excluded in the WHERE clause rather than dropped row-side: a removed action
// cannot be attached to a campaign at all, so returning it would offer the caller a choice
// guaranteed to fail, and it would consume a slot against maxConversionActionRows. PAUSED is
// kept — a paused action is attachable, and an operator setting up a campaign ahead of
// re-enabling one has a legitimate reason to pick it.
const conversionActionQuery = `SELECT conversion_action.id, conversion_action.resource_name, conversion_action.name, conversion_action.status, conversion_action.type, conversion_action.category, conversion_action.primary_for_goal FROM conversion_action WHERE conversion_action.status != 'REMOVED' ORDER BY conversion_action.name`

// ListConversionActions returns the conversion actions configured on this client's account,
// for a caller building a picker over CampaignInput.ConversionActions.
//
// A row missing its id or resource name FAILS THE WHOLE RESPONSE rather than being skipped.
// Both are what the caller would send back on a create, so a row without them is a choice
// that cannot be acted on; and an absent id here means the SELECT and this struct have
// drifted apart, which is a fact about every row and not about one of them.
func (c *Client) ListConversionActions(ctx context.Context) ([]ConversionAction, error) {
	rows, err := c.gaqlSearch(ctx, conversionActionQuery)
	if err != nil {
		return nil, fmt.Errorf("list conversion actions: %w", err)
	}
	if len(rows) > maxConversionActionRows {
		return nil, fmt.Errorf("list conversion actions: account %s returned %d conversion actions, more than the %d this endpoint publishes", c.account.CustomerID, len(rows), maxConversionActionRows)
	}
	out := make([]ConversionAction, 0, len(rows))
	for i, raw := range rows {
		var row gaqlConversionActionRow
		if uErr := json.Unmarshal(raw, &row); uErr != nil {
			return nil, &transportError{
				Method: http.MethodPost,
				Path:   c.customerPath("googleAds:search"),
				Err:    fmt.Errorf("decode conversion action row at index %d: %w", i, uErr),
			}
		}
		ca := row.ConversionAction
		if strings.TrimSpace(ca.ID) == "" || strings.TrimSpace(ca.ResourceName) == "" {
			return nil, fmt.Errorf("list conversion actions: row %d is missing its id or resource name", i)
		}
		out = append(out, ConversionAction{
			ID:             ca.ID,
			ResourceName:   ca.ResourceName,
			Name:           ca.Name,
			Status:         ca.Status,
			Type:           ca.Type,
			Category:       ca.Category,
			PrimaryForGoal: ca.PrimaryForGoal,
		})
	}
	return out, nil
}
