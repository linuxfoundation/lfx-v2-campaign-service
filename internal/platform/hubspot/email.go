// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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
			// Match the query in name OR subject INDEPENDENTLY. Concatenating them and
			// searching the joined string would also match a query that spans the field
			// boundary (name "Sale", subject "Invite", query "e i") — a false positive.
			if needle == "" ||
				strings.Contains(strings.ToLower(e.Name), needle) ||
				strings.Contains(strings.ToLower(e.Subject), needle) {
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
	if id = strings.TrimSpace(id); id == "" {
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
		// GetEmail is an idempotent GET, so a malformed 2xx cannot leave any mutation
		// in an unconfirmed state — this is a plain malformed-response error (NOT an
		// unconfirmedError), so IsUnconfirmed stays false and the read is safely
		// retryable. (IsUnconfirmed is a mutating-outcome signal; a read can't commit.)
		return nil, fmt.Errorf("hubspot: GetEmail(%s) returned a 2xx with no id (malformed response)", id)
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
	if sourceID = strings.TrimSpace(sourceID); sourceID == "" {
		return nil, fmt.Errorf("hubspot: CloneEmail requires a non-empty source id")
	}
	// sourceID is trimmed above — a whitespace-padded id posted raw in the clone
	// body could be rejected by HubSpot, causing a silent staging failure.
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
		return nil, unconfirmed("hubspot: clone email UNCONFIRMED (2xx with an undecodable body — a draft may have been created; verify before retrying)", err)
	}
	if e.ID == "" {
		return nil, unconfirmed("hubspot: clone email UNCONFIRMED (2xx with no id — a draft may have been created; verify before retrying)", nil)
	}
	e.AppURL = c.emailEditURL(e.ID)
	return &e, nil
}

// EmailSettings carries the subject/from fields to patch on a draft. Nil pointers
// are omitted (a HubSpot PATCH preserves omitted fields).
//
// Preview/preheader text is deliberately NOT here: the Marketing Emails v3 object
// exposes no first-class preheader field (verified against HubSpot's OpenAPI spec —
// there is no `previewText` or `preview_text` property; preview text is only settable
// through an undocumented content-module path). Sending a fake field would be
// silently ignored while reporting success, so we don't offer it. Tracked for the
// content path in LFXV2-2775.
type EmailSettings struct {
	Subject   *string
	FromName  *string
	FromEmail *string
}

// PatchEmailSettings updates subject and sender (from-name / reply-to) on a draft.
// MUTATING.
func (c *Client) PatchEmailSettings(ctx context.Context, id string, s EmailSettings) (*Email, error) {
	if id = strings.TrimSpace(id); id == "" {
		return nil, fmt.Errorf("hubspot: PatchEmailSettings requires a non-empty id")
	}
	payload := map[string]any{}
	if s.Subject != nil {
		payload["subject"] = *s.Subject
	}
	// The v3 `from` object uses fromName + replyTo (verified against HubSpot's
	// PublicEmailFromDetails schema) — NOT name/email, which HubSpot ignores.
	// replyTo doubles as the from-address recipients see.
	from := map[string]any{}
	if s.FromName != nil {
		from["fromName"] = *s.FromName
	}
	if s.FromEmail != nil {
		from["replyTo"] = *s.FromEmail
	}
	if len(from) > 0 {
		payload["from"] = from
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("hubspot: PatchEmailSettings called with nothing to set")
	}
	return c.patchEmail(ctx, id, payload)
}

// SetSendList sets the recipient (ILS) send list and suppression lists on a draft.
// ilsListID is the built master audience (an ILS list id); suppressionListIDs are
// excluded.
//
// Recipients are set ONLY via contactIlsLists. HubSpot's ILS migration removed
// functional support for the legacy `contactLists` recipient field after
// 2024-10-31 (it's silently non-functional now), so this client never emits it —
// callers resolve an ILS list id from the Lists v3 API. A COMPLETE `to` object is
// sent (contactIds cleared) so no stale clone-source recipients remain. MUTATING.
func (c *Client) SetSendList(ctx context.Context, id, ilsListID string, suppressionListIDs []string) (*Email, error) {
	id = strings.TrimSpace(id)
	ilsListID = strings.TrimSpace(ilsListID)
	if id == "" || ilsListID == "" {
		return nil, fmt.Errorf("hubspot: SetSendList requires a non-empty email id and ILS send-list id")
	}
	to := map[string]any{
		// Clear individual contacts the clone source may have carried over.
		"contactIds": map[string]any{"include": []string{}, "exclude": []string{}},
		// ilsListID is trimmed above — a whitespace-padded id sent raw could be
		// rejected by HubSpot, leaving the email with no recipients.
		"contactIlsLists": map[string]any{"include": []string{ilsListID}, "exclude": cleanIDs(suppressionListIDs)},
	}
	return c.patchEmail(ctx, id, map[string]any{"to": to})
}

// patchEmail PATCHes the email's DRAFT (/marketing/v3/emails/{id}/draft) and decodes
// the returned email. The /draft sub-route stages subject/from/send-list changes on
// the unpublished draft buffer; the base /{id} route mutates the LIVE email instead,
// so draft edits must go through /draft (verified against HubSpot's v3 spec).
func (c *Client) patchEmail(ctx context.Context, id string, payload map[string]any) (*Email, error) {
	raw, err := c.doRequest(ctx, http.MethodPatch, emailsPath+"/"+url.PathEscape(id)+"/draft", payload, false)
	if err != nil {
		return nil, fmt.Errorf("hubspot: patch email %s draft: %w", id, err)
	}
	var e Email
	if err := json.Unmarshal(raw, &e); err != nil {
		// A PATCH is mutating: an undecodable 2xx body means HubSpot may already have
		// applied the change, so surface it as UNCONFIRMED (like CloneEmail) rather
		// than a plain decode error, so callers verify instead of blind-retrying.
		return nil, unconfirmed(fmt.Sprintf("hubspot: patch email %s UNCONFIRMED (2xx with an undecodable body — the update may have applied; verify before retrying)", id), err)
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
