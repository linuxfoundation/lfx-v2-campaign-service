// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Line-item keyword targeting: list + delete (LFXV2-2665)
//
// WHERE AN X KEYWORD LIVES. As a TARGETING CRITERION of the line item: one entity per keyword,
// with its own id, a targeting_type (BROAD_KEYWORD, PHRASE_KEYWORD, EXACT_KEYWORD,
// UNORDERED_KEYWORD), the keyword itself as targeting_value, and an operator_type (EQ, or NE for
// a negative keyword). Sources, consulted 2026-10-06: the X Ads API v12 Campaign Management
// reference, https://docs.x.com/x-ads-api/campaign-management/reference (GET and DELETE
// accounts/:account_id/targeting_criteria[/:targeting_criterion_id]; the page is too long to be
// fetched whole, so its targeting section was confirmed through the page's search index) and X's
// official Python SDK, https://github.com/xdevplatform/twitter-python-ads-sdk
// (twitter_ads/campaign.py: TargetingCriteria's RESOURCE_COLLECTION
// /accounts/{account_id}/targeting_criteria, RESOURCE .../targeting_criteria/{id}, its properties
// id, line_item_id, operator_type, targeting_type, targeting_value, deleted, and
// LineItem.targeting_criteria listing with line_item_ids=[self.id]).
//
// THE CREATE PATH SETS NO TARGETING CRITERIA: CreateCampaign creates the campaign, the line item
// and the promoted tweet and never calls targeting_criteria. So a line item this service created
// has keyword criteria only if an operator added them in X Ads Manager.
//
// ONE DELETE PER CRITERION, NOT THE BATCH ENDPOINT. X also offers
// POST batch/accounts/:account_id/targeting_criteria, but its size limit and whether a batch is
// applied atomically could not be established from the sources available (they disagree), and a
// per-criterion DELETE gives each removal its own, honest outcome.
// ---------------------------------------------------------------------------

// Keyword targeting types X reports on a targeting criterion.
const (
	TargetingBroadKeyword     = "BROAD_KEYWORD"
	TargetingPhraseKeyword    = "PHRASE_KEYWORD"
	TargetingExactKeyword     = "EXACT_KEYWORD"
	TargetingUnorderedKeyword = "UNORDERED_KEYWORD"
	// OperatorEQ is a positive criterion; NE negates it.
	OperatorEQ = "EQ"
)

// IsKeywordTargetingType reports whether t is one of X's keyword targeting types.
func IsKeywordTargetingType(t string) bool {
	switch t {
	case TargetingBroadKeyword, TargetingPhraseKeyword, TargetingExactKeyword, TargetingUnorderedKeyword:
		return true
	default:
		return false
	}
}

// ErrInvalidTargetingCriterionID marks a criterion id the client refuses before any request.
var ErrInvalidTargetingCriterionID = errors.New("twitter: targeting criterion id must be non-empty and contain only letters and digits")

// ErrTargetingCriterionNotFound marks a DELETE X answered 404: it holds no such live criterion.
// Definite — this request changed nothing.
var ErrTargetingCriterionNotFound = errors.New("twitter: no such live targeting criterion")

// ErrTargetingUnreadable marks a targeting-criteria list that could not be read completely and
// unambiguously. Refused, never treated as "no keywords": the caller decides ownership and the
// last-keyword guard from this list.
var ErrTargetingUnreadable = errors.New("twitter: the line item's targeting criteria could not be read completely")

// ErrWriteNotSent marks a mutation abandoned BEFORE it was sent (the write pacer's wait was cut
// short). Definite — nothing reached X.
var ErrWriteNotSent = errors.New("twitter: the request was not sent")

// TargetingCriterion is one targeting criterion as X reported it.
type TargetingCriterion struct {
	ID            string
	LineItemID    string
	TargetingType string
	// TargetingValue is the value when X reported a JSON string; "" otherwise.
	TargetingValue string
	OperatorType   string
	Deleted        bool
}

// PositiveKeyword reports whether the criterion is a live, non-negated keyword.
func (t TargetingCriterion) PositiveKeyword() bool {
	return !t.Deleted && IsKeywordTargetingType(t.TargetingType) && t.OperatorType == OperatorEQ && t.TargetingValue != ""
}

type targetingCriterionWire struct {
	ID             string          `json:"id"`
	LineItemID     string          `json:"line_item_id"`
	TargetingType  string          `json:"targeting_type"`
	TargetingValue json.RawMessage `json:"targeting_value"`
	OperatorType   string          `json:"operator_type"`
	Deleted        bool            `json:"deleted"`
}

func (w targetingCriterionWire) criterion() TargetingCriterion {
	out := TargetingCriterion{
		ID:            strings.TrimSpace(w.ID),
		LineItemID:    strings.TrimSpace(w.LineItemID),
		TargetingType: strings.TrimSpace(w.TargetingType),
		OperatorType:  strings.TrimSpace(w.OperatorType),
		Deleted:       w.Deleted,
	}
	if v := bytes.TrimSpace(w.TargetingValue); len(v) > 0 && v[0] == '"' {
		var s string
		if json.Unmarshal(v, &s) == nil {
			out.TargetingValue = s
		}
	}
	return out
}

// ListLineItemTargetingCriteria lists every live targeting criterion of one line item via
// GET accounts/:account_id/targeting_criteria?line_item_ids=…&with_deleted=false, following
// next_cursor. A PURE READ, so its failure is always definite.
//
// The list is all-or-error, because the caller proves ownership and enforces the last-keyword
// guard from it: a body with no result set, a full page without a cursor X gives a meaning to,
// the page cap reached, an element without a usable id, or one reported under ANOTHER line item
// (the scope did not hold) are all ErrTargetingUnreadable — never a shorter list.
func (c *Client) ListLineItemTargetingCriteria(ctx context.Context, lineItemID string) ([]TargetingCriterion, error) {
	if _, _, err := c.lineItemPath(lineItemID); err != nil {
		return nil, err
	}
	lineItemID = strings.TrimSpace(lineItemID)
	path := "targeting_criteria?line_item_ids=" + url.QueryEscape(lineItemID) + "&with_deleted=false&count=" + strconv.Itoa(listPageSize)
	out := []TargetingCriterion{}
	cursor := ""
	for page := 0; page < maxListPages; page++ {
		resp, err := c.requestPage(ctx, path, cursor)
		if err != nil {
			return nil, fmt.Errorf("twitter: list line item %s targeting criteria: %w", lineItemID, err)
		}
		if resp == nil {
			return nil, fmt.Errorf("twitter: list line item %s targeting criteria: empty response: %w", lineItemID, ErrTargetingUnreadable)
		}
		var items []targetingCriterionWire
		if len(resp.Data) > 0 {
			if err := json.Unmarshal(resp.Data, &items); err != nil {
				return nil, fmt.Errorf("twitter: list line item %s targeting criteria: the response is not a list: %w", lineItemID, ErrTargetingUnreadable)
			}
		}
		// Absent and null both leave items nil; only a present [] is an authoritative "none".
		if items == nil {
			return nil, fmt.Errorf("twitter: list line item %s targeting criteria: the response carried no result set: %w", lineItemID, ErrTargetingUnreadable)
		}
		for _, w := range items {
			tc := w.criterion()
			if tc.ID == "" || !accountIDRe.MatchString(tc.ID) {
				return nil, fmt.Errorf("twitter: list line item %s targeting criteria: an element carries no usable id: %w", lineItemID, ErrTargetingUnreadable)
			}
			if tc.LineItemID != lineItemID {
				return nil, fmt.Errorf("twitter: list line item %s targeting criteria: criterion %s is reported under another line item: %w", lineItemID, tc.ID, ErrTargetingUnreadable)
			}
			out = append(out, tc)
		}
		switch cursorVerdict(resp) {
		case cursorUnknowable:
			if len(items) >= listPageSize {
				return nil, fmt.Errorf("twitter: list line item %s targeting criteria: a full page carried no usable next_cursor: %w", lineItemID, ErrTargetingUnreadable)
			}
			return out, nil
		case cursorExhausted:
			return out, nil
		case cursorMore:
			cursor = resp.NextCursor
		}
	}
	return nil, fmt.Errorf("twitter: list line item %s targeting criteria: more than %d pages: %w", lineItemID, maxListPages, ErrTargetingUnreadable)
}

// DeleteTargetingCriterion deletes one targeting criterion via
// DELETE accounts/:account_id/targeting_criteria/:id. It takes a slot on the client's shared
// WRITE PACER first, like every other X mutation; a pacer wait cut short is ErrWriteNotSent, and
// so is a request doRequest proves never left the process (ProbeNotSent: a DNS or connect-time
// failure, or a context already done when the request was about to be built).
//
// THE 429 IS NOT RETRIED IN-CALL, for UpdateLineItemBid's reason: a throttle may be reported
// after X applied the write, so every 429 comes back as itself and createOutcomeAmbiguous
// classifies it UNCONFIRMED for a mutating method. A transport failure, 3xx or 5xx is
// UNCONFIRMED too (IsOutcomeUnconfirmed). A 404 is ErrTargetingCriterionNotFound; any other 4xx a
// definite refusal. A 2xx is APPLIED only when it names this criterion and reports it deleted;
// any other 2xx is UNCONFIRMED.
func (c *Client) DeleteTargetingCriterion(ctx context.Context, criterionID string) error {
	accountID := strings.TrimSpace(c.account.AccountID)
	if accountID == "" || !accountIDRe.MatchString(accountID) || len(accountID) > maxAccountIDLen {
		return fmt.Errorf("twitter: delete targeting criterion: %w", ErrInvalidAccountID)
	}
	criterionID = strings.TrimSpace(criterionID)
	if !accountIDRe.MatchString(criterionID) || len(criterionID) > maxAccountIDLen {
		return ErrInvalidTargetingCriterionID
	}
	if err := c.pace(ctx); err != nil {
		return fmt.Errorf("twitter: delete targeting criterion %s aborted before it was sent: %w: %w", criterionID, ErrWriteNotSent, err)
	}
	resp, err := c.doRequest(ctx, http.MethodDelete, "targeting_criteria/"+url.PathEscape(criterionID), nil,
		false /* a 429 is unconfirmed, never retried: see above */)
	if err != nil {
		// Checked before any API classification: doRequest marks a DNS or connect-time failure,
		// and a context already done at entry, as a request PROVEN never to have left the
		// process. That is NOT_SENT, not a platform refusal.
		if ProbeNotSent(err) {
			return fmt.Errorf("twitter: delete targeting criterion %s was not sent: %w: %w", criterionID, ErrWriteNotSent, err)
		}
		var ae *apiError
		if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
			return fmt.Errorf("twitter: delete targeting criterion %s: %w", criterionID, ErrTargetingCriterionNotFound)
		}
		return fmt.Errorf("twitter: delete targeting criterion %s: %w", criterionID, err)
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return &transportError{Method: http.MethodDelete, Path: "targeting criterion", err: fmt.Errorf("delete of targeting criterion %s was acknowledged without the deleted criterion", criterionID)}
	}
	var echo targetingCriterionWire
	if jerr := json.Unmarshal(resp.Data, &echo); jerr != nil {
		return &transportError{Method: http.MethodDelete, Path: "targeting criterion", err: fmt.Errorf("decode 2xx delete response for targeting criterion %s: not a targeting criterion object", criterionID)}
	}
	if got := strings.TrimSpace(echo.ID); got != criterionID {
		return &transportError{Method: http.MethodDelete, Path: "targeting criterion", err: fmt.Errorf("delete of targeting criterion %s was acknowledged for another criterion", criterionID)}
	}
	if !echo.Deleted {
		return &transportError{Method: http.MethodDelete, Path: "targeting criterion", err: fmt.Errorf("delete of targeting criterion %s was acknowledged without deleted=true", criterionID)}
	}
	return nil
}
