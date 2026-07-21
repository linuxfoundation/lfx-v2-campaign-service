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
// CRM contact-list + event-definition operations (LFXV2-2780)
//
// Lists are how the audience build (LFXV2-2774) materializes inclusion,
// suppression, and master audiences in HubSpot. `filterBranch` is passed through
// as opaque JSON (the caller/audience-builder owns its shape — HubSpot rejects
// AND-roots and nested-ORs, and requires filterType "IN_LIST" not "LIST_MEMBERSHIP"
// in membership branches; those invariants live with the builder, not this client).
// ---------------------------------------------------------------------------

const (
	listsPath        = "/crm/v3/lists"
	listSearchPath   = "/crm/v3/lists/search"
	eventDefsPath    = "/events/v3/event-definitions"
	contactObjectTID = "0-1" // HubSpot object type id for contacts
)

// List is the subset of a HubSpot contact list this client surfaces.
//
// Membership size comes back in TWO different shapes depending on the endpoint:
//   - GET / CREATE (PublicObjectList) expose a TOP-LEVEL integer `size` (TopLevelSize);
//   - SEARCH hits have no top-level size — they expose `hs_list_size` as a STRING
//     under `additionalProperties`, and only when requested.
//
// Size normalizes both (resolveSize): the top-level integer wins when present, else
// the additionalProperties string is parsed.
type List struct {
	ListID         string `json:"listId"`
	Name           string `json:"name"`
	ProcessingType string `json:"processingType"`
	// ObjectTypeID is the list's member object type ("0-1" = contacts). It is a
	// RESPONSE property on each search hit (the v3 ListSearchRequest body has no
	// objectTypeId field), so SearchLists filters on it client-side.
	ObjectTypeID string `json:"objectTypeId"`
	// Size is the normalized membership count (see resolveSize). Not decoded directly.
	Size int `json:"-"`
	// TopLevelSize decodes the GET/CREATE `size` integer (absent on search hits).
	TopLevelSize *int `json:"size,omitempty"`
	// AdditionalProperties captures the requested extra props (e.g. hs_list_size on
	// search hits), which HubSpot returns as string values.
	AdditionalProperties map[string]string `json:"additionalProperties,omitempty"`
	// FilterBranch is only populated by GetList (includeFilters=true).
	FilterBranch json.RawMessage `json:"filterBranch,omitempty"`
	// AppURL is a human-facing link (built client-side, never from the API).
	AppURL string `json:"-"`
}

// hsListSizeProp is the additionalProperties key HubSpot uses for a search hit's
// membership size (a decimal string).
const hsListSizeProp = "hs_list_size"

// resolveSize sets Size from whichever shape the endpoint returned: the top-level
// integer `size` (GET/CREATE) if present, else the `hs_list_size` string under
// additionalProperties (SEARCH). A missing/unparseable value leaves Size at 0.
func (l *List) resolveSize() {
	if l.TopLevelSize != nil {
		l.Size = *l.TopLevelSize
		return
	}
	if s, ok := l.AdditionalProperties[hsListSizeProp]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
			l.Size = n
		}
	}
}

// SearchLists returns ALL contact lists whose name matches query. Read-only. The
// lists search API paginates with offset/hasMore, so this follows every page rather
// than returning only the first — otherwise a portal with >count matching lists would
// silently return an incomplete result despite the all-matches contract.
func (c *Client) SearchLists(ctx context.Context, query string) ([]List, error) {
	const pageSize = 100
	// Trim before forwarding — a padded term would otherwise fail to match names it
	// should, silently returning no lists.
	query = strings.TrimSpace(query)
	out := make([]List, 0)
	seen := make(map[string]struct{})
	offset := 0
	for page := 0; page < maxListPages; page++ {
		body := map[string]any{
			"query":  query,
			"count":  pageSize,
			"offset": offset,
			// objectTypeId is NOT a valid ListSearchRequest body field (it's a response
			// property on each hit) — HubSpot would ignore it and return company/deal/
			// custom lists too, so we filter to contacts (0-1) client-side below.
			// `includeFilters` is likewise a GET-single-list field, not a search field.
			// Request hs_list_size so the membership count comes back.
			"additionalProperties": []string{hsListSizeProp},
		}
		raw, err := c.doRequest(ctx, http.MethodPost, listSearchPath, body, true)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Lists   []List `json:"lists"`
			HasMore bool   `json:"hasMore"`
			Offset  int    `json:"offset"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("hubspot: decode list search: %w", err)
		}
		// Track whether this page introduced any list id we hadn't already collected.
		// A page that repeats only ids we've seen means the server is looping (same
		// rows across requests) — returning success would hand back duplicates, so we
		// error like the cursor paginators do on a stuck cursor.
		newThisPage := 0
		for i := range resp.Lists {
			id := resp.Lists[i].ListID
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			newThisPage++
			// Contact-only contract enforced client-side: the search body can't
			// constrain the object type, so drop any non-contact (company/deal/custom)
			// list the server returned for the same name.
			if resp.Lists[i].ObjectTypeID != contactObjectTID {
				continue
			}
			resp.Lists[i].resolveSize()
			resp.Lists[i].AppURL = c.listURL(resp.Lists[i].ListID)
			out = append(out, resp.Lists[i])
		}
		// Done when the server says there's no more.
		if !resp.HasMore {
			return out, nil
		}
		// hasMore=true but an empty page means the server can't advance us to the
		// remaining results — returning `out` here would be a SILENT partial, which
		// this all-or-error contract refuses (a truncated list under-targets an
		// audience). Surface it as an error instead.
		if len(resp.Lists) == 0 {
			return nil, fmt.Errorf("hubspot: SearchLists got an empty page with hasMore=true (cannot complete)")
		}
		// hasMore=true but a non-empty page added no NEW list ids: the server is
		// repeating a page it already served. Refuse to loop on it (would duplicate
		// results), consistent with the cursor paginators' stuck-cursor guard.
		if newThisPage == 0 {
			return nil, fmt.Errorf("hubspot: SearchLists received a repeated page (no new list ids) with hasMore=true")
		}
		next := resp.Offset
		if next <= offset {
			// Defensive: a non-advancing server offset would otherwise loop forever;
			// advance past the rows we just consumed.
			next = offset + len(resp.Lists)
		}
		offset = next
	}
	return nil, fmt.Errorf("hubspot: SearchLists exceeded %d pages; refusing to page unbounded", maxListPages)
}

// GetList fetches one list, including its filterBranch and processingType.
// (processingType is retained on the returned List for callers; SetSendList itself is
// ILS-only and does not route on it — the legacy-vs-ILS branch was removed.) Read-only.
func (c *Client) GetList(ctx context.Context, listID string) (*List, error) {
	if listID = strings.TrimSpace(listID); listID == "" {
		return nil, fmt.Errorf("hubspot: GetList requires a non-empty list id")
	}
	q := url.Values{}
	q.Set("includeFilters", "true")
	raw, err := c.doRequest(ctx, http.MethodGet, listsPath+"/"+url.PathEscape(listID)+"?"+q.Encode(), nil, true)
	if err != nil {
		return nil, err
	}
	return c.decodeListEnvelope(raw, "get")
}

// createListRequest is the POST /crm/v3/lists/ body for a DYNAMIC contact list.
type createListRequest struct {
	Name           string          `json:"name"`
	ObjectTypeID   string          `json:"objectTypeId"`
	ProcessingType string          `json:"processingType"`
	FilterBranch   json.RawMessage `json:"filterBranch"`
}

// CreateList creates a DYNAMIC contact list from an opaque filterBranch. MUTATING
// (idempotent=false): a create has no idempotency key, so an ambiguous failure /
// 2xx-with-no-id is surfaced as UNCONFIRMED rather than blind-retried (which would
// create a duplicate list).
func (c *Client) CreateList(ctx context.Context, name string, filterBranch json.RawMessage) (*List, error) {
	if name = strings.TrimSpace(name); name == "" {
		return nil, fmt.Errorf("hubspot: CreateList requires a non-empty name")
	}
	if len(filterBranch) == 0 {
		return nil, fmt.Errorf("hubspot: CreateList requires a filterBranch")
	}
	body := createListRequest{
		Name:           name,
		ObjectTypeID:   contactObjectTID,
		ProcessingType: "DYNAMIC",
		FilterBranch:   filterBranch,
	}
	raw, err := c.doRequest(ctx, http.MethodPost, listsPath+"/", body, false)
	if err != nil {
		return nil, fmt.Errorf("hubspot: create list %q: %w", name, err)
	}
	l, derr := c.decodeListEnvelope(raw, "create")
	if derr != nil {
		// A 2xx create with no parseable listId is UNCONFIRMED: HubSpot may have
		// created the list, so a caller must verify by name rather than blind-retry
		// (which would duplicate it).
		return nil, unconfirmed(fmt.Sprintf("hubspot: create list %q UNCONFIRMED (a list may have been created; verify before retrying)", name), derr)
	}
	return l, nil
}

// UpdateListFilters replaces the filterBranch on an existing list. MUTATING
// (a PUT replace is not idempotent-retriable here: a 429 mid-replace could apply).
func (c *Client) UpdateListFilters(ctx context.Context, listID string, filterBranch json.RawMessage) error {
	if listID = strings.TrimSpace(listID); listID == "" {
		return fmt.Errorf("hubspot: UpdateListFilters requires a non-empty list id")
	}
	if len(filterBranch) == 0 {
		return fmt.Errorf("hubspot: UpdateListFilters requires a filterBranch")
	}
	body := map[string]any{"filterBranch": filterBranch}
	if _, err := c.doRequest(ctx, http.MethodPut, listsPath+"/"+url.PathEscape(listID)+"/update-list-filters", body, false); err != nil {
		return fmt.Errorf("hubspot: update list %s filters: %w", listID, err)
	}
	return nil
}

// EventDefinition is a HubSpot custom-event definition. fullyQualifiedName is the
// value the audience builder needs for a BEHAVIORAL_EVENT filter's eventTypeId. The
// human label lives under a nested `labels` object (singular/plural), NOT a top-level
// `label` string — Label surfaces labels.singular for callers.
type EventDefinition struct {
	FullyQualifiedName string `json:"fullyQualifiedName"`
	Name               string `json:"name"`
	// Labels is HubSpot's nested label object (BehavioralEventTypeDefinitionLabels).
	Labels struct {
		Singular string `json:"singular"`
		Plural   string `json:"plural"`
	} `json:"labels"`
	// Label is the singular label, populated from Labels for caller convenience.
	Label string `json:"-"`
}

// ListEventDefinitions returns ALL of the portal's custom-event definitions.
// Read-only. The endpoint is cursor-paginated (paging.next.after), so this follows
// every page — a portal with >limit definitions would otherwise silently lose the
// later ones, and the audience builder could not resolve those events.
func (c *Client) ListEventDefinitions(ctx context.Context) ([]EventDefinition, error) {
	out := make([]EventDefinition, 0)
	after := ""
	for page := 0; page < maxListPages; page++ {
		q := url.Values{}
		q.Set("limit", "100")
		// includeProperties is deliberately NOT set: EventDefinition doesn't retain a
		// definition's property list, and requesting it serializes potentially large
		// property payloads for every event on every page (pushing toward the client's
		// 10 MiB response cap) for data we discard.
		if after != "" {
			q.Set("after", after)
		}
		raw, err := c.doRequest(ctx, http.MethodGet, eventDefsPath+"?"+q.Encode(), nil, true)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Results []EventDefinition `json:"results"`
			Paging  *paging           `json:"paging"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("hubspot: decode event definitions: %w", err)
		}
		for i := range resp.Results {
			resp.Results[i].Label = resp.Results[i].Labels.Singular
		}
		out = append(out, resp.Results...)
		if resp.Paging == nil || resp.Paging.Next == nil || resp.Paging.Next.After == "" {
			return out, nil
		}
		// A non-advancing cursor would re-fetch the same page forever, duplicating
		// results until the page cap — refuse to loop on it.
		if resp.Paging.Next.After == after {
			return nil, fmt.Errorf("hubspot: ListEventDefinitions cursor did not advance (repeated after token)")
		}
		after = resp.Paging.Next.After
	}
	return nil, fmt.Errorf("hubspot: ListEventDefinitions exceeded %d pages; refusing to page unbounded", maxListPages)
}

// decodeListEnvelope decodes a list response, handling BOTH shapes HubSpot returns:
// a `{ "list": {…} }` wrapper (get/create) and a bare top-level list object. It
// prefers the wrapper when present, errors on malformed JSON or a missing listId.
func (c *Client) decodeListEnvelope(raw []byte, op string) (*List, error) {
	var env struct {
		List *List `json:"list"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("hubspot: decode list (%s): %w", op, err)
	}
	l := env.List
	if l == nil || l.ListID == "" {
		// Fall back to a top-level (un-wrapped) list object.
		var top List
		if err := json.Unmarshal(raw, &top); err != nil {
			return nil, fmt.Errorf("hubspot: decode list (%s): %w", op, err)
		}
		if top.ListID == "" {
			return nil, fmt.Errorf("hubspot: %s list response carried no listId", op)
		}
		l = &top
	}
	l.resolveSize() // GET/CREATE carry a top-level integer `size`
	l.AppURL = c.listURL(l.ListID)
	return l, nil
}

// listURL builds a human-facing link to a list. Empty when the portal id is unset.
func (c *Client) listURL(listID string) string {
	if c.account.PortalID == "" || listID == "" {
		return ""
	}
	return c.appBaseURL + "/contacts/" + c.account.PortalID + "/lists/" + listID
}
