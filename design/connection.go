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
// This file defines the API contract only. The stub service implementation
// lives in internal/service/connection.go; persistence/encryption land in
// LFXV2-2555/2556.
package design

import (
	//nolint:staticcheck // ST1001: the recommended way of using the goa DSL package is with the . import
	. "goa.design/goa/v3/dsl"
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

// projectIDAttr declares the project path parameter.
func projectIDAttr() {
	Attribute("project_id", String, "Project UUID or slug that scopes the connection", func() {
		Example("cncf")
	})
}

// ifMatchAttr declares the If-Match conditional-request header attribute.
func ifMatchAttr() {
	Attribute("if_match", String, "If-Match header carrying the current ETag/version", func() {
		Example("3")
	})
}

// ─── Standard error types (mirrors committee-service) ───

var BadRequestError = Type("bad-request-error", func() {
	Attribute("message", String, "Error message", func() { Example("The request was invalid.") })
	Required("message")
})

var NotFoundError = Type("not-found-error", func() {
	Attribute("message", String, "Error message", func() { Example("The connection was not found.") })
	Required("message")
})

var ConflictError = Type("conflict-error", func() {
	Attribute("message", String, "Error message", func() { Example("A connection for this provider already exists on the project.") })
	Required("message")
})

var PreconditionFailedError = Type("precondition-failed-error", func() {
	Attribute("message", String, "Error message", func() { Example("The supplied ETag does not match the current version.") })
	Required("message")
})

var PreconditionRequiredError = Type("precondition-required-error", func() {
	Attribute("message", String, "Error message", func() { Example("An If-Match header is required.") })
	Required("message")
})

var InternalServerError = Type("internal-server-error", func() {
	Attribute("message", String, "Error message", func() { Example("An internal server error occurred.") })
	Required("message")
})

var ConnServiceUnavailableError = Type("conn-service-unavailable-error", func() {
	Attribute("message", String, "Error message", func() { Example("The service is unavailable.") })
	Required("message")
})

// ─── Google Ads connection types ───

// GoogleAdsCredentials is the plaintext credential shape a caller supplies. It
// is encrypted at rest (AES-256-GCM) and never returned in a response.
var GoogleAdsCredentials = Type("google-ads-credentials", func() {
	Description("Google Ads OAuth credential set. Write-only; never returned.")
	Attribute("refresh_token", String, "OAuth refresh token")
	Attribute("client_id", String, "OAuth client id")
	Attribute("client_secret", String, "OAuth client secret")
	Attribute("developer_token", String, "Google Ads developer token")
	Required("refresh_token", "client_id", "client_secret", "developer_token")
})

// GoogleAdsConnectionConfig is the mutable, non-credential configuration.
var GoogleAdsConnectionConfig = Type("google-ads-connection-config", func() {
	Attribute("label", String, "Optional friendly name", func() { Example("TLF Main") })
	Attribute("account_id", String, "Google Ads customer ID", func() { Example("8666746580") })
	Attribute("login_customer_id", String, "Manager account used for API access", func() { Example("9746983954") })
	Required("account_id")
})

// GoogleAdsConnection is the response view. Credentials are redacted; a
// has_credentials flag reports whether a credential is stored.
var GoogleAdsConnection = Type("google-ads-connection", func() {
	Attribute("id", String, "Service-generated connection UUID (not used in paths)")
	Attribute("project_id", String, "Owning project")
	Attribute("label", String, "Optional friendly name")
	Attribute("account_id", String, "Google Ads customer ID")
	Attribute("login_customer_id", String, "Manager account used for API access")
	Attribute("has_credentials", Boolean, "Whether an encrypted credential is stored")
	Attribute("status", String, "Connection status", func() {
		Enum("active", "inactive", "error", "deleted")
	})
	Attribute("version", Int64, "Optimistic-concurrency version")
	Attribute("etag", String, "ETag header value (mirrors version)")
	Required("id", "project_id", "account_id", "has_credentials", "status", "version")
})

// TestResult is the outcome of verifying a credential against the provider.
var TestResult = Type("connection-test-result", func() {
	Attribute("ok", Boolean, "Whether the credential authenticated against the provider")
	Attribute("message", String, "Human-readable detail")
	Required("ok")
})

// ─── Connection service ───

var _ = Service("lfx-v2-campaign-service-connections", func() {
	Description("Manage a project's singleton, per-provider ad-platform connections.")

	Security(JWTAuth)

	// Provider: google-ads. The remaining providers (linkedin-ads, meta-ads,
	// reddit-ads, twitter-ads, microsoft-ads, hubspot) follow this exact shape
	// with their own typed payloads and are added in the same file as a
	// follow-up increment (tracked under LFXV2-2554).

	Method("create-google-ads", func() {
		Description("Create the project's Google Ads connection (singleton; 409 if one already exists).")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("config", GoogleAdsConnectionConfig)
			Attribute("credentials", GoogleAdsCredentials)
			Required("project_id", "config", "credentials")
		})
		Result(GoogleAdsConnection)
		Error("BadRequest", BadRequestError, "Bad request")
		Error("Conflict", ConflictError, "A connection already exists for this provider on the project")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			POST("/projects/{project_id}/connection-google-ads")
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

	Method("get-google-ads", func() {
		Description("Get the project's Google Ads connection (credentials redacted; returns ETag).")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Required("project_id")
		})
		Result(GoogleAdsConnection)
		Error("NotFound", NotFoundError, "Resource not found")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/connection-google-ads")
			Header("bearer_token:Authorization")
			Response(StatusOK, func() {
				Header("etag:ETag")
			})
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("update-google-ads", func() {
		Description("Replace the Google Ads connection config (requires If-Match; does not set credentials).")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			ifMatchAttr()
			Attribute("config", GoogleAdsConnectionConfig)
			Required("project_id", "config")
		})
		Result(GoogleAdsConnection)
		Error("BadRequest", BadRequestError, "Bad request")
		Error("NotFound", NotFoundError, "Resource not found")
		Error("PreconditionFailed", PreconditionFailedError, "ETag mismatch")
		Error("PreconditionRequired", PreconditionRequiredError, "If-Match header required")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			PUT("/projects/{project_id}/connection-google-ads")
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

	Method("delete-google-ads", func() {
		Description("Soft-delete the project's Google Ads connection.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Required("project_id")
		})
		Error("NotFound", NotFoundError, "Resource not found")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			DELETE("/projects/{project_id}/connection-google-ads")
			Header("bearer_token:Authorization")
			Response(StatusNoContent)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("test-google-ads", func() {
		Description("Verify the stored Google Ads credential against the provider.")
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
			POST("/projects/{project_id}/connection-google-ads/test")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("set-credential-google-ads", func() {
		Description("Replace the stored (encrypted) Google Ads credential. Separate from update so credential replacement is independently permissioned and audited. Not a rotate — the service does not generate or swap secrets upstream.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("credentials", GoogleAdsCredentials)
			Required("project_id", "credentials")
		})
		Error("BadRequest", BadRequestError, "Bad request")
		Error("NotFound", NotFoundError, "Resource not found")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			POST("/projects/{project_id}/connection-google-ads/set-credential")
			Header("bearer_token:Authorization")
			Response(StatusNoContent)
			Response("BadRequest", StatusBadRequest)
			Response("NotFound", StatusNotFound)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})
})
