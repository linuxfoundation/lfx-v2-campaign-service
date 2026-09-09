// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

// maxUnfilteredEmails bounds the UNFILTERED walk (an empty query — the picker's default
// first screen). Without it the default screen is the worst case: every row in the portal
// matches, so a large portal returns an enormous response or spends the orchestrator's
// deadline mid-walk and 503s, precisely on the portals that most need a picker.
//
// It applies ONLY when there is no needle. A filtered search is bounded far more generously --
// maxFilteredScan rows or maxFilteredPages pages, not 500 rows -- because truncating it early
// would answer "no such email" about an email sitting on page 50, and the callers of a lookup act
// on that absence. An unfiltered walk has no such failure mode: nothing is being looked UP, and
// the contract is already "the most recent ones".
//
// The filtered walk is not UNBOUNDED, though, and an earlier revision of this comment said it
// was. Unbounded, a query matching nothing walked the whole portal and died on the request
// deadline, so the caller saw a connection error rather than "no matches" -- trading a rare false
// absence for a guaranteed failure on exactly the large portals this exists to serve.
//
// WHICH rows the bounded walk returns depends on the server honouring `sort=-updatedAt`,
// and that dependency is real rather than hedged. An earlier revision over-collected 3×
// the cap before truncating and claimed this "kept the ordering ours"; it does not. Extra
// slack only re-sorts what was FETCHED, so if the server ignores the sort the newest rows
// can sit on a page the walk never reaches — no multiple of the cap fixes that, it only
// moves the cliff. Under a bounded walk the two guarantees are exclusive: stop early and
// you depend on server order, scan every page and you have no bound.
//
// Depending on it is the right trade HERE, and only here. This is the unfiltered default
// screen, whose contract is "recent emails to pick from" — a degraded order shows the
// user a less useful list, not a wrong answer. The client still re-sorts what it fetched
// (sortEmailsByUpdatedDesc), so the returned page is correctly ordered within itself even
// if the server's selection was not. Nothing that must be CORRECT depends on the hint: a
// filtered search, where a miss is a false absence, reads far more of them (maxFilteredScan).
const maxUnfilteredEmails = 500

// maxFilteredScan bounds how many emails a FILTERED search reads before giving up on finding
// more. Without it a query matching nothing walked every page in the portal and the request died
// on its deadline, so the picker reported a connection problem for what was really "no matches".
//
// 2000 is 20 full pages: comfortably inside a request deadline at ~0.8s per page, and far more
// than a user scanning a name-or-subject match needs, since the server returns newest-first.
//
// Bounded by PAGES as well, because a provider that ignores `limit` and returns a handful of rows
// per page would otherwise still walk to maxListPages before the row bound was reached -- the
// runaway this exists to stop.
const maxFilteredScan = 2000

// maxFilteredPages caps the same walk in pages, for the small-page case described above.
const maxFilteredPages = 20

// ErrSearchIncomplete reports that a FILTERED search hit its scan bound having matched nothing.
//
// It is deliberately an error rather than an empty result. `(nil-error, empty-slice)` states that
// the portal authoritatively holds no such email, and the caller acts on that by telling an
// operator the template does not exist — about a template that may sit on the next unread page.
// The published contract for the emails endpoint prefers a recoverable failure to that claim.
//
// Only the ZERO-match case qualifies. A bounded walk that found rows returns them: the caller has
// something true to show, and the listing is documented as bounded.
var ErrSearchIncomplete = errors.New("hubspot: email search reached its scan bound before finding any match; the result would be indistinguishable from an authoritative absence")

// SearchEmails returns marketing emails whose name or subject contains query
// (case-insensitive), most-recently-updated first. Read-only (idempotent). A FILTERED
// search follows paging.next.after across pages, so a match beyond the first page is not
// missed, up to maxFilteredScan rows or maxFilteredPages pages -- past which the walk stops
// and logs that its results may be incomplete. An UNFILTERED one (empty query) is bounded to maxUnfilteredEmails rows taken
// in server order — see that constant for why the two cases differ and what the bound does
// and does not promise.
func (c *Client) SearchEmails(ctx context.Context, query string) ([]Email, error) {
	// Trim before matching — a padded term like " kubecon " must still match
	// "KubeCon Invite" rather than silently returning no results.
	needle := strings.ToLower(strings.TrimSpace(query))
	out := make([]Email, 0)
	after := ""
	scanned := 0
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
		// name/subject/updatedAt for search + ordering (id always comes back), plus `state`
		// since LFXV2-3197: the email picker surfaces it so a caller can see that a template
		// is a draft before cloning it. It is REQUESTED rather than assumed — `Email.State`
		// decodes to "" for any field not named here, so a consumer promised a lifecycle
		// state would have received an empty string from every row.
		q["includedProperties"] = []string{"name", "subject", "updatedAt", "state"}
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
		scanned += len(resp.Results)
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
		// Two ways the walk ends: the server ran out of pages, or an unfiltered walk has
		// collected its cap. The truncation below is still reachable on the second path --
		// a page can carry `out` PAST the cap when the cap is not a page multiple -- so it
		// trims the overshoot rather than being dead code. It runs after the sort so the
		// rows dropped are the oldest of what was fetched.
		lastPage := resp.Paging == nil || resp.Paging.Next == nil || resp.Paging.Next.After == ""
		// A FILTERED walk was bounded only by maxListPages, so a query matching nothing scanned
		// the whole portal: 200 pages x ~0.8s is well past any request deadline, and the caller
		// saw `context deadline exceeded` rather than "no matches". Observed on a live portal --
		// the template picker failed to load every time while a direct one-page fetch took 0.8s.
		//
		// `scanned`, not `len(out)`: the bound has to hold when the query matches FEW rows or
		// none, which is exactly the case that ran longest. Rows are sorted newest-first by the
		// server hint, so the pages read are the ones most likely to contain what a user wants.
		enoughScanned := needle != "" && (scanned >= maxFilteredScan || page+1 >= maxFilteredPages)
		if enoughScanned && !lastPage {
			slog.WarnContext(ctx, "hubspot email search stopped at its scan bound; results may be incomplete",
				"query_len", len(needle), "scanned", scanned, "pages", page+1, "matched", len(out))

			// ZERO matches at the bound is a FALSE ABSENCE, and must not be returned as one.
			//
			// `(out, nil)` with an empty `out` says "the portal authoritatively has no such
			// email" — the published contract for this endpoint prefers a recoverable 503 over
			// exactly that claim, because the caller acts on the absence by concluding the
			// template does not exist. The warning above reaches an operator's logs; it does not
			// reach the caller, so on its own it moved the confusion rather than removing it.
			//
			// With matches IN HAND the bound is a partial answer rather than a false one: the
			// caller has real rows to show, sorted newest-first, and the endpoint documents the
			// listing as bounded. Only the empty case is a lie.
			if len(out) == 0 {
				return nil, fmt.Errorf("hubspot: email search scanned %d emails across %d pages without finding a match and could not read further: %w",
					scanned, page+1, ErrSearchIncomplete)
			}
		}
		if lastPage || enoughScanned || (needle == "" && len(out) >= maxUnfilteredEmails) {
			// SORT then trim, deliberately, and not the other way round. The trim is only
			// reachable when a page carries `out` past the cap — i.e. when the provider ignored
			// `limit`. If it ignored `limit` it may well have ignored `sort=-updatedAt` too, and
			// in that case truncating first keeps the provider's FIRST 500, which for an
			// oldest-first response is the 500 OLDEST emails: the worst possible answer for a
			// screen whose whole purpose is showing recent ones. Sorting first keeps the newest
			// 500 of what was actually read.
			//
			// The published contract describes this as "the newest of what was read" rather than
			// a prefix of the provider's order, for the same reason.
			sortEmailsByUpdatedDesc(out)
			if needle == "" && len(out) > maxUnfilteredEmails {
				out = out[:maxUnfilteredEmails]
			}
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
	suppressionIDs := cleanIDs(suppressionListIDs)
	// HubSpot applies contactIlsLists exclusions AFTER inclusions, so a send-list id
	// that also appears in the suppression set would silently exclude the ENTIRE
	// selected audience — the PATCH returns 2xx while the email ends up with zero
	// recipients. Reject that contradictory input up front (before the mutating PATCH)
	// rather than let it look like a successful send-list assignment.
	for _, s := range suppressionIDs {
		if s == ilsListID {
			return nil, fmt.Errorf("hubspot: SetSendList send-list id %q is also in the suppression list — the audience would be fully excluded", ilsListID)
		}
	}
	to := map[string]any{
		// Clear individual contacts the clone source may have carried over.
		"contactIds": map[string]any{"include": []string{}, "exclude": []string{}},
		// ilsListID is trimmed above — a whitespace-padded id sent raw could be
		// rejected by HubSpot, leaving the email with no recipients.
		"contactIlsLists": map[string]any{"include": []string{ilsListID}, "exclude": suppressionIDs},
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

// emailContent is the subset of an email's draft content this client READS. The HubSpot draft
// body lives under content.widgets.<key>.body.html, keyed by module id, so the widget map is
// decoded generically — the keys vary per template and are not ours to name.
//
// flexAreas is the DRAG_AND_DROP layout tree, and it is the only place a draft records the
// READING ORDER of its blocks. Nothing else can stand in for it: the widget map is a JSON
// object, so Go randomizes its iteration order, and the per-widget `order` field is absent on
// every drag-and-drop template observed. Without flexAreas the phrase "the first text block"
// has no meaning, and a caller asking for it would be picking a block at random.
type emailContent struct {
	Content struct {
		Widgets   map[string]json.RawMessage `json:"widgets"`
		FlexAreas map[string]flexArea        `json:"flexAreas"`
	} `json:"content"`
}

// flexArea is one drop zone of a drag-and-drop email, decoded down to the only thing this
// client asks of it: which module ids sit in which order. Every other field (styles, widths,
// box indices) is layout configuration that is read and written back verbatim as raw JSON —
// see SetEmailHTMLWidgets — never through this struct.
type flexArea struct {
	Sections []struct {
		Columns []struct {
			Widgets []string `json:"widgets"`
		} `json:"columns"`
	} `json:"sections"`
}

// widgetBody is one draft widget's body, decoded far enough to answer "is this a rich-text
// block, and what does it say".
//
// `Body` is a raw map rather than a struct with an `html` field, because the PRESENCE of the key
// is what identifies a rich-text widget. A struct cannot express that: an image module decodes
// into `struct{ HTML string }` perfectly happily, leaving HTML empty, so it is indistinguishable
// from a rich-text block whose body is blank. The two must not be conflated.
type widgetBody struct {
	Body map[string]json.RawMessage `json:"body"`
}

// html returns the widget's rich-text body and whether it is a rich-text widget at all.
//
// `ok` is key PRESENCE, not a non-empty value: an EMPTY rich-text block is still a block an
// operator can see and fill, while an image or divider carries no `html` key whatsoever.
func (w widgetBody) html() (string, bool) {
	raw, ok := w.Body["html"]
	if !ok {
		return "", false
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		// An `html` key holding something other than a string is not a rich-text body this can
		// write, so it is not counted as one either.
		return "", false
	}
	return out, true
}

// layoutOrder returns the module ids the drag-and-drop layout places, in reading order: area,
// then section top-to-bottom, then column left-to-right, then widget within the column.
//
// Areas are walked with `main` first and the rest in name order. HubSpot has only ever returned
// a single `main` area here, but a map has no order of its own, so the tie has to be broken by
// something stable — otherwise two runs against the same draft could disagree about which block
// is first.
func (ec emailContent) layoutOrder() []string {
	areas := make([]string, 0, len(ec.Content.FlexAreas))
	for name := range ec.Content.FlexAreas {
		if name != "main" {
			areas = append(areas, name)
		}
	}
	sort.Strings(areas)
	if _, ok := ec.Content.FlexAreas["main"]; ok {
		areas = append([]string{"main"}, areas...)
	}

	out := make([]string, 0, len(ec.Content.Widgets))
	for _, name := range areas {
		for _, section := range ec.Content.FlexAreas[name].Sections {
			for _, column := range section.Columns {
				out = append(out, column.Widgets...)
			}
		}
	}
	return out
}

// EmailHTMLBlock is one rich-text block of an email draft.
type EmailHTMLBlock struct {
	// Key is the widget (module) id. It is what SetEmailHTMLWidgets addresses.
	Key string
	// HTML is the block's body as the draft currently holds it. Empty is a real value, not a
	// missing one: an empty block is one an operator can see and fill.
	HTML string
}

// GetEmailHTMLWidgets returns the draft's rich-text blocks in READING ORDER.
//
// IDEMPOTENT (a GET). Every rich-text widget is returned, empty ones included, so len() is the
// number of blocks the draft has and blocks[0] is the one at the top of the email.
//
// Order comes from the drag-and-drop layout tree (content.flexAreas), never from the widget
// map, whose Go iteration order is randomized. Blocks the layout does not place — a classic
// non-drag-and-drop template places none at all — follow the placed ones in key order, so the
// result is deterministic for every template shape.
//
// A rich-text widget is identified by the PRESENCE of the `html` key, never by its value. Both
// alternatives have been wrong here:
//
//   - Omitting empty bodies UNDERCOUNTED: a template with one populated block and one empty
//     block looked like a single-block template, and it made the ONE unambiguous case
//     unaddressable — a template whose only rich-text block is empty had nothing to write into.
//   - Counting every object-bodied module OVERCOUNTED: an image decodes into the same shape with
//     an empty html field, so the ordinary template (rich text + header image) looked like two
//     blocks.
func (c *Client) GetEmailHTMLWidgets(ctx context.Context, id string) ([]EmailHTMLBlock, error) {
	if id = strings.TrimSpace(id); id == "" {
		return nil, fmt.Errorf("hubspot: GetEmailHTMLWidgets requires a non-empty id")
	}
	raw, err := c.doRequest(ctx, http.MethodGet, emailsPath+"/"+url.PathEscape(id)+"/draft", nil, true)
	if err != nil {
		return nil, err
	}
	var ec emailContent
	if uerr := json.Unmarshal(raw, &ec); uerr != nil {
		return nil, fmt.Errorf("hubspot: decode email %s draft content: %w", id, uerr)
	}
	return ec.htmlBlocks(), nil
}

// htmlBlocks selects the rich-text widgets and puts them in the layout's reading order.
func (ec emailContent) htmlBlocks() []EmailHTMLBlock {
	rich := make(map[string]string, len(ec.Content.Widgets))
	for key, rawWidget := range ec.Content.Widgets {
		var w widgetBody
		if json.Unmarshal(rawWidget, &w) != nil {
			continue // not a body-shaped widget at all
		}
		if body, isRichText := w.html(); isRichText {
			rich[key] = body
		}
	}

	out := make([]EmailHTMLBlock, 0, len(rich))
	placed := make(map[string]bool, len(rich))
	for _, key := range ec.layoutOrder() {
		body, isRichText := rich[key]
		// A layout can reference a module id the widget map does not carry — that is exactly the
		// damage a partial content write does — and a malformed tree could name one twice.
		// Neither may put a phantom or a duplicate block in the result.
		if !isRichText || placed[key] {
			continue
		}
		placed[key] = true
		out = append(out, EmailHTMLBlock{Key: key, HTML: body})
	}

	// Widgets the layout does not place: every block of a classic template, and template-level
	// modules on a drag-and-drop one. Key order is arbitrary but STABLE, which is all that is
	// available once the layout has nothing to say.
	rest := make([]string, 0, len(rich)-len(out))
	for key := range rich {
		if !placed[key] {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)
	for _, key := range rest {
		out = append(out, EmailHTMLBlock{Key: key, HTML: rich[key]})
	}
	return out
}

// SetEmailHTMLWidgets replaces the html body of the given widgets on an email's DRAFT.
// MUTATING.
//
// It READS the draft first and PATCHes the whole `content` object back with only body.html
// changed, every other byte of it re-sent verbatim as raw JSON.
//
// That round-trip is not defensive over-engineering; a partial write DESTROYS the draft. HubSpot
// does NOT merge `content` on a DRAG_AND_DROP email — the modern default, and every template
// observed in the LF portal. It treats the submitted content as authoritative: PATCHing two
// widgets of a 33-widget email left a draft holding those two widgets and nothing else, and even
// they were discarded, because they arrived without the drag-and-drop scaffolding (`path`,
// `module_id`, `schema_version`, `hs_wrapper_css`) a placed module needs. The layout tree still
// referenced all 32 placed modules, so the operator got 14 empty sections: an email with no
// content in it at all. Sending `content` back complete — flexAreas, styleSettings and
// templatePath included — is what makes "authoritative" harmless.
//
// The widgets must already exist on the draft: this writes into a block, it does not create one.
// A name the draft does not carry is an error rather than a silent no-op, since both callers name
// keys they just read from the same draft, so a miss means a real bug.
func (c *Client) SetEmailHTMLWidgets(ctx context.Context, id string, widgets map[string]string) (*Email, error) {
	if id = strings.TrimSpace(id); id == "" {
		return nil, fmt.Errorf("hubspot: SetEmailHTMLWidgets requires a non-empty id")
	}
	if len(widgets) == 0 {
		return nil, fmt.Errorf("hubspot: SetEmailHTMLWidgets requires at least one widget")
	}

	raw, err := c.doRequest(ctx, http.MethodGet, emailsPath+"/"+url.PathEscape(id)+"/draft", nil, true)
	if err != nil {
		return nil, fmt.Errorf("hubspot: read email %s draft before writing its body: %w", id, err)
	}
	// Every level is decoded one map deep into json.RawMessage, so each field this does not touch
	// is re-marshalled byte-for-byte rather than through a struct that would drop whatever it
	// never modelled.
	var doc struct {
		Content map[string]json.RawMessage `json:"content"`
	}
	if uerr := json.Unmarshal(raw, &doc); uerr != nil {
		return nil, fmt.Errorf("hubspot: decode email %s draft content: %w", id, uerr)
	}
	if len(doc.Content["widgets"]) == 0 {
		return nil, fmt.Errorf("hubspot: email %s draft carries no widgets to write into", id)
	}
	var wmap map[string]json.RawMessage
	if uerr := json.Unmarshal(doc.Content["widgets"], &wmap); uerr != nil {
		return nil, fmt.Errorf("hubspot: decode email %s draft widgets: %w", id, uerr)
	}

	for key, htmlBody := range widgets {
		merged, merr := widgetWithHTML(wmap[key], htmlBody)
		if merr != nil {
			return nil, fmt.Errorf("hubspot: email %s widget %s: %w", id, key, merr)
		}
		wmap[key] = merged
	}
	encoded, merr := json.Marshal(wmap)
	if merr != nil {
		return nil, fmt.Errorf("hubspot: encode email %s draft widgets: %w", id, merr)
	}
	doc.Content["widgets"] = encoded

	return c.patchEmail(ctx, id, map[string]any{"content": doc.Content})
}

// widgetWithHTML returns one widget's raw JSON with body.html replaced and everything else — the
// module metadata, styles and drag-and-drop scaffolding this client never models — left
// byte-identical.
func widgetWithHTML(rawWidget json.RawMessage, htmlBody string) (json.RawMessage, error) {
	if len(rawWidget) == 0 {
		return nil, errors.New("the draft has no widget by that name")
	}
	var w map[string]json.RawMessage
	if err := json.Unmarshal(rawWidget, &w); err != nil {
		return nil, fmt.Errorf("decode widget: %w", err)
	}
	// A JSON `null` decodes into a map WITHOUT error and leaves it nil, so neither the
	// len() guard above nor the error check catches it -- `"body": null` has len 4. Writing to
	// the nil map then panics, and a panic is not an error: applyEmailContent's best-effort
	// contract swallows failures, but the panic unwinds past it to the orchestrator's recover
	// and fails the WHOLE dispatch, orphaning the draft this path exists to protect.
	//
	// Reachable despite htmlBlocks filtering null-bodied widgets out, because the read and the
	// write are two separate requests: an operator editing the draft in HubSpot between them --
	// exactly what a human-reviewed draft invites -- lands the null on the write path.
	if w == nil {
		return nil, errors.New("the draft's widget is null")
	}
	body := map[string]json.RawMessage{}
	if len(w["body"]) > 0 {
		if err := json.Unmarshal(w["body"], &body); err != nil {
			return nil, fmt.Errorf("decode widget body: %w", err)
		}
		if body == nil {
			body = map[string]json.RawMessage{}
		}
	}
	encodedHTML, err := json.Marshal(htmlBody)
	if err != nil {
		return nil, fmt.Errorf("encode widget html: %w", err)
	}
	body["html"] = encodedHTML
	encodedBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode widget body: %w", err)
	}
	w["body"] = encodedBody
	return json.Marshal(w)
}
