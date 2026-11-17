// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// adStateFields is the field set the per-ad pre-write read asks for (#550, gap 14): the ad's own
// status and the ad set, campaign and account it is reported under, which the caller checks.
const adStateFields = "id,adset_id,campaign_id,account_id,status"

// ErrInvalidAdID marks a string that cannot name a Meta ad. Refused before any request.
var ErrInvalidAdID = errors.New("meta: invalid ad id")

// ValidateAdID reports whether id is a canonical Meta ad id: the campaign id rule (digits, no
// leading zero, at most 32). Nothing is trimmed. It contacts nothing.
func ValidateAdID(id string) error {
	if ValidateCampaignID(id) != nil {
		return ErrInvalidAdID
	}
	return nil
}

// AdState is what the per-ad pre-write read learns about one ad.
type AdState struct {
	ID         string
	AdSetID    string
	CampaignID string
	// AccountID is normalised to "act_<digits>".
	AccountID string
	// Status is the ad's configured status (ACTIVE, PAUSED, DELETED, ARCHIVED).
	Status string
}

type adStateWire struct {
	ID         *string `json:"id"`
	AdSetID    *string `json:"adset_id"`
	CampaignID *string `json:"campaign_id"`
	AccountID  *string `json:"account_id"`
	Status     *string `json:"status"`
}

// GetAdState reads one ad's identity and configured status: GET
// /{adID}?fields=id,adset_id,campaign_id,account_id,status. A pure read, so every failure is
// definite — nothing has been written. The id must be echoed, and adset_id, campaign_id,
// account_id and status must all be present and well-formed; the caller decides whether they are
// the ones it expects.
func (c *Client) GetAdState(ctx context.Context, adID string) (*AdState, error) {
	if err := ValidateAdID(adID); err != nil {
		return nil, err
	}
	raw, err := c.getChecked(ctx, "/"+adID+"?fields="+adStateFields)
	if err != nil {
		return nil, fmt.Errorf("read meta ad %s: %w", adID, err)
	}
	var w adStateWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("read meta ad %s: the response is not an ad object", adID)
	}
	if w.ID == nil || *w.ID != adID {
		return nil, fmt.Errorf("read meta ad %s: the response does not describe the requested ad", adID)
	}
	if w.AdSetID == nil || ValidateAdSetID(*w.AdSetID) != nil {
		return nil, fmt.Errorf("read meta ad %s: the response carries no canonical adset_id", adID)
	}
	if w.CampaignID == nil || ValidateCampaignID(*w.CampaignID) != nil {
		return nil, fmt.Errorf("read meta ad %s: the response carries no canonical campaign_id", adID)
	}
	account := ""
	if w.AccountID != nil {
		account = canonicalAccountID(*w.AccountID)
	}
	if account == "" {
		return nil, fmt.Errorf("read meta ad %s: the response carries no readable account_id", adID)
	}
	if w.Status == nil || !statusTokenRE.MatchString(*w.Status) {
		return nil, fmt.Errorf("read meta ad %s: the response carries no readable status", adID)
	}
	return &AdState{ID: adID, AdSetID: *w.AdSetID, CampaignID: *w.CampaignID, AccountID: account, Status: *w.Status}, nil
}

// UpdateAdStatusOnce sets an ad's status to ACTIVE or PAUSED with ONE POST /{adID} {"status": …},
// exactly as UpdateAdSetStatusOnce does for an ad set: sent once (no throttle retry), and a 2xx
// that does not confirm success is unconfirmed. Classify the error with ClassifyAdSetWrite, which
// reads the same Graph error envelope.
func (c *Client) UpdateAdStatusOnce(ctx context.Context, adID, status string) error {
	if err := ValidateAdID(adID); err != nil {
		return err
	}
	return c.updateStatusOnce(ctx, "ad", adID, status)
}

// updateStatusOnce is the single confirmed status POST shared by UpdateAdSetStatusOnce and
// UpdateAdStatusOnce. id must already be validated.
func (c *Client) updateStatusOnce(ctx context.Context, kind, id, status string) error {
	if status != StatusActive && status != StatusPaused {
		return fmt.Errorf("meta: status must be %q or %q", StatusActive, StatusPaused)
	}
	path := "/" + id
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodPost, path, map[string]any{"status": status}, &raw, false); err != nil {
		return fmt.Errorf("meta: update %s %s status to %s: %w", kind, id, status, err)
	}
	// The confirmation is checked as RAW BYTES first: {"success":false,"Success":true} decodes to
	// true under encoding/json's last-wins, case-insensitive matching. A body that cannot be
	// trusted confirms nothing, so it is unconfirmed, never applied.
	if err := identityjson.Check(raw); err != nil {
		return &transportError{Method: http.MethodPost, Path: path, Err: err}
	}
	var resp struct {
		Success *bool `json:"success"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.Success == nil || !*resp.Success {
		// Meta answered 2xx without confirming. It received the request, so the outcome is
		// unknown — never reported as success, never as "nothing changed".
		return &transportError{Method: http.MethodPost, Path: path, Err: errors.New("2xx response did not confirm success")}
	}
	return nil
}
