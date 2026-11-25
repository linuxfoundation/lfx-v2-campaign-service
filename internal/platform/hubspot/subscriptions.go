// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const subscriptionDefinitionsPath = "/communication-preferences/v3/definitions"

// SubscriptionDefinitionNames returns the portal's email subscription types, id -> name.
// Read-only.
//
// It exists so a list description can say "has opted out of “LF Events”" instead of naming a
// subscription by its id: GDPR and opt-out suppression lists are built almost entirely from
// subscription-status filters, and an id tells an operator nothing about who is excluded.
//
// The id arrives as a string on some portals and a number on others, so both are accepted.
// The endpoint needs the communication-preferences read scope; a caller treats any error as
// "no names" and falls back to the ids.
func (c *Client) SubscriptionDefinitionNames(ctx context.Context) (map[string]string, error) {
	raw, err := c.doRequest(ctx, http.MethodGet, subscriptionDefinitionsPath, nil, true)
	if err != nil {
		return nil, err
	}
	var resp struct {
		SubscriptionDefinitions []struct {
			ID   json.RawMessage `json:"id"`
			Name string          `json:"name"`
		} `json:"subscriptionDefinitions"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("hubspot: decode subscription definitions: %w", err)
	}
	if resp.SubscriptionDefinitions == nil {
		return nil, fmt.Errorf("hubspot: subscription definitions returned a 2xx with no subscriptionDefinitions array (malformed response)")
	}
	out := make(map[string]string, len(resp.SubscriptionDefinitions))
	for _, d := range resp.SubscriptionDefinitions {
		id := strings.Trim(strings.TrimSpace(string(d.ID)), `"`)
		if name := strings.TrimSpace(d.Name); id != "" && name != "" {
			out[id] = name
		}
	}
	return out, nil
}
