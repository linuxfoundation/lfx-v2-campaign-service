// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
)

// imageAsset is the ImageAsset sub-payload of an asset create: the raw image bytes,
// base64-encoded (Google Ads REST encodes every `bytes` proto field as standard
// base64). No MIME/format field is sent — Google infers PNG/JPG/GIF from the bytes.
type imageAsset struct {
	Data string `json:"data"`
}

// assetCreate is the create payload for assets:mutate. Only imageAsset is set; the
// asset `type` is deliberately NOT sent — it is output-only on a v23 create (Google
// derives it from which asset-subtype field is populated) and naming it can be
// rejected. No `name` is sent either: Google auto-names the asset, and image-asset
// names are not required to be unique, so omitting one avoids inventing a
// uniqueness/DUPLICATE_NAME concern the campaign/budget names already carry.
type assetCreate struct {
	ImageAsset imageAsset `json:"imageAsset"`
}

// uploadImageAsset uploads one image to the account's asset library via
// customers/{cid}/assets:mutate and returns the created asset's resource name
// (customers/{cid}/assets/{id}) — a single-id resource, unlike the composite
// {adGroupId}~{adId} an adGroupAds create returns. The resource name is what a Demand
// Gen ad references from each image role (marketing / square marketing / logo), so
// this is the first step G3's ad create depends on.
//
// idempotent=false, exactly like the budget/campaign/adGroup creates: assets:mutate
// carries no idempotency key, so doRequest must NOT blind-retry a mutating 429. Unlike
// those creates, though, this needs no partial-result/ambiguity contract and no
// app-side cache, for two reasons:
//
//  1. An image asset is a non-spending LIBRARY object. A duplicate is harmless — it
//     never double-spends the way a duplicate budget/campaign would — so a caller does
//     not need to reconcile "an asset may exist" the way it must for a campaign.
//  2. Google content-addresses image assets: identical bytes resolve to the SAME asset
//     resource, so a re-dispatch re-derives the same resource name rather than leaking
//     one. This mirrors meta.uploadImage, which likewise relies on content-addressed
//     idempotency with no cache. An in-Client (customerID, checksum) cache would buy
//     nothing here anyway: resolveGoogleAdsClient builds a fresh Client per dispatch, so
//     the cache would be empty on any retry and could only dedupe identical bytes WITHIN
//     one call — which content-addressing already handles.
//
// Not live-verified: the dedupe behaviour above (research O3) could not be probed
// against a real account from this workstation (no Google Ads credentials). If Google
// did NOT dedupe, the worst case is a leaked harmless library asset on a retry, never a
// double-spend — so the design does not depend on it holding.
func (c *Client) uploadImageAsset(ctx context.Context, image []byte) (string, error) {
	if len(image) == 0 {
		return "", fmt.Errorf("google-ads image asset upload called with no bytes")
	}

	req := mutateRequest{Operations: []mutateOperation{{Create: assetCreate{
		ImageAsset: imageAsset{Data: base64.StdEncoding.EncodeToString(image)},
	}}}}
	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("assets:mutate"), req, false)
	if err != nil {
		return "", fmt.Errorf("google-ads image asset upload failed: %w", err)
	}
	// A 2xx with no/malformed resource name is treated as UNCONFIRMED, like the other
	// creates: the upload may have landed but cannot be named, so it must not be reported
	// as a usable asset. firstResourceName only extracts a trailing id — validateResourceKind
	// then confirms the resource is this account's assets/{numericID} shape, rejecting a
	// wrong-account or wrong-kind 2xx before its id is handed to an ad create.
	resourceName, _, err := firstResourceName(resp)
	if err != nil {
		return "", fmt.Errorf("google-ads image asset upload UNCONFIRMED (2xx with no/malformed resource name): %w", err)
	}
	if verr := c.validateResourceKind("assets", resourceName, true); verr != nil {
		return "", fmt.Errorf("google-ads image asset upload UNCONFIRMED (malformed asset resource name %q): %w", resourceName, verr)
	}
	return resourceName, nil
}
