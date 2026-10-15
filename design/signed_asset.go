// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package design — the signed creative-asset route (LFXV2-2665).
//
// Reddit Ads API v3 accepts a promoted-post image only as a PUBLIC URL that Reddit fetches
// and re-hosts at post creation; it has no upload endpoint. To let a Reddit dispatch use an
// image stored through upload-creative-asset, the service serves that one asset's bytes at a
// short-lived, HMAC-signed URL that the Reddit client mints immediately before the
// promoted-post request that hands it to Reddit as media_url. See
// docs/knowledge/architecture/signed-creative-asset-urls.md for the full security model.
//
// The route is UNAUTHENTICATED BY DESIGN — Reddit's fetcher carries no LFX credential — so the
// signature IS the authorization. It is a service of its own, with no Security, so no JWT
// scheme can be inherited by accident, and its generated server is mounted with a formatter
// that turns EVERY failure — a malformed id, a bad or expired signature, a missing or foreign
// asset, the feature being off — into the same 404 body (see
// cmd/campaign-service/signed_asset_mount.go). It is hidden from the OpenAPI documents: no API
// client is meant to call it, and tokens are never returned to one.
package design

import (
	//nolint:staticcheck // ST1001: the recommended way of using the goa DSL package is with the . import
	. "goa.design/goa/v3/dsl"
)

// SignedAssetNotFoundError is the ONE body every rejected signed-asset request receives. It
// is a type of its own, not NotFoundError, because NotFoundError's published message names
// a connection, and because the point of this body is that it is identical for every cause.
var SignedAssetNotFoundError = Type("signed-asset-not-found-error", func() {
	errorAttrs("404", "Not found.")
})

// SignedAssetUnavailableError is answered only AFTER the signature and the asset have been
// verified, when the process-wide asset-memory budget cannot admit the bytes in time. It
// reveals nothing a valid-URL holder does not already know.
var SignedAssetUnavailableError = Type("signed-asset-unavailable-error", func() {
	errorAttrs("503", "The service is unavailable.")
})

// SignedCreativeAssetResult carries the response HEADERS of a served asset. The bytes
// themselves stream as the raw response body (SkipResponseBodyEncodeDecode), never through
// a JSON encoder.
var SignedCreativeAssetResult = Type("signed-creative-asset-result", func() {
	Attribute("content_type", String, "The asset's VERIFIED stored MIME type", func() {
		Enum("image/png", "image/jpeg")
	})
	Attribute("content_length", Int64, "Byte length of the image")
	Attribute("cache_control", String, "Cache policy for the response")
	Attribute("content_disposition", String, "Always inline")
	Attribute("x_content_type_options", String, "Always nosniff")
	Required("content_type", "content_length", "cache_control", "content_disposition", "x_content_type_options")
})

var _ = Service("lfx-v2-campaign-service-signed-assets", func() {
	Description("Serves one stored creative asset at a short-lived HMAC-signed URL that the Reddit client mints immediately before post creation. Unauthenticated by design: the signature is the authorization.")
	// Hidden from the OpenAPI documents as a whole (not only the method), so not even the
	// service tag is published: no API client calls this route.
	Meta("swagger:generate", "false")

	Error("NotFound", SignedAssetNotFoundError, "Not found (every rejection, whatever its cause)")
	Error("ServiceUnavailable", SignedAssetUnavailableError, "Asset memory budget exhausted")

	Method("get-signed-creative-asset", func() {
		Description("Serve the bytes of one stored creative asset named by a valid, unexpired signature over (project, brief, asset, expiry). Every rejection is the same 404.")
		Meta("swagger:generate", "false")
		Payload(func() {
			// Project ids in this service are canonical slugs, or (for historical rows) a
			// UUID; this pattern admits both and nothing that could traverse a path.
			Attribute("project_id", String, "Project slug or UUID the asset belongs to", func() {
				Pattern(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)
			})
			Attribute("brief_id", String, "Brief UUID", func() { Format(FormatUUID) })
			Attribute("asset_id", String, "Creative asset UUID", func() { Format(FormatUUID) })
			Attribute("exp", Int64, "Expiry, unix seconds", func() {
				Minimum(1)
			})
			// 32-byte HMAC-SHA256, base64url without padding: exactly 43 characters.
			Attribute("sig", String, "Signature", func() {
				Pattern(`^[A-Za-z0-9_-]{43}$`)
			})
			Required("project_id", "brief_id", "asset_id", "exp", "sig")
		})
		Result(SignedCreativeAssetResult)
		HTTP(func() {
			GET("/_campaigns/creative-assets/{project_id}/{brief_id}/{asset_id}")
			HEAD("/_campaigns/creative-assets/{project_id}/{brief_id}/{asset_id}")
			Param("exp")
			Param("sig")
			SkipResponseBodyEncodeDecode()
			Response(StatusOK, func() {
				Header("content_type:Content-Type")
				Header("content_length:Content-Length")
				Header("cache_control:Cache-Control")
				Header("content_disposition:Content-Disposition")
				Header("x_content_type_options:X-Content-Type-Options")
			})
			// Bodyless: Goa forbids a body on any HEAD response, and an empty 404 is also the
			// most generic answer there is — nothing in it can differ between causes.
			Response("NotFound", StatusNotFound, func() { Body(Empty) })
			Response("ServiceUnavailable", StatusServiceUnavailable, func() { Body(Empty) })
		})
	})
})
