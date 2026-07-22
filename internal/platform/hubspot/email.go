// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
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
	// UpdatedAt is the last-modified timestamp (ISO-8601). Used only to order
	// SearchEmails results most-recently-updated first — sortEmailsByUpdatedDesc PARSES
	// it (a raw lexical compare is unreliable when offsets or fractional precision
	// differ).
	UpdatedAt string `json:"updatedAt"`
	// AppURL is a human-facing edit link (built client-side, never from the API).
	AppURL string `json:"-"`
}

// sortEmailsByUpdatedDesc orders emails most-recently-updated first, in place. The
// updatedAt values are PARSED as RFC3339 timestamps before comparing — a raw lexical
// compare is wrong because equivalent instants can carry different offsets and optional
// fractional seconds (e.g. `2026-01-01T00:30:00+01:00` is OLDER than
// `2026-01-01T00:00:00Z` but sorts lexically after it). A missing/malformed timestamp is
// treated as the zero time (sorts last), and ties fall back to the id for determinism.
func sortEmailsByUpdatedDesc(emails []Email) {
	parsed := func(s string) time.Time {
		// RFC3339Nano parses BOTH plain and subsecond timestamps (HubSpot sends
		// millisecond `.000Z` values); plain RFC3339 would fail on those and treat a
		// valid timestamp as the zero time, corrupting the order.
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return time.Time{}
		}
		return t
	}
	sort.SliceStable(emails, func(i, j int) bool {
		ti, tj := parsed(emails[i].UpdatedAt), parsed(emails[j].UpdatedAt)
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return emails[i].ID > emails[j].ID
	})
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
	// Trim before matching — a padded term like " kubecon " must still match
	// "KubeCon Invite" rather than silently returning no results.
	needle := strings.ToLower(strings.TrimSpace(query))
	out := make([]Email, 0)
	after := ""
	for page := 0; page < maxListPages; page++ {
		q := url.Values{}
		q.Set("limit", "100")
		// `sort` IS a valid GET /marketing/v3/emails param (verified against HubSpot's
		// v3 docs) — request most-recently-updated first as a server hint. We STILL
		// re-sort client-side (sortEmailsByUpdatedDesc, below) as the guarantee, because
		// the aggregated multi-page result must be ordered as a whole and mixed
		// offsets/fractional seconds need a parsed comparison.
		q.Set("sort", "-updatedAt")
		// Restrict the returned fields: the list endpoint returns FULL email content by
		// default, so at limit=100 rich templates can blow past the client's response
		// cap. The marketing-emails list endpoint uses REPEATED `includedProperties`
		// entries (not a CRM-style comma-separated `properties` string). We only need
		// name/subject/updatedAt for search + ordering (id always comes back).
		q["includedProperties"] = []string{"name", "subject", "updatedAt"}
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
		// A malformed 2xx body such as `{}` or `null` decodes with Results==nil (a
		// genuinely empty portal returns `{"results":[]}`, which is non-nil). A missing
		// results array is malformed on ANY page — on a LATER page it would otherwise
		// silently end the walk and return a TRUNCATED result. Treat nil Results as a
		// decode error regardless of page.
		if resp.Results == nil {
			return nil, fmt.Errorf("hubspot: email search returned a 2xx with no results array (malformed response)")
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
			sortEmailsByUpdatedDesc(out)
			return out, nil
		}
		// `paging.next.after` is an OPAQUE token from the JSON body — a JSON string field
		// is NOT percent-encoded, so it arrives as the server's raw value and must be
		// forwarded VERBATIM. url.Values.Encode below applies exactly one round of
		// percent-encoding on the wire, which the server decodes once back to this raw
		// token; pre-decoding it here would corrupt any token carrying a literal `%` (and
		// diverges from the verbatim handling in the linkedin/googleads cursor walks).
		next := resp.Paging.Next.After
		// A non-advancing cursor (HubSpot or a proxy echoing the same raw `after`) would
		// otherwise re-fetch the same page every iteration, duplicating results until the
		// page cap. Refuse to loop on it — the raw-to-raw compare is exact.
		if next == after {
			return nil, fmt.Errorf("hubspot: SearchEmails cursor did not advance (repeated after token)")
		}
		after = next
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
	// Value decode (not the *Email-pointer pattern patchEmail uses to detect a null
	// body): here the `e.ID == ""` check below already covers a JSON `null` body — it
	// unmarshals to a zero-valued Email whose ID is "" — so a separate nil check would
	// be redundant.
	var e Email
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("hubspot: decode email: %w", err)
	}
	if e.ID == "" {
		// GetEmail is an idempotent GET, so a malformed 2xx (incl. a `null` body) cannot
		// leave any mutation in an unconfirmed state — this is a plain malformed-response
		// error (NOT an unconfirmedError), so IsUnconfirmed stays false and the read is
		// safely retryable. (IsUnconfirmed is a mutating-outcome signal; a read can't commit.)
		return nil, fmt.Errorf("hubspot: GetEmail(%s) returned a 2xx with no id (malformed response)", id)
	}
	e.AppURL = c.emailEditURL(e.ID)
	return &e, nil
}

// cloneEmailRequest is the POST /marketing/v3/emails/clone body. NOTE: no `language`
// field — omitting it makes HubSpot preserve the SOURCE draft's locale, which is the
// faithful-clone behavior this method promises. (A field defaulting to "en" would
// silently re-language a non-English source; a never-populated `language,omitempty`
// field would just be dead code, so it's left off entirely.)
type cloneEmailRequest struct {
	ID        string `json:"id"`
	CloneName string `json:"cloneName"`
}

// CloneEmail clones sourceID into a new draft named cloneName and returns it.
// MUTATING (idempotent=false): a clone has no idempotency key, so an ambiguous
// failure must NOT blind-retry (that would create a second draft). An ambiguous
// error / a 2xx with no id is surfaced as UNCONFIRMED so the caller verifies.
func (c *Client) CloneEmail(ctx context.Context, sourceID, cloneName string) (*Email, error) {
	if sourceID = strings.TrimSpace(sourceID); sourceID == "" {
		return nil, fmt.Errorf("hubspot: CloneEmail requires a non-empty source id")
	}
	if cloneName = strings.TrimSpace(cloneName); cloneName == "" {
		return nil, fmt.Errorf("hubspot: CloneEmail requires a non-empty clone name")
	}
	// sourceID/cloneName are trimmed above — a whitespace-padded id posted raw could
	// be rejected by HubSpot (a silent staging failure), and a padded name would
	// produce a misnamed draft (CreateList normalizes names the same way). No language
	// is sent (see cloneEmailRequest) so HubSpot preserves the source draft's locale.
	body := cloneEmailRequest{ID: sourceID, CloneName: cloneName}
	raw, err := c.doRequest(ctx, http.MethodPost, emailsPath+"/clone", body, false)
	if err != nil {
		return nil, fmt.Errorf("hubspot: clone email from %s: %w", sourceID, err)
	}
	// Value decode (not the *Email-pointer pattern patchEmail uses): the `e.ID == ""`
	// check below already covers a JSON `null` body (it unmarshals to a zero-valued
	// Email), and a null body is treated as UNCONFIRMED there — same as a no-id body —
	// so a separate nil check would be redundant.
	var e Email
	if err := json.Unmarshal(raw, &e); err != nil {
		// A malformed/truncated 2xx body reaches here AFTER HubSpot may have already
		// created the draft. Mark it UNCONFIRMED (not a plain decode error) so the
		// caller verifies rather than blind-retrying into a duplicate clone.
		return nil, unconfirmed("hubspot: clone email UNCONFIRMED (2xx with an undecodable body — a draft may have been created; verify before retrying)", err)
	}
	if e.ID == "" {
		return nil, unconfirmed("hubspot: clone email UNCONFIRMED (2xx with no id or a null body — a draft may have been created; verify before retrying)", nil)
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
	// Decode into a POINTER: a JSON `null` body unmarshals into a *Email as nil
	// WITHOUT error (Go's null-into-pointer semantics), which is exactly how we detect
	// it — cleaner than string-matching the raw bytes. A PATCH is mutating, so a null
	// body means the update MAY have applied; surface it as UNCONFIRMED (verify, don't
	// blind-retry) rather than as a phantom success via the id-fallback below.
	var e *Email
	if err := json.Unmarshal(raw, &e); err != nil {
		// An undecodable 2xx body: same UNCONFIRMED treatment (the change may have landed).
		return nil, unconfirmed(fmt.Sprintf("hubspot: patch email %s UNCONFIRMED (2xx with an undecodable body — the update may have applied; verify before retrying)", id), err)
	}
	if e == nil {
		return nil, unconfirmed(fmt.Sprintf("hubspot: patch email %s UNCONFIRMED (2xx with a null body — the update may have applied; verify before retrying)", id), nil)
	}
	if e.ID == "" {
		e.ID = id // some PATCH responses omit the id; keep the caller's
	}
	e.AppURL = c.emailEditURL(e.ID)
	return e, nil
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
