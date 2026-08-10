// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// AuthenticatedPortalID reports the hub (portal) id the private-app token actually
// authenticates against, read from /account-info/v3/details.
//
// This is deliberately NOT AccountConfig.PortalID. That field is an operator-supplied
// string used only to build app URLs, and nothing keeps it in step with the token: a
// credential swap replaces the token and leaves providerConfig untouched, so the
// configured value can name portal A while every request goes to portal B. Only the
// token can say which portal a request will reach, and this is how you ask it.
//
// The id is returned as a string because that is what it is compared against
// everywhere else here — a stored provenance value, not a number to do arithmetic on.
func (c *Client) AuthenticatedPortalID(ctx context.Context) (string, error) {
	raw, err := c.doRequest(ctx, "GET", "/account-info/v3/details", nil, true)
	if err != nil {
		return "", fmt.Errorf("read hubspot account details: %w", err)
	}
	var body struct {
		PortalID json.Number `json:"portalId"`
	}
	if uerr := json.Unmarshal(raw, &body); uerr != nil {
		// The cause is dropped, not just the body: json.SyntaxError and
		// json.UnmarshalTypeError can reproduce fragments of the input in their own
		// messages. Both callers of this method put perr into something that ends up
		// logged — GetCampaignMetrics's default branch via safeErrSummary(err), and
		// Dispatch's best-effort warning directly — so a wrapped cause here is a second
		// path into the same log line statistics.go's decode error already avoids.
		// Matches internal/platform/hubspot/statistics.go's decode error.
		return "", fmt.Errorf("read hubspot account details: response is not valid JSON (%d bytes)", len(raw))
	}
	// A zero or absent portalId is a successful call that established nothing, and the
	// caller's whole reason for asking is to refuse when identity is unknown. Reporting
	// "" with no error would hand it the answer it must not treat as an answer.
	id, perr := strconv.ParseInt(body.PortalID.String(), 10, 64)
	if perr != nil || id <= 0 {
		return "", fmt.Errorf("read hubspot account details: response carried no usable portalId")
	}
	return strconv.FormatInt(id, 10), nil
}
