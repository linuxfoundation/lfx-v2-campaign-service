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
// All seven providers are defined here: the six PAID ad platforms (google-ads,
// linkedin-ads, meta-ads, reddit-ads, twitter-ads, microsoft-ads) plus hubspot,
// the EMAIL channel — see model.ChannelKind for why that distinction matters
// (email has no budget and no run state to pause). Each shares the six
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
// gateway (audience = this service) and verified in-app against Heimdall's JWKS:
// signature, issuer, audience and expiry, plus a non-empty principal claim. The
// gateway checks the same token, so this is not the happy path working twice — the
// gateway's guarantee stops at the cluster boundary, and these claims become the
// created_by / updated_by of records that say who authorized paid ad spend. See
// internal/infrastructure/auth. Authorization on the campaign_manager relation is
// still enforced at the gateway, not here.
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

// ConflictError carries an OPTIONAL reason on top of the standard code/message pair.
//
// Several endpoints return 409 for genuinely different situations that call for
// different client behaviour — retry after a refresh, wait and poll, or stop and
// surface the collision. Distinguishing them by the message text means a client
// pattern-matches English prose that this repo edits freely for operator clarity, so
// the first reworded message silently breaks it. `reason` is the part that is promised
// not to change.
//
// It is deliberately NOT Required, because ConflictError is shared by every endpoint in
// the API and the slugs are being introduced group by group rather than all at once.
// Today only the audiences group populates it: `mapAudienceErr` sets a reason on all
// three of its 409s, which is where the need was sharpest — those three carry OPPOSITE
// remedies. The briefs group also distinguishes many conflicts, but does so in message
// prose only and sets no reason yet; that is a gap to close, not the intended end state.
// Until it is closed a client must treat an absent reason as "unspecified conflict" and
// fall back to the message, which is what the message already says. Making the field
// Required now would force a slug onto every 409 in one change and invent a taxonomy
// nobody has agreed to maintain.
//
// The example on `reason` is chosen to AGREE with the message example already on the type.
// ConflictError is the 409 body for every endpoint in the API, so the object-level example
// Goa publishes is ONE instance standing in for twenty-nine different conflicts — which is
// what makes the choice awkward, and why omitting it is not the safe option it looks like.
//
// Leaving `Example` off does not produce an example-free schema. Goa fills the gap from the
// Enum, and it picks the first member: `audience_build_in_flight`, which the generated
// contract then pairs with "A connection for this provider already exists". That publishes a
// mapping between two unrelated endpoints, which is worse than a value belonging to one of
// them. `already_exists` is the reason that actually accompanies this message, so the
// published instance is at least internally true.
//
// The Enum is what publishes the full vocabulary, and that — not the example — is the part
// clients are entitled to rely on.
var ConflictError = Type("conflict-error", func() {
	errorAttrs("409", "A connection for this provider already exists on the project.")
	Attribute("reason", String, "Stable machine-readable discriminator, present only where an endpoint returns more than one kind of conflict. Absent means unspecified.", func() {
		Enum("stale_approval", "audience_build_in_flight", "already_exists")
		Example("already_exists")
	})
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
		// BadRequest on THIS method also covers payload validation, but that is not why
		// every method below declares it too. JWTAuth returns *conn.BadRequestError when
		// a token is refused, and Goa generates the error encoder from THIS list: a
		// method that omits BadRequest has no case for it, so the typed 400 falls through
		// to the generic encoder and reaches the caller as a 500 — undocumented in
		// OpenAPI, and telling a client with a bad credential to treat it as a server
		// fault. The declaration is what makes JWTAuth's mapping real, so it is required
		// on every method carrying bearerToken(), payload or no payload.
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
		// BadRequest is declared on EVERY secured method, including the reads and the
		// delete, because JWTAuth can now refuse a token — and a refusal it cannot encode
		// becomes a 500. See the comment on the create method's copy.
		Error("BadRequest", BadRequestError, "Bad request")
		Error("NotFound", NotFoundError, "Resource not found")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/connection-" + key)
			Header("bearer_token:Authorization")
			Response(StatusOK, func() {
				Header("etag:ETag")
			})
			Response("BadRequest", StatusBadRequest)
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
		// BadRequest is declared on EVERY secured method, including the reads and the
		// delete, because JWTAuth can now refuse a token — and a refusal it cannot encode
		// becomes a 500. See the comment on the create method's copy.
		Error("BadRequest", BadRequestError, "Bad request")
		Error("NotFound", NotFoundError, "Resource not found")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			DELETE("/projects/{project_id}/connection-" + key)
			Header("bearer_token:Authorization")
			Response(StatusNoContent)
			Response("BadRequest", StatusBadRequest)
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
		// BadRequest is declared on EVERY secured method, including the reads and the
		// delete, because JWTAuth can now refuse a token — and a refusal it cannot encode
		// becomes a 500. See the comment on the create method's copy.
		Error("BadRequest", BadRequestError, "Bad request")
		Error("NotFound", NotFoundError, "Resource not found")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			POST("/projects/{project_id}/connection-" + key + "/test")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			Response("BadRequest", StatusBadRequest)
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

// GoogleAdsConnectionConfig is the FIRST provider config where account_id is optional, and
// the reason is specific rather than a general loosening: a caller may only create a
// connection without an account id where there is an account-DISCOVERY endpoint to find out
// what to put there afterwards.
//
// Discovery is a NECESSARY condition, not a sufficient one. The other half is that the
// operations needing an account id must fail with reason=account_not_selected rather than a
// generic error, so that a connection parked mid-bootstrap is DIAGNOSABLE as such rather than
// indistinguishable from a bad credential. Where the operation is synchronous that reason
// reaches the caller; where it is asynchronous it reaches the dispatch-failure log instead,
// which is the Meta case spelled out below. Google Ads had both
// from the start. Meta is the one provider where the halves came apart — it gained discovery
// in LFXV2-3062 and stayed required here until LFXV2-3061 supplied the tagging; see the
// paragraph below MetaAdsConnectionConfig's own godoc. Of the remaining four, only Microsoft
// has BOTH, as of LFXV2-3064: Reddit and X still lack discovery, while LinkedIn gained a
// discovery endpoint in that ticket and is missing the OTHER half. resolveLinkedInCredentials
// does tag domain.ErrAccountNotSelected, but LinkedInDispatcher.Dispatch never calls it — the
// create path resolves inline and answers a missing account id with a bare notCreated, so the
// missing choice is never named. Microsoft, Reddit and X tag it on a path create reaches
// (validateMicrosoftConnection, resolveRedditClient, validateTwitterConnection). Naming the
// halves separately matters because the bar is the conjunction — Reddit or X becomes eligible
// the day it gains discovery, while LinkedIn needs its create path routed through the resolver.
//
// That is the bootstrap this enables — credentials first, account chosen afterwards:
//
//	POST   /projects/{id}/connection-google-ads          (credentials, no account_id)
//	GET    /projects/{id}/connection-google-ads/accounts (discovery: what can this reach?)
//	PUT    /projects/{id}/connection-google-ads          (set the chosen account_id)
//
// Requiring the id at step one meant the caller had to already know the answer that step two
// exists to produce, which made discovery useful only for RE-POINTING a connection that was
// already complete.
//
// Meta no longer requires it either, as of LFXV2-3061. Both halves the rule below asks for
// are now present: the discovery endpoint arrived with LFXV2-3062, and Meta's Dispatch tags
// an empty account id with domain.ErrAccountNotSelected via requireMetaAccountID
// (internal/dispatch/meta.go), so a connection parked mid-bootstrap is classified rather than
// left an unclassified error. Campaign create remains the only Meta path that needs the id at
// all — the status toggle and the metrics read address the campaign node by id — and it is
// ASYNCHRONOUS, so what a caller who created the row and stopped there meets is the polled job
// result rather than an HTTP status.
//
// That result is GENERIC, and the rule below does not ask otherwise. dispatchPlatform collapses
// every dispatcher error into "platform campaign creation failed", so no tagging can make the
// polled result name the missing choice; what the tagging buys is a fixed-vocabulary
// reason=account_not_selected on the dispatch-failure log line, which is where an operator tells
// this apart from a bad credential. The requirement the rule states is that a half-configured
// connection is DIAGNOSABLE, not that the API returns a bespoke code for it.
//
// LinkedIn, Microsoft, Reddit and X keep Required("account_id"), but no longer all for the
// same reason, and the difference is what tells you how far each is from being relaxed.
// Reddit and X still have NO list to choose from, so relaxing the requirement would create a
// connection that can never be finished from inside this API: the operator has to obtain the
// id out-of-band anyway, and the only thing gained is a half-configured row. LinkedIn and
// Microsoft DO have a discovery endpoint as of LFXV2-3064 — the list exists. What blocks each
// differs, and two independent gates are easy to conflate here:
//
//   - THIS Required("account_id") gates the PUBLIC connection APIs. LinkedIn stays required
//     because it lacks the second half — its create path resolves inline and answers a missing
//     account id with a bare notCreated, so the choice is never named (paragraph above).
//     Microsoft has both halves and is therefore behaviourally eligible to have it relaxed.
//   - accountDiscoveryProviders (internal/bootstrap/sysacct.go) gates only whether an
//     account-less SYSTEM row is installable by the bootstrap CLI. Microsoft is deliberately
//     not in it yet — that is a change to what the CLI accepts and belongs in its own commit.
//
// So Microsoft's exclusion here is a sequencing decision, not a missing capability; an earlier
// version of this comment named the bootstrap map as its "other half", which contradicted the
// paragraph above. Relaxing either gate without both halves is what the next rule forbids.
//
// Add the requirement back for Google Ads or Meta, or drop it for another provider, only
// together with that provider's discovery endpoint AND its account_not_selected tagging.
// Discovery capability and credentials-first bootstrap are not the same thing, and shipping
// one without the other hides which half is missing.
//
// A connection in this state stays status=active, and account_id comes back as "". See
// docs/knowledge/code/internal-service.md — "active" says the connection is ENABLED for
// credential-based operations such as discovery (which refuses a non-active connection),
// NOT that the credentials were verified: nothing verifies them, so an active row can hold
// material the platform will reject. Readiness to run a campaign is account_id being
// non-empty, and the operations that need it say so with reason=account_not_selected.
var GoogleAdsConnectionConfig = Type("google-ads-connection-config", func() {
	Attribute("label", String, "Optional friendly name", func() { Example("TLF Main") })
	Attribute("account_id", String, "Google Ads customer ID. Optional: omit it to create the connection with credentials only, then choose one from GET .../connection-google-ads/accounts and set it with PUT.", func() { Example("8666746580") })
	Attribute("login_customer_id", String, "Manager account used for API access", func() { Example("9746983954") })
})

var GoogleAdsConnection = Type("google-ads-connection", func() {
	commonConnectionAttrs()
	Attribute("login_customer_id", String, "Manager account used for API access")
	commonConnectionRequired()
})

// AccessibleAccount represents an ad account reachable via the connection's
// stored credential. Returned by account discovery operations.
//
// No Example on `id`, deliberately, and the examples live on the two METHODS instead. The
// type is shared by every provider's discovery method and Goa copies an attribute-level
// example into each one's schema, so a single value cannot be right: Google Ads mints bare
// digits, Meta mints `act_`-prefixed ids, and the Meta method below promises the prefix in its
// own description. Pinning Google's `8666746580` published a Meta example that Meta's own
// connection validation rejects.
//
// Deleting it is not sufficient on its own: Goa fabricates a lorem-ipsum example for any
// attribute that lacks one, so a bare type published `id: Iste et aspernatur delectus.` to both
// providers — no longer a wrong claim about a real format, but still not the documented one.
// Each method therefore carries its own result-level example, which is per-provider without
// splitting the element type the shared handler is built on.
//
// The generated `AccessibleAccount` COMPONENT schema still carries Goa's fabricated example,
// and that is left as it is. Every response that returns the type overrides it with the right
// one, so the fabricated value only survives where no provider is in scope — and there, a
// visibly fake string is the safer artifact: a reader cannot copy it into a connection, whereas
// a plausible `8666746580` in a provider-less context is exactly the wrong claim to publish to
// the Meta half of the callers. The description on `id` carries both formats, which is where
// the contract belongs.
var AccessibleAccount = Type("accessible-account", func() {
	Attribute("id", String, "Account identifier in the ad platform's own namespace, ready to store as the connection's account_id. Google Ads: bare digits (8666746580). Meta: act_-prefixed (act_8666746580).")
	Attribute("label", String, "Human-readable account name or label")
	Required("id")
})

// MarketingEmail is one row of the HubSpot email picker. NOT an account: a HubSpot connection
// is already scoped to the portal its private-app token authenticates against, so there is
// nothing to choose there. What the caller must choose is which email to CLONE, because
// hubspotConfig.SourceEmailID is required and has no default.
var MarketingEmail = Type("marketing-email", func() {
	Attribute("id", String, "HubSpot marketing-email id, ready to pass as the campaign config's sourceEmailId", func() { Example("112233445566") })
	Attribute("name", String, "Internal email name, as it appears in the HubSpot email list")
	Attribute("subject", String, "Subject line")
	// Returned so a picker can show that a template is still a DRAFT before someone clones
	// it. Deliberately not promising ARCHIVED: HubSpot models archival as a separate
	// `archived` boolean rather than a lifecycle state, and this search does not request
	// archived rows, so they are absent from the result entirely — an absence a `state`
	// value could never express.
	Attribute("state", String, "HubSpot lifecycle state of the email (e.g. DRAFT, PUBLISHED). Archived emails are not returned at all — archival is a separate flag in HubSpot, not a state.")
	// Ordering is already applied server-side (most-recently-updated first). It is returned
	// anyway because two templates routinely share a name, and the date is what tells them
	// apart in a list.
	Attribute("updated_at", String, "Last-modified timestamp (ISO-8601)")
	Required("id")
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

// MetaAdsConnectionConfig is the second provider config where account_id is optional,
// for the same reason as GoogleAdsConnectionConfig above: a caller can create the
// connection with credentials only, defer account selection, and set the chosen id
// afterwards with PUT (discovery via GET .../connection-meta-ads/accounts, added in
// LFXV2-3062 and declared below as list-meta-ads-accounts, same pattern as Google Ads'
// list-google-ads-accounts).
// page_id stays Required — it names a Facebook page the operator already controls,
// not something the token's reachable-account list resolves, so there is nothing for
// discovery to do about it.
//
// A connection in this state stays status=active with account_id "", same caveat as
// Google Ads: "active" means enabled for credential-based operations (discovery), not
// that the credentials were verified. Readiness to CREATE a campaign is account_id being
// non-empty — the dispatch path says so with reason=account_not_selected. Status-toggle
// and metrics-read do NOT require it: both target an existing campaign by platform id and
// never read the connection's account_id, so they keep working on a connection whose
// account was cleared after the campaign was created.
var MetaAdsConnectionConfig = Type("meta-ads-connection-config", func() {
	Attribute("label", String, "Optional friendly name")
	// account_id must be the canonical Meta format act_<digits>: the Meta client
	// rejects anything else before dispatch, so a non-conforming value (e.g. "foo",
	// whitespace, or a bare number) stored on an active connection could never create a
	// campaign. Validating the same Pattern here rejects it as a 4xx at creation.
	Attribute("account_id", String, "Meta ad account ID. Optional: omit it (while still supplying credentials and page_id) to defer account selection, then list the reachable accounts with GET /projects/{project_id}/connection-meta-ads/accounts and set the chosen id with PUT.", func() {
		Example("act_193556282970417")
		Pattern(`^act_[0-9]+$`)
		// The pattern bounds shape but not length; cap the stored size so an arbitrarily
		// long numeric string can't be persisted at the 4xx boundary (real Meta ids are
		// far shorter).
		MaxLength(64)
	})
	// page_id must be a non-empty NUMERIC Facebook page id. Required alone only checks
	// presence — {"page_id":""} would still pass, be stored active, and then always
	// fail dispatch (the Meta client also rejects non-numeric page ids). The digit
	// pattern surfaces both failure modes as a 4xx at connection creation.
	Attribute("page_id", String, "Facebook page ID", func() {
		Example("123456789012345")
		Pattern(`^[0-9]+$`)
		// Bound the stored size (see account_id); real Facebook page ids are far shorter.
		MaxLength(64)
	})
	Attribute("app_id", String, "Meta app ID")
	// page_id is required at connection time: the Meta dispatcher needs it to attach
	// the promoted-object page, so an active connection without it would always fail
	// dispatch. Requiring it here surfaces the error as a 4xx at connection creation
	// rather than a silent runtime dispatch failure. account_id is deliberately NOT
	// required — see the type comment above.
	Required("page_id")
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
	// Reddit requires a conversion pixel on EVERY campaign create -- including Traffic and
	// Awareness, not only Conversions as the API docs describe (observed against the live LF
	// account, 2026-08-13). It identifies the advertiser's pixel, which is one per ad account,
	// so it belongs on the connection rather than being re-entered per campaign.
	//
	// NOT Required, and the reason is narrower than "legacy rows": this type is REQUEST-only
	// (GET returns RedditAdsConnection), so requiring it could not make an existing row
	// unreadable. What it would break is the caller — every existing integration that PUTs a
	// Reddit connection without this newly-added field would start getting a 400.
	//
	// The cost of leaving it optional is real and must be stated rather than discovered: PUT
	// is a FULL REPLACE on every provider in this API, so omitting this field CLEARS a
	// configured pixel, and the next Reddit dispatch is then refused. That is the same
	// semantic account_id and label have always had here — consistency across seven providers
	// is worth more than special-casing one field — but it is sharper for a field a caller may
	// not know exists yet. Send it on every update, or read the current value first.
	// The example is deliberately DIFFERENT from account_id's above. On the LF account the
	// two happen to share a value, and copying it here published a schema in which the
	// advertiser id is offered as the pixel id — a UI developer wiring the connection form
	// reads the generated OpenAPI, not this account's coincidence.
	Attribute("conversion_pixel_id", String, "Reddit conversion pixel ID (Reddit Ads → Events Manager)", func() {
		Example("a2_1b3c5d7e9f")
	})
	Required("account_id")
})

var RedditAdsConnection = Type("reddit-ads-connection", func() {
	commonConnectionAttrs()
	// Surfaced on the read model the way google-ads surfaces login_customer_id: it is
	// non-secret configuration, and without it a caller cannot tell a connection that has a
	// pixel from one that does not -- which is the difference between a connection that can
	// create campaigns and one that cannot.
	Attribute("conversion_pixel_id", String, "Reddit conversion pixel ID")
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
	// account_id is the X Ads account identifier — an ALPHANUMERIC handle (e.g. "8r7gb"),
	// NOT numeric-only. The twitter client rejects anything outside ^[A-Za-z0-9]+$ before
	// dispatch, so a non-conforming value stored on an active connection could never
	// create a campaign; validating the same Pattern here rejects it as a 4xx at creation.
	// MaxLength bounds the stored size (real X account ids are far shorter).
	Attribute("account_id", String, "X/Twitter Ads account ID (alphanumeric handle)", func() {
		Example("8r7gb")
		Pattern(`^[A-Za-z0-9]+$`)
		MaxLength(64)
	})
	// funding_instrument_id is REQUIRED by the X Ads campaign-create flow (the client
	// rejects an empty value) and is interpolated into the account-scoped request path,
	// so — like account_id — it is restricted to the safe alphanumeric charset (a value
	// with '/', '?', whitespace, … could redirect the POST). Requiring + pattern-checking
	// it here rejects a missing/malformed value as a 4xx at connection creation instead of
	// an asynchronous dispatch failure.
	Attribute("funding_instrument_id", String, "X/Twitter funding instrument id (alphanumeric)", func() {
		Example("lygyi")
		Pattern(`^[A-Za-z0-9]+$`)
		MaxLength(64)
	})
	Required("account_id", "funding_instrument_id")
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

	// Account discovery is declared per provider rather than inside connectionMethods:
	// only providers whose dispatcher implements the AccountLister interface have one, and
	// a generated method for a provider that cannot answer it would be a 400 by
	// construction.
	//
	// Google Ads, Meta, LinkedIn, Microsoft and X have one. Reddit does not: its platform
	// client has no ListAdAccounts, so its account id stays hand-entered on the connection
	// until one is built. Do not add a method here for it without the dispatcher side —
	// the endpoint would exist and always fail.
	Method("list-google-ads-accounts", func() {
		Description("Enumerate the Google Ads ad accounts accessible via the stored connection credential.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Required("project_id")
		})
		Result(func() {
			// The example lives on the METHOD's result, not on the shared element type.
			// Goa fabricates lorem-ipsum ("Iste et aspernatur delectus.") for any attribute
			// left without one, so removing the type-level example did not stop an example
			// being published — it only stopped a TRUE one being published. Anchoring it
			// here gives each provider its own id format without splitting the element type,
			// which the shared listAccounts handler depends on.
			Attribute("accounts", ArrayOf(AccessibleAccount), func() {
				Example([]map[string]any{
					{"id": "8666746580", "label": "Linux Foundation"},
					{"id": "1234567890", "label": "CNCF"},
				})
			})
			Required("accounts")
		})
		Error("NotFound", NotFoundError, "Resource not found")
		Error("BadRequest", BadRequestError, "Bad request")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/connection-google-ads/accounts")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("BadRequest", StatusBadRequest)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("list-meta-ads-accounts", func() {
		Description("Enumerate the Meta ad accounts accessible via the stored connection credential. " +
			"Returns act_-prefixed account ids, ready to store as the connection's account_id. " +
			"Accounts Meta reports as disabled, unsettled or closed are included with the reason " +
			"in their label rather than filtered out, so the caller can see why an account they " +
			"expected cannot be used.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Required("project_id")
		})
		Result(func() {
			// Meta's own ids, for the reason given on the Google Ads method above: the
			// `act_` prefix is what this method's description promises and what Meta's
			// connection validation accepts.
			Attribute("accounts", ArrayOf(AccessibleAccount), func() {
				Example([]map[string]any{
					{"id": "act_8666746580", "label": "Linux Foundation"},
					{"id": "act_1234567890", "label": "CNCF (unsettled)"},
				})
			})
			Required("accounts")
		})
		Error("NotFound", NotFoundError, "Resource not found")
		Error("BadRequest", BadRequestError, "Bad request")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/connection-meta-ads/accounts")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("BadRequest", StatusBadRequest)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("list-linkedin-ads-accounts", func() {
		Description("Enumerate the LinkedIn ad accounts accessible via the stored connection credential. " +
			"Returns bare numeric account ids, ready to store as the connection's account_id.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Required("project_id")
		})
		Result(func() {
			// Per-provider example, for the reason spelled out on list-google-ads-accounts:
			// Goa fabricates lorem-ipsum for an attribute with no example, so omitting one
			// publishes a false id format rather than none. LinkedIn ids are bare digits.
			Attribute("accounts", ArrayOf(AccessibleAccount), func() {
				Example([]map[string]any{
					{"id": "507404993", "label": "Linux Foundation [USD]"},
					{"id": "512233445", "label": "CNCF [USD] — on billing hold"},
				})
			})
			Required("accounts")
		})
		Error("NotFound", NotFoundError, "Resource not found")
		Error("BadRequest", BadRequestError, "Bad request")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/connection-linkedin-ads/accounts")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("BadRequest", StatusBadRequest)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("list-microsoft-ads-accounts", func() {
		Description("Enumerate the Microsoft Advertising accounts accessible via the stored connection " +
			"credential, across every customer the credential can reach. Returns account ids as " +
			"digits, ready to store as the connection's account_id; the label carries Microsoft's " +
			"human-facing account number, which is what its own UI shows.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Required("project_id")
		})
		Result(func() {
			Attribute("accounts", ArrayOf(AccessibleAccount), func() {
				Example([]map[string]any{
					{"id": "1234567", "label": "Linux Foundation (X1234567)"},
					{"id": "7654321", "label": "CNCF (X7654321) — suspended"},
				})
			})
			Required("accounts")
		})
		Error("NotFound", NotFoundError, "Resource not found")
		Error("BadRequest", BadRequestError, "Bad request")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/connection-microsoft-ads/accounts")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("BadRequest", StatusBadRequest)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("list-twitter-ads-accounts", func() {
		Description("Enumerate the X/Twitter Ads accounts accessible via the stored connection " +
			"credential. Returns account ids as the alphanumeric handle X uses, ready to store as " +
			"the connection's account_id. Accounts that are under review, rejected or deleted are " +
			"RETURNED with the reason in the label rather than hidden, so a caller whose only " +
			"account is unusable sees it and why.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Required("project_id")
		})
		Result(func() {
			// Per-provider example, for the reason spelled out on list-google-ads-accounts:
			// X's ids are alphanumeric handles, not digits, so the shared element type's
			// example would misdescribe the one field a caller has to store.
			Attribute("accounts", ArrayOf(AccessibleAccount), func() {
				Example([]map[string]any{
					{"id": "18ce54d4x5t", "label": "Linux Foundation"},
					{"id": "8r7gb", "label": "CNCF — under review"},
				})
			})
			Required("accounts")
		})
		Error("NotFound", NotFoundError, "Resource not found")
		Error("BadRequest", BadRequestError, "Bad request")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/connection-twitter-ads/accounts")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("BadRequest", StatusBadRequest)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("list-hubspot-emails", func() {
		Description("Search the marketing emails reachable via the stored HubSpot connection, " +
			"most-recently-updated first. This is a TEMPLATE picker, not an account picker: a " +
			"HubSpot connection is already scoped to the portal its private-app token " +
			"authenticates against, but staging an email campaign clones a caller-specified " +
			"source email (sourceEmailId is required and has no default), so the caller has to " +
			"be able to find one.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			// Optional: an absent query lists recent emails, the useful first screen for a
			// picker. The CLIENT matches name OR subject case-insensitively across every page —
			// HubSpot's list endpoint is not queried by name or subject, and `q` never goes
			// upstream (`SearchEmails` sends only limit, sort, includedProperties and after).
			//
			// That is why the walk and the maxUnfilteredEmails cap exist at all. Reading this as
			// a server-side search parameter invites optimising the walk away, which
			// reintroduces the false absence those guards prevent.
			//
			// The cap is part of the CONTRACT, not an implementation detail, because this
			// endpoint has no pagination fields: without it a caller cannot tell a complete
			// portal listing from a silently truncated first screen, and has no way to learn
			// that older templates are reachable only by searching.
			Attribute("q", String, "Substring matched against email name and subject, case-insensitively. A search walks every page, so a match is never missed. Omit to list recent emails instead: that listing is capped at 500 and sorted most-recently-updated first. WHICH 500 depends on the provider, because the walk stops once it has enough — so it is NOT guaranteed to be the 500 newest in the portal, only the newest of what was read. There is no paging; reach an older template by searching for it.", func() {
				Example("KubeCon")
			})
			Required("project_id")
		})
		Result(func() {
			Attribute("emails", ArrayOf(MarketingEmail), func() {
				Example([]map[string]any{
					{"id": "112233445566", "name": "KubeCon EU 2026 — announce", "subject": "KubeCon EU 2026 registration is open", "state": "PUBLISHED", "updated_at": "2026-08-01T17:04:00Z"},
				})
			})
			Required("emails")
		})
		Error("NotFound", NotFoundError, "Resource not found")
		Error("BadRequest", BadRequestError, "Bad request")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/connection-hubspot/emails")
			Param("q")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("BadRequest", StatusBadRequest)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})
})
