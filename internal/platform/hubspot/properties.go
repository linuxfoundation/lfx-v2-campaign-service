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

const contactPropertiesPath = "/crm/v3/properties/contacts"

// ContactProperty is the subset of a contact property definition a list filter
// depends on.
//
// Type matters because HubSpot accepts a filter whose operationType disagrees with
// the property's type and then matches nobody: an enumeration property filtered as
// MULTISTRING builds a list that looks right and is empty.
type ContactProperty struct {
	Name      string `json:"name"`
	Label     string `json:"label"`
	Type      string `json:"type"`
	FieldType string `json:"fieldType"`
	Options   []struct {
		Label string `json:"label"`
		Value string `json:"value"`
	} `json:"options"`
}

// IsEnumeration reports whether filters on this property must use ENUMERATION.
func (p ContactProperty) IsEnumeration() bool { return p.Type == "enumeration" }

// GetContactProperty reads one contact property's definition. Read-only.
//
// A property the portal does not define is (nil, nil), not an error: callers
// validate a set of names and report every missing one together, and a 404 is
// the expected answer for a mistyped or other-portal name.
func (c *Client) GetContactProperty(ctx context.Context, name string) (*ContactProperty, error) {
	if name = strings.TrimSpace(name); name == "" {
		return nil, fmt.Errorf("hubspot: GetContactProperty requires a property name")
	}
	raw, err := c.doRequest(ctx, http.MethodGet, contactPropertiesPath+"/"+url.PathEscape(name), nil, true)
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var p ContactProperty
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("hubspot: decode contact property %q: %w", name, err)
	}
	if p.Name == "" {
		return nil, fmt.Errorf("hubspot: contact property %q returned a 2xx with no name (malformed response)", name)
	}
	return &p, nil
}

// EventProperty is one property defined on a custom event type.
type EventProperty struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Type  string `json:"type"`
}

// EventTypeProperties returns the properties of the event definition whose
// objectTypeId is objectTypeID (e.g. "6-58204655"). Read-only.
//
// (nil, nil) when no definition carries that id: the caller decides whether a
// missing definition fails its request. Pages at 50 because properties are
// requested and some definitions carry many of them.
func (c *Client) EventTypeProperties(ctx context.Context, objectTypeID string) ([]EventProperty, error) {
	if objectTypeID = strings.TrimSpace(objectTypeID); objectTypeID == "" {
		return nil, fmt.Errorf("hubspot: EventTypeProperties requires an object type id")
	}
	after := ""
	for page := 0; page < maxListPages; page++ {
		q := url.Values{}
		q.Set("limit", "50")
		q.Set("includeProperties", "true")
		if after != "" {
			q.Set("after", after)
		}
		raw, err := c.doRequest(ctx, http.MethodGet, eventDefsPath+"?"+q.Encode(), nil, true)
		if err != nil {
			return nil, err
		}
		var resp struct {
			Results []struct {
				ObjectTypeID string          `json:"objectTypeId"`
				Properties   []EventProperty `json:"properties"`
			} `json:"results"`
			Paging *paging `json:"paging"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, fmt.Errorf("hubspot: decode event definitions: %w", err)
		}
		if resp.Results == nil {
			return nil, fmt.Errorf("hubspot: event definitions returned a 2xx with no results array (malformed response)")
		}
		for _, d := range resp.Results {
			if d.ObjectTypeID == objectTypeID {
				return d.Properties, nil
			}
		}
		if resp.Paging == nil || resp.Paging.Next == nil || resp.Paging.Next.After == "" {
			return nil, nil
		}
		if resp.Paging.Next.After == after {
			return nil, fmt.Errorf("hubspot: EventTypeProperties cursor did not advance (repeated after token)")
		}
		after = resp.Paging.Next.After
	}
	return nil, fmt.Errorf("hubspot: EventTypeProperties exceeded %d pages; refusing to page unbounded", maxListPages)
}
