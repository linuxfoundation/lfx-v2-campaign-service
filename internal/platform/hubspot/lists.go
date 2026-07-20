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
type List struct {
	ListID         string `json:"listId"`
	Name           string `json:"name"`
	Size           int    `json:"size"`
	ProcessingType string `json:"processingType"`
	// FilterBranch is only populated by GetList(includeFilters=true).
	FilterBranch json.RawMessage `json:"filterBranch,omitempty"`
	// AppURL is a human-facing link (built client-side, never from the API).
	AppURL string `json:"-"`
}

// SearchLists returns contact lists whose name matches query. Read-only.
func (c *Client) SearchLists(ctx context.Context, query string) ([]List, error) {
	body := map[string]any{"query": query, "count": 20, "includeFilters": false}
	raw, err := c.doRequest(ctx, http.MethodPost, listSearchPath, body, true)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Lists []List `json:"lists"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("hubspot: decode list search: %w", err)
	}
	for i := range resp.Lists {
		resp.Lists[i].AppURL = c.listURL(resp.Lists[i].ListID)
	}
	return resp.Lists, nil
}

// GetList fetches one list, including its filterBranch and processingType (needed to
// route ILS-vs-legacy on the send-list PATCH). Read-only.
func (c *Client) GetList(ctx context.Context, listID string) (*List, error) {
	if strings.TrimSpace(listID) == "" {
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
	if strings.TrimSpace(name) == "" {
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
		return nil, fmt.Errorf("hubspot: create list %q UNCONFIRMED (a list may have been created; verify before retrying): %w", name, derr)
	}
	return l, nil
}

// UpdateListFilters replaces the filterBranch on an existing list. MUTATING
// (a PUT replace is not idempotent-retriable here: a 429 mid-replace could apply).
func (c *Client) UpdateListFilters(ctx context.Context, listID string, filterBranch json.RawMessage) error {
	if strings.TrimSpace(listID) == "" {
		return fmt.Errorf("hubspot: UpdateListFilters requires a non-empty list id")
	}
	if len(filterBranch) == 0 {
		return fmt.Errorf("hubspot: UpdateListFilters requires a filterBranch")
	}
	body := map[string]any{"filterBranch": filterBranch}
	if _, err := c.doRequest(ctx, http.MethodPut, listsPath+"/"+url.PathEscape(listID)+"/filter-branch", body, false); err != nil {
		return fmt.Errorf("hubspot: update list %s filters: %w", listID, err)
	}
	return nil
}

// EventDefinition is a HubSpot custom-event definition. fullyQualifiedName is the
// value the audience builder needs for a BEHAVIORAL_EVENT filter's eventTypeId.
type EventDefinition struct {
	FullyQualifiedName string `json:"fullyQualifiedName"`
	Label              string `json:"label"`
	Name               string `json:"name"`
}

// ListEventDefinitions returns the portal's custom-event definitions. Read-only.
func (c *Client) ListEventDefinitions(ctx context.Context) ([]EventDefinition, error) {
	q := url.Values{}
	q.Set("limit", "100")
	q.Set("includeProperties", "true")
	raw, err := c.doRequest(ctx, http.MethodGet, eventDefsPath+"?"+q.Encode(), nil, true)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Results []EventDefinition `json:"results"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("hubspot: decode event definitions: %w", err)
	}
	return resp.Results, nil
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
