// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package design — connection endpoints.
//
// Connections are strongly typed per provider and singleton per project: a
// project holds at most one connection of any given provider, addressed as
// /projects/{projectId}/connection-{provider} (no service id in the path, no
// List endpoint — the provider name is the identity within the project). See
// docs/api-catalog.md and docs/channel-connections-schema.md.
//
// All seven paid providers (google-ads, linkedin-ads, meta-ads, reddit-ads,
// twitter-ads, microsoft-ads, hubspot) are defined here. Each shares the six
// endpoint shapes via connectionMethods and carries its own strongly-typed
// credential/config/result. This file defines the API contract only; the stub
// service implementation lives in internal/service/connection.go, and
// persistence/encryption land in LFXV2-2555/2556.
package design

import (
	//nolint:staticcheck // ST1001: the recommended way of using the goa DSL package is with the . import
	. "goa.design/goa/v3/dsl"
	"goa.design/goa/v3/eval"
)

// JWTAuth is the JWT security scheme. Tokens are issued by Heimdall at the
// gateway (audience = this service) and validated in-app. Authorization on the
// campaign_manager relation is enforced at the gateway, not here.
var JWTAuth = JWTSecurity("jwt", func() {
	Description("JWT issued by Heimdall; audience is this service.")
})

// ─── Shared attribute helpers ───

// bearerToken declares the JWT bearer token attribute on a payload.
func bearerToken() {
	Token("bearer_token", String, func() {
		Description("JWT token issued by Heimdall")
		Example("eyJhbGci...")
	})
}

// projectIDAttr declares the project path parameter (UUID-or-slug) for the
// read/update/delete routes, which stay permissive (migration 000003 preserved
// historical UUID-keyed rows).
func projectIDAttr() {
	Attribute("project_id", String, "Project UUID or slug that scopes the connection", func() {
		Example("cncf")
	})
}

// projectSlugAttr declares project_id constrained to a CANONICAL SLUG (not a UUID) for
// the campaign-naming CREATE routes only (create-brief, create-campaigns). project_id is
// stamped into the campaign-name attribution key and is the exact-match key for the
// connection lookup at dispatch, so a UUID there breaks the slug-based join. The Pattern
// requires single internal hyphens (no `foo--bar`) and MaxLength(35) rejects a canonical
// 36-char UUID (RE2 has no negative lookahead, so the length bound is the reliable UUID
// discriminator). Declaring the Pattern/MaxLength here makes Goa BOTH publish the
// constraint in the OpenAPI contract AND generate the request-decoder validation for
// the create routes; the handlers additionally guard with validateProjectSlug /
// projectSlugProblem so direct/non-HTTP callers get the same rejection.
func projectSlugAttr() {
	Attribute("project_id", String, "Canonical LFX project slug (NOT a UUID) that scopes the resource", func() {
		Example("cncf")
		Pattern(`^[a-z0-9]+(-[a-z0-9]+)*$`)
		MaxLength(35)
	})
}

// ifMatchAttr declares the If-Match conditional-request header attribute.
func ifMatchAttr() {
	Attribute("if_match", String, "If-Match header carrying the current ETag/version", func() {
		Example("3")
	})
}

// ─── Standard error types ───
//
// Each carries both code and message, matching the service-level
// ServiceUnavailableError in design/design.go so the API's error schemas are
// consistent across the OpenAPI document.

func errorAttrs(codeExample, msgExample string) {
	Attribute("code", String, "HTTP status code", func() { Example(codeExample) })
	Attribute("message", String, "Error message", func() { Example(msgExample) })
	Required("code", "message")
}

var BadRequestError = Type("bad-request-error", func() {
	errorAttrs("400", "The request was invalid.")
})

var NotFoundError = Type("not-found-error", func() {
	errorAttrs("404", "The connection was not found.")
})

var ConflictError = Type("conflict-error", func() {
	errorAttrs("409", "A connection for this provider already exists on the project.")
})

var PreconditionFailedError = Type("precondition-failed-error", func() {
	errorAttrs("412", "The supplied ETag does not match the current version.")
})

var PreconditionRequiredError = Type("precondition-required-error", func() {
	errorAttrs("428", "An If-Match header is required.")
})

var InternalServerError = Type("internal-server-error", func() {
	errorAttrs("500", "An internal server error occurred.")
})

var ConnServiceUnavailableError = Type("conn-service-unavailable-error", func() {
	errorAttrs("503", "The service is unavailable.")
})

// TestResult is the outcome of verifying a credential against the provider.
var TestResult = Type("connection-test-result", func() {
	Attribute("ok", Boolean, "Whether the credential authenticated against the provider")
	Attribute("message", String, "Human-readable detail")
	Required("ok")
})

// commonConnectionAttrs declares the response fields every provider connection
// shares. Per-provider result types call this and then add provider-specific
// config fields.
func commonConnectionAttrs() {
	Attribute("id", String, "Service-generated connection UUID (not used in paths)")
	Attribute("project_id", String, "Owning project")
	Attribute("label", String, "Optional friendly name")
	Attribute("account_id", String, "Provider account identifier")
	Attribute("has_credentials", Boolean, "Whether an encrypted credential is stored")
	Attribute("status", String, "Connection status", func() {
		Enum("active", "inactive", "error", "deleted")
	})
	Attribute("version", Int64, "Optimistic-concurrency version")
	Attribute("etag", String, "ETag header value (mirrors version)")
}

// commonConnectionRequired lists the always-required response fields. etag is
// required so implementations cannot accidentally omit it (FR-004/FR-005).
func commonConnectionRequired() {
	Required("id", "project_id", "account_id", "has_credentials", "status", "version", "etag")
}

// ─── Per-provider method helper ───

// connectionMethods emits the six singleton endpoints for one provider under
// /projects/{projectId}/connection-{key}. It is called once per provider with
// that provider's typed config, credentials, and result types, so the six
// endpoints stay identical in shape while payloads stay strongly typed.
//
// title is a human-readable provider name used in descriptions (e.g. "Google
// Ads"). Goa derives the generated method names from the method keys
// (create-{key} → CreateGoogleAds, etc.), so no explicit suffix is needed.
func connectionMethods(key, title string, config, creds, result eval.Expression) {
	Method("create-"+key, func() {
		Description("Create the project's " + title + " connection (singleton; 409 if one already exists).")
		Payload(func() {
			bearerToken()
			// A connection is created keyed by project_id, which is later the EXACT-MATCH
			// key for the dispatch lookup (ConnectionRepo.Get). brief/campaign create
			// already require a canonical slug, so a UUID-keyed connection could never be
			// joined to a dispatched campaign — constrain create to a slug too. get/update/
			// delete/set-credential/test stay permissive (projectIDAttr) for historical
			// UUID-keyed rows (migration 000003). The generated decoder validates the
			// pattern; the service additionally guards via validateConnectionProjectSlug.
			projectSlugAttr()
			Attribute("config", config)
			Attribute("credentials", creds)
			Required("project_id", "config", "credentials")
		})
		Result(result)
		Error("BadRequest", BadRequestError, "Bad request")
		Error("Conflict", ConflictError, "A connection already exists for this provider on the project")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			POST("/projects/{project_id}/connection-" + key)
			Header("bearer_token:Authorization")
			Response(StatusCreated, func() {
				Header("etag:ETag")
			})
			Response("BadRequest", StatusBadRequest)
			Response("Conflict", StatusConflict)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("get-"+key, func() {
		Description("Get the project's " + title + " connection (credentials redacted; returns ETag).")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Required("project_id")
		})
		Result(result)
		Error("NotFound", NotFoundError, "Resource not found")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/connection-" + key)
			Header("bearer_token:Authorization")
			Response(StatusOK, func() {
				Header("etag:ETag")
			})
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("update-"+key, func() {
		Description("Replace the " + title + " connection config (requires If-Match; does not set credentials).")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			ifMatchAttr()
			Attribute("config", config)
			// if_match is intentionally NOT Required here: a required header makes
			// Goa's decoder reject a missing value with 400, but FR-005 wants a
			// missing precondition to be 428 Precondition Required. Leaving it
			// optional lets the request reach the service, which returns 428 when
			// the header is empty (and 412 on a version mismatch).
			Required("project_id", "config")
		})
		Result(result)
		Error("BadRequest", BadRequestError, "Bad request")
		Error("NotFound", NotFoundError, "Resource not found")
		Error("PreconditionFailed", PreconditionFailedError, "ETag mismatch")
		Error("PreconditionRequired", PreconditionRequiredError, "If-Match header required")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			PUT("/projects/{project_id}/connection-" + key)
			Header("bearer_token:Authorization")
			Header("if_match:If-Match")
			Response(StatusOK, func() {
				Header("etag:ETag")
			})
			Response("BadRequest", StatusBadRequest)
			Response("NotFound", StatusNotFound)
			Response("PreconditionFailed", StatusPreconditionFailed)
			Response("PreconditionRequired", StatusPreconditionRequired)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("delete-"+key, func() {
		Description("Soft-delete the project's " + title + " connection.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Required("project_id")
		})
		Error("NotFound", NotFoundError, "Resource not found")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			DELETE("/projects/{project_id}/connection-" + key)
			Header("bearer_token:Authorization")
			Response(StatusNoContent)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("test-"+key, func() {
		Description("Verify the stored " + title + " credential against the provider.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Required("project_id")
		})
		Result(TestResult)
		Error("NotFound", NotFoundError, "Resource not found")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			POST("/projects/{project_id}/connection-" + key + "/test")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("set-credential-"+key, func() {
		Description("Replace the stored (encrypted) " + title + " credential. Separate from update so credential replacement is independently permissioned and audited. Not a rotate — the service does not generate or swap secrets upstream.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("credentials", creds)
			Required("project_id", "credentials")
		})
		Error("BadRequest", BadRequestError, "Bad request")
		Error("NotFound", NotFoundError, "Resource not found")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			POST("/projects/{project_id}/connection-" + key + "/set-credential")
			Header("bearer_token:Authorization")
			Response(StatusNoContent)
			Response("BadRequest", StatusBadRequest)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})
}

// ─── Google Ads ───

var GoogleAdsCredentials = Type("google-ads-credentials", func() {
	Description("Google Ads OAuth credential set. Write-only; never returned.")
	Attribute("refresh_token", String, "OAuth refresh token")
	Attribute("client_id", String, "OAuth client id")
	Attribute("client_secret", String, "OAuth client secret")
	Attribute("developer_token", String, "Google Ads developer token")
	Required("refresh_token", "client_id", "client_secret", "developer_token")
})

var GoogleAdsConnectionConfig = Type("google-ads-connection-config", func() {
	Attribute("label", String, "Optional friendly name", func() { Example("TLF Main") })
	Attribute("account_id", String, "Google Ads customer ID", func() { Example("8666746580") })
	Attribute("login_customer_id", String, "Manager account used for API access", func() { Example("9746983954") })
	Required("account_id")
})

var GoogleAdsConnection = Type("google-ads-connection", func() {
	commonConnectionAttrs()
	Attribute("login_customer_id", String, "Manager account used for API access")
	commonConnectionRequired()
})

// ─── LinkedIn Ads ───

var LinkedInAdsCredentials = Type("linkedin-ads-credentials", func() {
	Description("LinkedIn Ads credential. Write-only; never returned.")
	Attribute("access_token", String, "OAuth access token")
	Required("access_token")
})

var LinkedInAdsConnectionConfig = Type("linkedin-ads-connection-config", func() {
	Attribute("label", String, "Optional friendly name")
	// Both ids are interpolated into LinkedIn request paths / URNs
	// (adAccounts/<account_id>/..., urn:li:organization:<org_id>), and the client treats
	// them as the bare NUMERIC id — a non-numeric value stored on an active connection
	// would fail every dispatch asynchronously. Validate the numeric shape here so an
	// unusable id is a 4xx at connection creation instead. MaxLength bounds the stored
	// size (real LinkedIn ids are short).
	Attribute("account_id", String, "LinkedIn ad account ID (numeric)", func() {
		Example("538170226")
		Pattern(`^[0-9]+$`)
		MaxLength(64)
	})
	Attribute("org_id", String, "LinkedIn organization ID (the bare NUMERIC id, e.g. 208777 — not the full urn:li:organization: URN)", func() {
		Example("208777")
		Pattern(`^[0-9]+$`)
		MaxLength(64)
	})
	Required("account_id", "org_id")
})

var LinkedInAdsConnection = Type("linkedin-ads-connection", func() {
	commonConnectionAttrs()
	Attribute("org_id", String, "LinkedIn organization ID (the bare NUMERIC id, not the full urn:li:organization: URN)")
	commonConnectionRequired()
})

// ─── Meta Ads ───

var MetaAdsCredentials = Type("meta-ads-credentials", func() {
	Description("Meta Ads credential. Write-only; never returned.")
	Attribute("access_token", String, "Meta access token")
	Attribute("app_secret", String, "Meta app secret")
	Required("access_token", "app_secret")
})

var MetaAdsConnectionConfig = Type("meta-ads-connection-config", func() {
	Attribute("label", String, "Optional friendly name")
	Attribute("account_id", String, "Meta ad account ID", func() { Example("act_193556282970417") })
	Attribute("page_id", String, "Facebook page ID")
	Attribute("app_id", String, "Meta app ID")
	// page_id is required at connection time: the Meta dispatcher needs it to attach
	// the promoted-object page, so an active connection without it would always fail
	// dispatch. Requiring it here surfaces the error as a 4xx at connection creation
	// rather than a silent runtime dispatch failure.
	Required("account_id", "page_id")
})

var MetaAdsConnection = Type("meta-ads-connection", func() {
	commonConnectionAttrs()
	Attribute("page_id", String, "Facebook page ID")
	Attribute("app_id", String, "Meta app ID")
	commonConnectionRequired()
})

// ─── Reddit Ads ───

var RedditAdsCredentials = Type("reddit-ads-credentials", func() {
	Description("Reddit Ads OAuth credential. Write-only; never returned.")
	Attribute("client_id", String, "OAuth client id")
	Attribute("client_secret", String, "OAuth client secret")
	Attribute("refresh_token", String, "OAuth refresh token")
	Required("client_id", "client_secret", "refresh_token")
})

var RedditAdsConnectionConfig = Type("reddit-ads-connection-config", func() {
	Attribute("label", String, "Optional friendly name")
	Attribute("account_id", String, "Reddit advertiser ID", func() { Example("t2_gv9wtbfa") })
	Required("account_id")
})

var RedditAdsConnection = Type("reddit-ads-connection", func() {
	commonConnectionAttrs()
	commonConnectionRequired()
})

// ─── X / Twitter Ads (OAuth 1.0a) ───

var TwitterAdsCredentials = Type("twitter-ads-credentials", func() {
	Description("X/Twitter Ads OAuth 1.0a credential set. Write-only; never returned.")
	Attribute("consumer_key", String, "OAuth 1.0a consumer key")
	Attribute("consumer_secret", String, "OAuth 1.0a consumer secret")
	Attribute("access_token", String, "OAuth 1.0a access token")
	Attribute("access_token_secret", String, "OAuth 1.0a access token secret")
	Required("consumer_key", "consumer_secret", "access_token", "access_token_secret")
})

var TwitterAdsConnectionConfig = Type("twitter-ads-connection-config", func() {
	Attribute("label", String, "Optional friendly name")
	Attribute("account_id", String, "X/Twitter Ads account ID", func() { Example("8r7gb") })
	Attribute("funding_instrument_id", String, "Funding instrument for the ad account")
	Required("account_id")
})

var TwitterAdsConnection = Type("twitter-ads-connection", func() {
	commonConnectionAttrs()
	Attribute("funding_instrument_id", String, "Funding instrument for the ad account")
	commonConnectionRequired()
})

// ─── Microsoft Ads ───

var MicrosoftAdsCredentials = Type("microsoft-ads-credentials", func() {
	Description("Microsoft Advertising OAuth credential set. Write-only; never returned.")
	Attribute("client_id", String, "OAuth client id")
	Attribute("client_secret", String, "OAuth client secret")
	Attribute("refresh_token", String, "OAuth refresh token")
	Attribute("developer_token", String, "Microsoft Advertising developer token")
	Required("client_id", "client_secret", "refresh_token", "developer_token")
})

var MicrosoftAdsConnectionConfig = Type("microsoft-ads-connection-config", func() {
	Attribute("label", String, "Optional friendly name")
	Attribute("account_id", String, "Microsoft Advertising account ID")
	Attribute("customer_id", String, "Microsoft Advertising customer ID")
	Required("account_id")
})

var MicrosoftAdsConnection = Type("microsoft-ads-connection", func() {
	commonConnectionAttrs()
	Attribute("customer_id", String, "Microsoft Advertising customer ID")
	commonConnectionRequired()
})

// ─── HubSpot (email) ───

var HubSpotCredentials = Type("hubspot-credentials", func() {
	Description("HubSpot private app token. Write-only; never returned.")
	Attribute("private_app_token", String, "HubSpot private app token")
	Required("private_app_token")
})

var HubSpotConnectionConfig = Type("hubspot-connection-config", func() {
	Attribute("label", String, "Optional friendly name")
	Attribute("account_id", String, "HubSpot list/audience ID")
	Attribute("portal_id", String, "HubSpot portal/account ID")
	Attribute("sender_email", String, "Default sender address")
	Attribute("sender_name", String, "Default sender name")
	Attribute("brand_kit", String, "Per-project brand kit selector")
	Required("account_id")
})

var HubSpotConnection = Type("hubspot-connection", func() {
	commonConnectionAttrs()
	Attribute("portal_id", String, "HubSpot portal/account ID")
	Attribute("sender_email", String, "Default sender address")
	Attribute("sender_name", String, "Default sender name")
	Attribute("brand_kit", String, "Per-project brand kit selector")
	commonConnectionRequired()
})

// ─── Connection service ───

var _ = Service("lfx-v2-campaign-service-connections", func() {
	Description("Manage a project's singleton, per-provider ad-platform connections.")

	Security(JWTAuth)

	connectionMethods("google-ads", "Google Ads", GoogleAdsConnectionConfig, GoogleAdsCredentials, GoogleAdsConnection)
	connectionMethods("linkedin-ads", "LinkedIn Ads", LinkedInAdsConnectionConfig, LinkedInAdsCredentials, LinkedInAdsConnection)
	connectionMethods("meta-ads", "Meta Ads", MetaAdsConnectionConfig, MetaAdsCredentials, MetaAdsConnection)
	connectionMethods("reddit-ads", "Reddit Ads", RedditAdsConnectionConfig, RedditAdsCredentials, RedditAdsConnection)
	connectionMethods("twitter-ads", "X/Twitter Ads", TwitterAdsConnectionConfig, TwitterAdsCredentials, TwitterAdsConnection)
	connectionMethods("microsoft-ads", "Microsoft Ads", MicrosoftAdsConnectionConfig, MicrosoftAdsCredentials, MicrosoftAdsConnection)
	connectionMethods("hubspot", "HubSpot", HubSpotConnectionConfig, HubSpotCredentials, HubSpotConnection)
})
