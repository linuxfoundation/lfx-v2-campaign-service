// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Marketing-email operations (LFXV2-2779)
//
// These build on doRequest. Reads are idempotent (retry a 429); creates/clones and
// PATCHes are NOT (no idempotency key -> a retried 429 could double-create), so they
// pass idempotent=false and, on an ambiguous outcome, surface a reconcilable partial
// rather than a blind retry.
// ---------------------------------------------------------------------------

const emailsPath = "/marketing/v3/emails"

// Email is the subset of a HubSpot marketing email this client consumes.
type Email struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Subject string `json:"subject"`
	State   string `json:"state"`
	// AppURL is a human-facing edit link (built client-side, never from the API).
	AppURL string `json:"-"`
}

// emailListResponse is the shape of GET /marketing/v3/emails. HubSpot cursor-
// paginates: paging.next.after carries the next page's cursor (absent on the last).
type emailListResponse struct {
	Results []Email `json:"results"`
	Paging  *paging `json:"paging"`
}

// paging is HubSpot's standard cursor envelope (paging.next.after).
type paging struct {
	Next *struct {
		After string `json:"after"`
	} `json:"next"`
}

// maxListPages caps how many pages any paginated list-walk follows, so a portal with
// a runaway result set (or an API that never stops returning a cursor) can't loop
// unbounded. 200 pages × 100/page = 20k records, well past any realistic portal.
const maxListPages = 200

// SearchEmails returns marketing emails whose name or subject contains query
// (case-insensitive), most-recently-updated first. Read-only (idempotent). It follows
// paging.next.after across ALL pages, so a match beyond the first page is not missed.
func (c *Client) SearchEmails(ctx context.Context, query string) ([]Email, error) {
	needle := strings.ToLower(query)
	out := make([]Email, 0)
	after := ""
	for page := 0; page < maxListPages; page++ {
		q := url.Values{}
		q.Set("limit", "100")
		q.Set("sort", "-updatedAt")
		if after != "" {
			q.Set("after", after)
		}
		raw, err := c.doRequest(ctx, http.MethodGet, emailsPath+"?"+q.Encode(), nil, true)
		if err != nil {
			return nil, err
		}
		var resp emailListResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("hubspot: decode email search: %w", err)
		}
		for _, e := range resp.Results {
			if needle == "" || strings.Contains(strings.ToLower(e.Name+" "+e.Subject), needle) {
				e.AppURL = c.emailEditURL(e.ID)
				out = append(out, e)
			}
		}
		if resp.Paging == nil || resp.Paging.Next == nil || resp.Paging.Next.After == "" {
			return out, nil
		}
		after = resp.Paging.Next.After
	}
	return nil, fmt.Errorf("hubspot: SearchEmails exceeded %d pages; refusing to page unbounded", maxListPages)
}

// GetEmail fetches one marketing email by id. Read-only (idempotent).
func (c *Client) GetEmail(ctx context.Context, id string) (*Email, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("hubspot: GetEmail requires a non-empty id")
	}
	raw, err := c.doRequest(ctx, http.MethodGet, emailsPath+"/"+url.PathEscape(id), nil, true)
	if err != nil {
		return nil, err
	}
	var e Email
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("hubspot: decode email: %w", err)
	}
	if e.ID == "" {
		// A 2xx with no id is a malformed success — treat as UNCONFIRMED so a caller
		// verifies rather than assuming the email exists.
		return nil, fmt.Errorf("hubspot: GetEmail(%s) UNCONFIRMED (2xx with no id in the response)", id)
	}
	e.AppURL = c.emailEditURL(e.ID)
	return &e, nil
}

// cloneEmailRequest is the POST /marketing/v3/emails/clone body.
type cloneEmailRequest struct {
	ID        string `json:"id"`
	CloneName string `json:"cloneName"`
	Language  string `json:"language,omitempty"`
}

// CloneEmail clones sourceID into a new draft named cloneName and returns it.
// MUTATING (idempotent=false): a clone has no idempotency key, so an ambiguous
// failure must NOT blind-retry (that would create a second draft). An ambiguous
// error / a 2xx with no id is surfaced as UNCONFIRMED so the caller verifies.
func (c *Client) CloneEmail(ctx context.Context, sourceID, cloneName string) (*Email, error) {
	if strings.TrimSpace(sourceID) == "" {
		return nil, fmt.Errorf("hubspot: CloneEmail requires a non-empty source id")
	}
	body := cloneEmailRequest{ID: sourceID, CloneName: cloneName, Language: "en"}
	raw, err := c.doRequest(ctx, http.MethodPost, emailsPath+"/clone", body, false)
	if err != nil {
		return nil, fmt.Errorf("hubspot: clone email from %s: %w", sourceID, err)
	}
	var e Email
	if err := json.Unmarshal(raw, &e); err != nil {
		// A malformed/truncated 2xx body reaches here AFTER HubSpot may have already
		// created the draft. Mark it UNCONFIRMED (not a plain decode error) so the
		// caller verifies rather than blind-retrying into a duplicate clone.
		return nil, fmt.Errorf("hubspot: clone email UNCONFIRMED (2xx with an undecodable body — a draft may have been created; verify before retrying): %w", err)
	}
	if e.ID == "" {
		return nil, fmt.Errorf("hubspot: clone email UNCONFIRMED (2xx with no id — a draft may have been created; verify before retrying)")
	}
	e.AppURL = c.emailEditURL(e.ID)
	return &e, nil
}

// EmailSettings carries the subject/from/preheader fields to patch on a draft.
// Nil pointers are omitted (a HubSpot PATCH preserves omitted fields).
type EmailSettings struct {
	Subject   *string
	FromName  *string
	FromEmail *string
	Preheader *string
}

// PatchEmailSettings updates subject/from/preheader on a draft. MUTATING.
func (c *Client) PatchEmailSettings(ctx context.Context, id string, s EmailSettings) (*Email, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("hubspot: PatchEmailSettings requires a non-empty id")
	}
	payload := map[string]any{}
	if s.Subject != nil {
		payload["subject"] = *s.Subject
	}
	// from-name/from-email/preheader live under the `from`/`subscriptionDetails`
	// shape HubSpot expects on the email object.
	from := map[string]any{}
	if s.FromName != nil {
		from["name"] = *s.FromName
	}
	if s.FromEmail != nil {
		from["email"] = *s.FromEmail
	}
	if len(from) > 0 {
		payload["from"] = from
	}
	if s.Preheader != nil {
		payload["preview_text"] = *s.Preheader
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("hubspot: PatchEmailSettings called with nothing to set")
	}
	return c.patchEmail(ctx, id, payload)
}

// SetSendList sets the recipient (and suppression) lists on a draft. sendListID
// is the built master audience; suppressionListIDs are excluded. isILS selects the
// namespace: an ILS list (any CRM-v3 processingType) MUST go in contactIlsLists — a
// CRM-v3 (ILS) list id placed in contactLists makes HubSpot silently reject the
// whole `to` object, leaving the email with no recipients. Only same-namespace
// suppressions are sent; HubSpot mirrors the exclude to the other namespace itself.
// A COMPLETE `to` object is sent (contactIds cleared) so no stale clone-source
// recipients remain. MUTATING.
func (c *Client) SetSendList(ctx context.Context, id, sendListID string, suppressionListIDs []string, isILS bool) (*Email, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(sendListID) == "" {
		return nil, fmt.Errorf("hubspot: SetSendList requires a non-empty email id and send-list id")
	}
	to := map[string]any{
		// Clear individual contacts the clone source may have carried over.
		"contactIds": map[string]any{"include": []string{}, "exclude": []string{}},
	}
	suppress := cleanIDs(suppressionListIDs)
	if isILS {
		to["contactIlsLists"] = map[string]any{"include": []string{sendListID}, "exclude": suppress}
	} else {
		// Legacy lists are numeric. REJECT a non-numeric id up front — if Atoi
		// silently dropped it, the PATCH would send an empty include, and HubSpot
		// would clear the existing recipients and return success, leaving the email
		// with NO recipients (the same silent-empty failure the ILS-namespace
		// routing guards against). strconv.Atoi doesn't trim, so trim first.
		n, err := strconv.Atoi(strings.TrimSpace(sendListID))
		if err != nil {
			return nil, fmt.Errorf("hubspot: SetSendList legacy send-list id %q is not numeric (a non-numeric id would clear all recipients)", sendListID)
		}
		to["contactLists"] = map[string]any{"include": []any{n}, "exclude": suppress}
	}
	e, err := c.patchEmail(ctx, id, map[string]any{"to": to})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// patchEmail PATCHes /marketing/v3/emails/{id} and decodes the returned email.
func (c *Client) patchEmail(ctx context.Context, id string, payload map[string]any) (*Email, error) {
	raw, err := c.doRequest(ctx, http.MethodPatch, emailsPath+"/"+url.PathEscape(id), payload, false)
	if err != nil {
		return nil, fmt.Errorf("hubspot: patch email %s: %w", id, err)
	}
	var e Email
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("hubspot: decode patched email: %w", err)
	}
	if e.ID == "" {
		e.ID = id // some PATCH responses omit the id; keep the caller's
	}
	e.AppURL = c.emailEditURL(e.ID)
	return &e, nil
}

// emailEditURL builds a human-facing edit link. Empty when the portal id is unset.
func (c *Client) emailEditURL(emailID string) string {
	if c.account.PortalID == "" || emailID == "" {
		return ""
	}
	return c.appBaseURL + "/email/" + c.account.PortalID + "/edit/" + emailID + "/settings"
}

// cleanIDs trims, drops empties, and returns a non-nil slice (so an omitted list
// serializes as [] not null).
func cleanIDs(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, s := range ids {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
