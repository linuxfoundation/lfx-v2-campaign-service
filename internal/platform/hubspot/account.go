// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// tokenInfoPath is the only endpoint that answers "which portal is THIS token for?" for a
// private app.
//
// The obvious candidate, GET /account-info/v3/details, does not work here and was the first
// attempt. HubSpot documents that endpoint as requiring the `oauth` scope, and `oauth` is not
// a scope a private app can hold — it does not appear in the private-app scope picker at all,
// because it is the implicit scope carried by an OAuth-installed public app. A private-app
// token is therefore rejected there in every account, not just under-scoped ones. Since both
// callers treat a failed lookup as "portal unknown", using it would have meant Dispatch never
// recording a portal and ReadMetrics fail-closing on ErrCampaignProvenanceUnknown forever:
// the guard would have looked correct in tests, been wired end to end, and returned nothing
// in production.
//
// This endpoint is the documented private-app equivalent. Note the shape difference: the
// token goes in the BODY as `tokenKey`, not (only) in the Authorization header, and the field
// is `hubId` rather than `portalId`. Sending the header too is harmless and keeps doRequest's
// single auth path.
//
// Putting a credential in a request body is safe HERE specifically because this client's
// errors are typed (preSendError, transportError, apiError) and render method and path only —
// no request body reaches an error string. That is a property to re-check before adding any
// other body-carried secret.
const tokenInfoPath = "/oauth/v2/private-apps/get/access-token-info"

// AuthenticatedPortalID reports the hub (portal) id the private-app token actually
// authenticates against.
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
	// Idempotent: this is a read, so a 429 is safe to retry.
	raw, err := c.doRequest(ctx, "POST", tokenInfoPath,
		map[string]string{"tokenKey": c.creds.PrivateAppToken}, true)
	if err != nil {
		return "", fmt.Errorf("read hubspot account details: %w", err)
	}
	var body struct {
		HubID json.Number `json:"hubId"`
	}
	if uerr := json.Unmarshal(raw, &body); uerr != nil {
		// The cause is dropped, not just the body: json.SyntaxError and
		// json.UnmarshalTypeError can reproduce fragments of the input in their own
		// messages. Both callers of this method put perr into something that ends up
		// logged — GetCampaignMetrics's default branch via safeErrSummary(err), and
		// Dispatch's best-effort warning directly — so a wrapped cause here is a second
		// path into the same log line statistics.go's decode error already avoids.
		// Matches internal/platform/hubspot/statistics.go's decode error.
		//
		// Worded as "not a valid token-info response" rather than "not valid JSON"
		// because this branch catches more than malformed JSON: `{"hubId":"abc"}` is
		// syntactically valid and still fails here, because a non-numeric string cannot
		// unmarshal into json.Number. Naming the syntax would misreport that case as a
		// broken response body when the body parsed fine and simply said something this
		// endpoint cannot mean.
		return "", fmt.Errorf("read hubspot account details: response is not a valid token-info response (%d bytes)", len(raw))
	}
	// A zero or absent hubId is a successful call that established nothing, and the
	// caller's whole reason for asking is to refuse when identity is unknown. Reporting
	// "" with no error would hand it the answer it must not treat as an answer.
	id, perr := strconv.ParseInt(body.HubID.String(), 10, 64)
	if perr != nil || id <= 0 {
		return "", fmt.Errorf("read hubspot account details: response carried no usable hubId")
	}
	return strconv.FormatInt(id, 10), nil
}
