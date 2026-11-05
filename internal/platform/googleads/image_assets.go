// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
)

// base64Image renders one image for the wire. Google's ImageAsset.data is a proto
// bytes field, which the REST surface carries as standard base64.
func base64Image(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// imageAssetNamePrefix starts every image asset name this client sends.
const imageAssetNamePrefix = "LFX image "

// imageAssetName is the Asset.name every image asset create sends: imageAssetNamePrefix and
// the first 16 hex characters of the image bytes' SHA-256 (#491).
//
// Google REQUIRES a name on an image asset. Without one, assets:mutate (and an image create
// inside googleAds:mutate) is refused with AssetError NAME_REQUIRED_FOR_ASSET_TYPE, which is
// why no image-bearing creative reached Google before this (validateOnly probes on
// 2026-11-05, account 8666746580). The same probes showed that a name need not be unique in
// the account and that 128 characters is accepted and 129 refused (TOO_LONG); this name is
// always 26.
//
// Derived from the BYTES, never from the source URL: the obvious label (slot plus URL) would
// ship the caller's creative URL, query string and all, to Google as a permanent
// human-visible label in the shared Foundation account. A hash names nothing about where the
// image came from, and the same bytes always get the same name, which matches Google
// returning the existing asset for a repeated image (result_ids.go).
func imageAssetName(data []byte) string {
	sum := sha256.Sum256(data)
	return imageAssetNamePrefix + hex.EncodeToString(sum[:8])
}
