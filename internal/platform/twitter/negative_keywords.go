// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/textfold"
)

// ---------------------------------------------------------------------------
// Line-item NEGATIVE keywords: create (LFXV2-2665)
//
// An X negative keyword is a keyword targeting criterion of the line item with operator_type NE.
// X's Campaign Management guide (https://docs.x.com/x-ads-api/campaign-management, consulted
// 2026-10-13) lists the keyword types Broad, Unordered (deprecated), Phrase and Exact and their
// negative variants Negative, Negative Phrase and Negative Exact, and states that negative
// keywords use operator_type NE; the reference's POST accounts/:account_id/targeting_criteria
// takes line_item_id, operator_type (EQ, NE: "to set negated targeting, use operator_type=NE"),
// targeting_type and targeting_value, and its own example negates a BROAD_KEYWORD
// (https://docs.x.com/x-ads-api/campaign-management/reference). The NEGATIVE_* targeting types
// of older API versions are not sent: they now come back as the positive type with NE.
// UNORDERED_KEYWORD is deprecated and not offered.
//
// ONE POST PER KEYWORD, NOT THE BATCH ENDPOINT. POST batch/accounts/:account_id/targeting_criteria
// takes up to 40 items but succeeds or fails as a group, so it cannot give each keyword its own
// outcome.
// ---------------------------------------------------------------------------

// OperatorNE negates a targeting criterion.
const OperatorNE = "NE"

// NegativeKeyword reports whether the criterion is a live, negated keyword.
func (t TargetingCriterion) NegativeKeyword() bool {
	return !t.Deleted && IsKeywordTargetingType(t.TargetingType) && t.OperatorType == OperatorNE && t.TargetingValue != ""
}

// CreateNegativeKeywordCriterion adds one negative keyword to a line item via
// POST accounts/:account_id/targeting_criteria with line_item_id, operator_type=NE,
// targeting_type and targeting_value, and returns the id X assigned. targetingType must be
// BROAD_KEYWORD, PHRASE_KEYWORD or EXACT_KEYWORD.
//
// It takes a slot on the client's shared WRITE PACER first, like every X mutation; a pacer wait
// cut short, or a request doRequest proves never left the process (ProbeNotSent), is
// ErrWriteNotSent. The POST is sent ONCE: a 429 is NOT retried in-call (X has no idempotency key
// for a criterion, so a retry after a committed write would add a second one), and
// createOutcomeAmbiguous classifies it UNCONFIRMED, as it does a transport failure, 3xx or 5xx
// (IsOutcomeUnconfirmed). Any other 4xx is a definite refusal. A 2xx is success only when it
// reports a live criterion with an id, under THIS line item, of the requested type, negated, for
// this keyword (compared with textfold, the key the dispatcher's already-present lookup uses); any
// other 2xx is UNCONFIRMED.
func (c *Client) CreateNegativeKeywordCriterion(ctx context.Context, lineItemID, targetingType, keyword string) (string, error) {
	if _, _, err := c.lineItemPath(lineItemID); err != nil {
		return "", err
	}
	lineItemID = strings.TrimSpace(lineItemID)
	switch targetingType {
	case TargetingBroadKeyword, TargetingPhraseKeyword, TargetingExactKeyword:
	default:
		return "", fmt.Errorf("twitter: negative keyword: unsupported targeting type %q", targetingType)
	}
	if strings.TrimSpace(keyword) == "" {
		return "", errors.New("twitter: negative keyword: empty keyword")
	}
	if err := c.pace(ctx); err != nil {
		return "", fmt.Errorf("twitter: create negative keyword on line item %s aborted before it was sent: %w: %w", lineItemID, ErrWriteNotSent, err)
	}
	resp, err := c.doRequest(ctx, http.MethodPost, "targeting_criteria", map[string]string{
		"line_item_id":    lineItemID,
		"operator_type":   OperatorNE,
		"targeting_type":  targetingType,
		"targeting_value": keyword,
	}, false /* sent once: a 429 is unconfirmed, never retried */)
	if err != nil {
		if ProbeNotSent(err) {
			return "", fmt.Errorf("twitter: create negative keyword on line item %s was not sent: %w: %w", lineItemID, ErrWriteNotSent, err)
		}
		return "", fmt.Errorf("twitter: create negative keyword on line item %s: %w", lineItemID, err)
	}
	unconfirmed := func(why string) error {
		return &transportError{Method: http.MethodPost, Path: "targeting criteria", err: fmt.Errorf("negative keyword on line item %s: %s", lineItemID, why)}
	}
	if resp == nil || len(resp.Data) == 0 || string(resp.Data) == "null" {
		return "", unconfirmed("acknowledged without the created criterion")
	}
	var echo targetingCriterionWire
	if jerr := json.Unmarshal(resp.Data, &echo); jerr != nil {
		return "", unconfirmed("the 2xx response is not a targeting criterion object")
	}
	tc := echo.criterion()
	switch {
	case tc.ID == "" || !accountIDRe.MatchString(tc.ID) || len(tc.ID) > maxAccountIDLen:
		return "", unconfirmed("acknowledged without a usable criterion id")
	case tc.LineItemID != lineItemID:
		return "", unconfirmed("acknowledged under another line item")
	case tc.TargetingType != targetingType || tc.OperatorType != OperatorNE || tc.Deleted:
		return "", unconfirmed("acknowledged as a criterion other than the negated keyword sent")
	case !textfold.Equal(tc.TargetingValue, keyword):
		return "", unconfirmed("acknowledged for another keyword")
	}
	return tc.ID, nil
}
