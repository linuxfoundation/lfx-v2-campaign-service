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

// PayloadTooLargeError is the 413 body for a request whose payload exceeded
// constants.MaxRequestBodyBytes and was refused by middleware.MaxBodyBytes before (or
// part-way through) being read.
//
// It is declared here even though no HANDLER ever returns it: the 413 is produced by
// middleware that sits outside the mux, so it never passes through a Goa encoder. What
// the declaration buys is the CLIENT half of the contract. Without it the generated
// client has no decode case for 413 and reports an ordinary oversized upload as
// ErrInvalidResponse — an unknown-status failure rather than the actionable "your
// payload is too large" — and the OpenAPI documents omit the behaviour entirely, so a
// consumer generating from the spec cannot know the endpoint can answer it.
//
// A SEPARATE type from BadRequestError for the reason given on UnauthorizedError: Goa
// keys a method's error encoder on the error NAME, so one type mapped to two statuses
// cannot be told apart on the wire.
var PayloadTooLargeError = Type("payload-too-large-error", func() {
	errorAttrs("413", "The request payload was too large.")
})

// UnauthorizedError is the 401 body for a token JWTAuth refused: absent, expired,
// wrongly signed, or accepted by the verifier without naming a principal.
//
// It is a SEPARATE type from BadRequestError rather than the same type mapped to a
// second status, because Goa keys a method's error encoder on the error NAME: one type
// under two names would still need two `Error(...)` declarations, and one name cannot
// carry two statuses. The two are also genuinely different answers — 400 says the
// request was malformed and must not be retried unchanged; 401 says the request was
// well-formed and the credential is what needs replacing, which is a retry after a
// refresh. A client cannot derive that split from a status it shares with payload
// validation, which is the whole reason for this type (RFC 9110 §15.5.2).
//
// `www_authenticate` is an ATTRIBUTE, not a constant emitted by the framework: Goa's
// Response-level Header() maps an attribute of the error type onto a response header,
// and has no form that writes a fixed string. Making it a field of the error is what
// gets `WWW-Authenticate: Bearer` onto the wire at all. `Required` states that the
// challenge is part of the contract — RFC 9110 §15.5.2 makes it mandatory on a 401 —
// but it is a DESIGN-TIME declaration, not a runtime guarantee on the response path:
// the generated field is a plain `string`, and the server encoder writes whatever it
// holds, so service code constructing this error with an empty value still emits a bare
// 401. What `Required` buys is the generated CLIENT decoder, which rejects a 401 whose
// `Www-Authenticate` header is empty (`goa.MissingFieldError`), plus the openapi
// contract. Populating the field is the handlers' job — every one of them fills it from
// the shared `bearerChallenge` constant, and
// TestEveryConnectionUnauthorizedEncoderSetsTheChallenge is what keeps that true.
//
// The message stays as opaque as the 400 it replaces. The status distinguishes "your
// credential is the problem" from "your payload is the problem"; it deliberately does
// NOT distinguish expired from wrongly-signed from unattributed, because that only
// tells an attacker which part to fix next. See internal/service/auth.go's
// `authenticate` and TestAuthenticate_RejectionMessagesAreOpaque.
var UnauthorizedError = Type("unauthorized-error", func() {
	errorAttrs("401", "Unauthorized.")
	Attribute("www_authenticate", String, "Authentication challenge (RFC 9110 §15.5.2)", func() {
		Example("Bearer")
	})
	Required("www_authenticate")
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

// authErrors declares the two errors a SECURED method must carry regardless of whether
// it accepts a body, and connectionAuthErrorResponses maps them.
//
// They exist as a pair for the same reason commonBriefErrors/briefErrorResponses do: a
// declared error with no Response mapping encodes exactly like an undeclared one — a 500
// — so the two are only correct if neither can be called without the other. The
// connections service declares its errors inline per method rather than through one
// `commonConnectionErrors`, because the eleven methods genuinely differ past this point
// (create has Conflict, update has the two precondition errors, the reads have NotFound).
// What they do NOT differ in is the security scheme, so the auth pair is factored out
// and the rest stays inline.
//
// Unauthorized is what JWTAuth returns now; BadRequest remains for payload and
// path-parameter validation, which the bodyless reads still reach through the generated
// decoder (create-* constrains project_id to a slug Pattern). See the type comments on
// UnauthorizedError and the note in commonBriefErrors.
func authErrors() {
	Error("BadRequest", BadRequestError, "Bad request")
	Error("Unauthorized", UnauthorizedError, "Unauthorized")
	// Declared on every method, not only the body-bearing ones: MaxBodyBytes is global
	// middleware, so any route can answer 413 if a caller sends an oversized body to it.
	Error("PayloadTooLarge", PayloadTooLargeError, "Payload too large")
}

// connectionAuthErrorResponses maps the pair authErrors declares. The Header call maps
// the error type's www_authenticate attribute onto the WWW-Authenticate response header;
// without it the challenge would serialize into the JSON body instead (RFC 9110 §15.5.2).
func connectionAuthErrorResponses() {
	Response("BadRequest", StatusBadRequest)
	Response("Unauthorized", StatusUnauthorized, func() {
		Header("www_authenticate:WWW-Authenticate")
	})
	Response("PayloadTooLarge", StatusRequestEntityTooLarge)
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
		// every method below declares the auth pair too. JWTAuth returns
		// *conn.UnauthorizedError when a token is refused, and Goa generates the error
		// encoder from THIS list: a method that omits Unauthorized has no case for it, so
		// the typed 401 falls through to the generic encoder and reaches the caller as a
		// 500 — undocumented in OpenAPI, and telling a client with an expired credential
		// to treat it as a server fault. The declaration is what makes JWTAuth's mapping
		// real, so authErrors() is required on every method carrying bearerToken(),
		// payload or no payload.
		authErrors()
		Error("Conflict", ConflictError, "A connection already exists for this provider on the project")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			POST("/projects/{project_id}/connection-" + key)
			Header("bearer_token:Authorization")
			Response(StatusCreated, func() {
				Header("etag:ETag")
			})
			connectionAuthErrorResponses()
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
		// The auth pair is declared on EVERY secured method, including the reads and the
		// delete, because JWTAuth can refuse a token — and a refusal it cannot encode
		// becomes a 500. See the comment on the create method's copy.
		authErrors()
		Error("NotFound", NotFoundError, "Resource not found")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/connection-" + key)
			Header("bearer_token:Authorization")
			Response(StatusOK, func() {
				Header("etag:ETag")
			})
			connectionAuthErrorResponses()
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
		authErrors()
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
			connectionAuthErrorResponses()
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
		// The auth pair is declared on EVERY secured method, including the reads and the
		// delete, because JWTAuth can refuse a token — and a refusal it cannot encode
		// becomes a 500. See the comment on the create method's copy.
		authErrors()
		Error("NotFound", NotFoundError, "Resource not found")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			DELETE("/projects/{project_id}/connection-" + key)
			Header("bearer_token:Authorization")
			Response(StatusNoContent)
			connectionAuthErrorResponses()
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
		// The auth pair is declared on EVERY secured method, including the reads and the
		// delete, because JWTAuth can refuse a token — and a refusal it cannot encode
		// becomes a 500. See the comment on the create method's copy.
		authErrors()
		Error("NotFound", NotFoundError, "Resource not found")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			POST("/projects/{project_id}/connection-" + key + "/test")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			connectionAuthErrorResponses()
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
		authErrors()
		Error("NotFound", NotFoundError, "Resource not found")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			POST("/projects/{project_id}/connection-" + key + "/set-credential")
			Header("bearer_token:Authorization")
			Response(StatusNoContent)
			connectionAuthErrorResponses()
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
// paragraph below MetaAdsConnectionConfig's own godoc. X is the most recent to earn both, in
// LFXV2-3319, and unlike Microsoft it was carried across both config gates in the same change.
// Of the three that remain required, Microsoft (as of LFXV2-3064) has BOTH: Reddit still lacks
// discovery, while LinkedIn gained a discovery endpoint in that ticket and is missing the
// OTHER half.
// resolveLinkedInCredentials
// does tag domain.ErrAccountNotSelected, but LinkedInDispatcher.Dispatch never calls it — the
// create path resolves inline and answers a missing account id with a bare notCreated, so the
// missing choice is never named. Microsoft, Reddit and X tag it on a path create reaches
// (validateMicrosoftConnection, resolveRedditClient, validateTwitterConnection). Naming the
// halves separately matters because the bar is the conjunction — Reddit becomes eligible
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
// LinkedIn, Microsoft and Reddit keep Required("account_id"), but no longer all for the
// same reason, and the difference is what tells you how far each is from being relaxed.
// Reddit still has NO list to choose from, so relaxing the requirement would create a
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
//     account-less SYSTEM row is installable by the bootstrap CLI.
//
// BOTH gates are per-provider and must be relaxed TOGETHER to make a provider credentials-first;
// relaxing only one leaves half the flow blocked, which is why X was carried across both in
// LFXV2-3319. X is now credentials-first on both: it holds both halves (discovery from
// LFXV2-3319, and a Dispatch that resolves through validateTwitterConnection and tags
// ErrAccountNotSelected), so it no longer appears in the list above. Microsoft holds both halves
// too and remains excluded from both gates — that exclusion is a sequencing decision, not a
// missing capability. Relaxing either gate without both halves is what the next rule forbids.
//
// Add the requirement back for any credentials-first provider — Google Ads, Meta or X — or drop
// it for another provider, only together with that provider's discovery endpoint AND its
// account_not_selected tagging. Stated as the CLASS rather than a roster: the membership grows,
// and a fixed list is what sends a later edit to treat the newest member as unrelated.
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
// the Meta half of the callers.
//
// The description on `id` describes the SHAPE of the contract — an opaque, per-provider,
// store-verbatim string — rather than enumerating one member per provider. It listed only
// Google's and Meta's forms while five providers already reused the type, so LinkedIn,
// Microsoft Ads and X/Twitter callers read a description that did not cover their id. An
// enumeration in a shared type goes stale the moment a provider is added and nothing fails when
// it does, because no test can assert prose. The shape statement stays true as providers are
// added; the per-provider FORM belongs in each method's own example, where adding a method
// forces the author to supply one.
var AccessibleAccount = Type("accessible-account", func() {
	Attribute("id", String, "Account identifier in the ad platform's OWN namespace, ready to store as the connection's account_id verbatim. The format is per-provider and is whatever that platform mints — bare digits on Google Ads, LinkedIn and Microsoft Ads; an `act_`-prefixed id on Meta; an alphanumeric handle on X/Twitter — so a caller must treat it as an OPAQUE string and must not validate, normalise or re-derive it. Each discovery method's own example shows its provider's form. Storing it unchanged is what matters: the connection validation for each provider accepts only its own format.")
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
	Attribute("access_token", String, "OAuth access token (LinkedIn: valid 60 days)")
	// Refresh is OPTIONAL, unlike Microsoft's, where refresh_token is Required.
	// LinkedIn issues programmatic refresh tokens only to approved Marketing Developer
	// Platform partners, so a connection may legitimately carry an access token alone —
	// making these Required would reject every non-MDP connection. When all three are
	// present the service renews the access token before expiry instead of failing at
	// dispatch 60 days later; when they are absent it stays bearer-only.
	// The trio is ALL-OR-NONE, enforced in the service layer
	// (validateLinkedInRefreshCredentials) rather than by Goa's Required, which cannot
	// express a conditional group. A partial set would pass CanRefresh()'s gate as
	// false and silently degrade the connection to bearer-only while the operator
	// believed renewal was configured — so it is rejected with a 400 instead.
	Attribute("refresh_token", String, "OAuth refresh token (optional; MDP-approved apps only, valid ~365 days). Must be supplied together with client_id and client_secret.")
	Attribute("client_id", String, "OAuth client id (optional; required together with refresh_token and client_secret)")
	Attribute("client_secret", String, "OAuth client secret (optional; required together with refresh_token and client_id)")
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
	// An explicit JSON `null` decodes identically to an ABSENT key (both yield a nil *string,
	// which strVal renders as ""), and that conflation is deliberate rather than overlooked.
	// It is not a new state: on these endpoints PUT is a FULL REPLACE, so an absent account_id
	// ALREADY means "clear the selection" (see rejectForcedSystemAccountWrite, which permits
	// exactly that) — there is no separate "not chosen yet" intent for null to collide with, and
	// a create has no prior selection to lose. Google Ads and Meta have behaved this way since
	// LFXV2-3061/3062 and X now matches them rather than inventing a third convention. Note
	// what is NOT relaxed: an explicit "" is a PRESENT value, so it still fails the Pattern
	// below — only the missing/null key is accepted, which is why the apivalidation table keeps
	// its empty-string case as a rejection.
	//
	// account_id is deliberately NOT required (LFXV2-3319), making X credentials-first:
	// a connection may be created with credentials only and pointed at an account after
	// GET .../connection-twitter-ads/accounts enumerates the choices. X earned this by
	// holding BOTH halves — discovery (twitter.ListAdAccounts / TwitterDispatcher.ListAccounts)
	// and a create path that NAMES the missing choice, because Dispatch itself resolves
	// through validateTwitterConnection, which tags an empty account id with
	// domain.ErrAccountNotSelected. funding_instrument_id STAYS required: it has no
	// discovery endpoint, so relaxing it would create a row nothing in this API could
	// finish — exactly the objection that keeps Reddit's account_id required.
	Required("funding_instrument_id")
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
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/connection-google-ads/accounts")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			connectionAuthErrorResponses()
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("get-google-ads-keywords", func() {
		Description("Read Google Ads keyword performance for this project's own campaigns, live from the " +
			"platform. Scoped to the campaigns this service holds for the project, NOT to the connected ad " +
			"account: the Google Ads customer is shared across foundations, so an account-wide read would " +
			"return other projects' keywords. A pure read-through — nothing is persisted, and this service " +
			"stores no keyword of its own. Rows are the TOP keywords by impressions over the window, capped; " +
			"`truncated` reports whether the project's campaigns hold more. The returned " +
			"criterion_id/ad_group_id pairs are the handles the keyword-actions endpoint takes.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("window", String, "Platform-agnostic reporting window; defaults to last_30_days when omitted", metricsWindowEnum)
			Required("project_id")
		})
		Result(GoogleAdsKeywords)
		Error("NotFound", NotFoundError, "Resource not found")
		// authErrors() rather than a hand-listed BadRequest: it also declares Unauthorized,
		// which every bearerToken() method must carry or a refused token encodes as a 500.
		authErrors()
		Error("Conflict", ConflictError, "Conflict")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/google-ads/keywords")
			Header("bearer_token:Authorization")
			connectionAuthErrorResponses()
			Param("window")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("Conflict", StatusConflict)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("get-google-ads-audience", func() {
		Description("Read Google Ads audience demographics — age, gender and device — for this project's " +
			"own campaigns, live from the platform. Scoped to the campaigns this service holds for the " +
			"project, NOT to the connected ad account, which is a Google Ads customer shared across " +
			"foundations. A pure read-through; nothing is persisted. The three " +
			"breakdowns are returned in one array discriminated by `dimension`. Each breakdown covers the " +
			"SAME traffic independently, so impressions must be totalled within a dimension, never across " +
			"them. Google's UNDETERMINED/UNKNOWN buckets are returned as-is rather than dropped: they are " +
			"real unattributed traffic, and hiding them would make the buckets silently under-sum.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("window", String, "Platform-agnostic reporting window; defaults to last_30_days when omitted", metricsWindowEnum)
			Required("project_id")
		})
		Result(GoogleAdsAudience)
		Error("NotFound", NotFoundError, "Resource not found")
		// authErrors() rather than a hand-listed BadRequest: it also declares Unauthorized,
		// which every bearerToken() method must carry or a refused token encodes as a 500.
		authErrors()
		Error("Conflict", ConflictError, "Conflict")
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/google-ads/audience")
			Header("bearer_token:Authorization")
			connectionAuthErrorResponses()
			Param("window")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			Response("Conflict", StatusConflict)
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("resolve-google-ads-campaign", func() {
		Description("Resolve one Google Ads campaign id to this service's own campaign and brief. " +
			"A caller holding a keyword row has the PLATFORM's numeric campaign id; every mutation " +
			"route here is keyed by this service's campaign UUID under its brief. Nothing else " +
			"bridges the two, so a keyword table cannot act on its own rows without this. " +
			"A pure READ: it enumerates nothing and mutates nothing, and it is scoped to the " +
			"project's own campaigns by the same `project_id` predicate the keyword and audience " +
			"reads use — so it cannot be used to discover whether ANOTHER project owns a given " +
			"upstream id, which on a shared ad account is the question that must not be answerable. " +
			"**An unowned id is 200 with an empty `matches`, not 404.** The project genuinely owning " +
			"no such campaign is an answer the caller acts on by refusing the action, and it must be " +
			"distinguishable from the route or the project being wrong, which is what a 404 would say. " +
			"**`matches` is an array, but a valid database can never return more than one entry.** Migration 000020's `uq_campaigns_platform_campaign_live` is a UNIQUE index on (platform, platform_campaign_id) over every live Google Ads row, and it is global rather than per-project — so scoping to a project can only narrow one row to zero or one. The array is DEFENSIVE against that invariant lapsing (a dropped index, a narrowed predicate, a platform added to this read but not to the index), not a claim that duplicates occur: a single-ref contract would force some layer to pick a row, and picking would mutate a campaign nobody named. A caller receiving more than one must refuse rather than choose. " +
			"This is NOT a list endpoint under rule 3: it is a keyed lookup returning the matches " +
			"for one supplied id, with no collection, pagination or filtering.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			// Digits-only for the same reason KeywordActionInput's ids are: the value is a
			// Google Ads campaign id, and admitting a non-numeric one only moves its refusal
			// to a place that classifies it less clearly.
			Attribute("platform_campaign_id", String, "The Google Ads campaign id to resolve. Digits only, no leading zero, and within int64.", func() {
				// No leading zero, and 1-19 digits. The column is TEXT, so "007" and "7" are
				// different rows: a leading-zero spelling of a real id matches nothing and comes
				// back as a 200 "unowned" answer, which reads as "this campaign is not yours"
				// rather than "that is not an id". "0" is not a Google Ads id either.
				Pattern(`^[1-9][0-9]{0,18}$`)
				MaxLength(19)
				Example("24183781329")
			})
			Required("project_id", "platform_campaign_id")
		})
		Result(PlatformCampaignResolution)
		Error("NotFound", NotFoundError, "Resource not found")
		// authErrors() rather than a hand-listed BadRequest: it also declares Unauthorized,
		// which every bearerToken() method must carry or a refused token encodes as a 500.
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		// Declared even though this method contacts no platform: `resolveBackendWithOrch`
		// returns a 503 until storage and the orchestrator are wired. That is USUALLY cold
		// start, and retrying is then right — but NOT always: in the supported no-database mode
		// NewContainer leaves both nil deliberately, so these routes stay mounted and answer
		// this same 503 for the life of the process. A caller must read it as "not available
		// yet", never as a promise that waiting will clear it. An undeclared error is encoded as
		// a 500 by the generated encoder, so a caller would otherwise see an opaque failure.
		//
		// A storage FAULT is separately a 500, not this — that one is a fault in a service that
		// is up, and retrying does not help. Cold start is the only 503 here.
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/google-ads/campaign-ref")
			Header("bearer_token:Authorization")
			connectionAuthErrorResponses()
			Param("platform_campaign_id")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
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
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/connection-meta-ads/accounts")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			connectionAuthErrorResponses()
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
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/connection-linkedin-ads/accounts")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			connectionAuthErrorResponses()
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
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/connection-microsoft-ads/accounts")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			connectionAuthErrorResponses()
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("list-twitter-ads-accounts", func() {
		Description("Enumerate the X/Twitter Ads accounts accessible via the stored connection " +
			"credential. Returns account ids as the alphanumeric handle X uses, ready to store as " +
			"the connection's account_id. Accounts that are under review or rejected are RETURNED " +
			"with the reason in the label rather than hidden, so a caller whose only account is " +
			"unusable sees it and why. DELETED accounts are a different case and are not promised: " +
			"the walk does not send `with_deleted`, so it takes X's documented default of false " +
			"and deleted accounts are normally excluded upstream — a deleted account is not a " +
			"choice. The per-row deleted flag is still honoured defensively, so a row X flags " +
			"anyway is labelled rather than passing as live.")
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
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/connection-twitter-ads/accounts")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			connectionAuthErrorResponses()
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
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/connection-hubspot/emails")
			Param("q")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			connectionAuthErrorResponses()
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("search-hubspot-campaigns", func() {
		Description("Find LF HubSpot marketing campaigns by name, to read back an existing campaign's " +
			"`hs_utm` token. " +
			"**THE NAMESPACE IS PORTAL-WIDE.** HubSpot campaigns are not scoped to a project, so this " +
			"returns every campaign in the portal the connection authenticates against, regardless " +
			"of which project scopes the path. " +
			"`project_id` gates permission AND selects WHICH portal is visible: a HubSpot connection " +
			"is stored per project with its own token and `portal_id`, and the LF system fallback is " +
			"refused for HubSpot — so two projects see the same campaigns only when they are " +
			"configured against the same portal, which is common under the LF umbrella but is not " +
			"guaranteed. The portal-wide part is a property of HubSpot's data model rather than a " +
			"gap in the scoping here, and it is why the create route below needs a warning. " +
			"The match is HubSpot's own `query` search over its default searchable properties: NOT an " +
			"exact-name lookup, and NOT relevance-ranked — the CRM v3 search API has no relevance " +
			"sort, and no `sorts` is sent, so rows arrive in HubSpot's default order (by object " +
			"creation). **Do not read the first row as the best match** — the search is token-based, " +
			"so a hit can merely share a token with the query. Every match is returned, in the order " +
			"HubSpot returned them, rather than narrowed to a best one, because choosing between " +
			"similarly-named campaigns needs a human reading the names — collapsing them here would " +
			"hide the ambiguity from the only party able to resolve it. " +
			"**An empty `campaigns` array is a 200, not a 404**: 'no campaign is named that' is the " +
			"answer a caller acts on by offering to create one, and it must be distinguishable from a " +
			"search that failed. " +
			"A campaign with no `utm` is a real result and is returned as one — an absent token does " +
			"NOT mean the campaign was not found, and treating it that way would prompt a duplicate " +
			"create. " +
			"**The result set is CAPPED at 200 and there is no paging.** A campaign ranked below the " +
			"cap is not returned, and a caller reads an absent campaign as licence to create one — so " +
			"an operator who cannot find a campaign should search a narrower term rather than assume " +
			"it does not exist. 200 is HubSpot's own per-request maximum (raised from 100 in September " +
			"2024), so the gap between \"not in " +
			"the top N\" and \"does not exist\" is as small as one request can make it. " +
			"**`capped` reports when that gap is actually open**, derived from HubSpot's own total " +
			"rather than from the returned count — an exactly-full page and a truncated one are the " +
			"same length. While it is true the caller must not offer an unqualified create. " +
			"This is NOT a list endpoint under rule 3: it is a keyed query returning the matches for " +
			"one supplied term, with no collection, pagination or filtering.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("q", String, "The campaign name to search for. Matched by HubSpot's own `query` search over its default searchable properties — not an exact-name lookup, and not relevance-ranked. Must contain a non-whitespace character.", func() {
				// MinLength(1) alone admits "   ", which the handler then refuses with a 400:
				// the contract would promise a request the service does not accept. The pattern
				// states the rule the handler already enforces, so the generated decoder and the
				// runtime agree and the published schema is what callers can rely on.
				MinLength(1)
				Pattern(`\S`)
				Example("KubeCon NA 2026")
			})
			Required("project_id", "q")
		})
		Result(func() {
			Attribute("campaigns", ArrayOf(HubSpotCampaign), "Matches in the order HubSpot returned them — by object creation, NOT by relevance, so the first row is not the best match. Empty when nothing matched.")
			Attribute("capped", Boolean, "True when the search could NOT be shown to be complete. That covers the case HubSpot reported more matches than it returned, and equally the cases where completeness is simply unknown: an absent `total`, or one that contradicts the rows (negative, or fewer than were returned). All of them fail CLOSED, because \"we cannot tell\" must not be reported as the proven absence a caller acts on by creating a campaign. While it is true, absence from `campaigns` is NOT proof the campaign does not exist, and a caller must not offer an unqualified create on an empty result — it would duplicate a campaign in a namespace shared by everyone on that HubSpot portal. Narrow the search term instead.")
			Required("campaigns", "capped")
		})
		Error("NotFound", NotFoundError, "Resource not found")
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			GET("/projects/{project_id}/connection-hubspot/campaigns")
			Param("q")
			Header("bearer_token:Authorization")
			Response(StatusOK)
			Response("NotFound", StatusNotFound)
			connectionAuthErrorResponses()
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})

	Method("create-hubspot-campaign", func() {
		Description("Create an LF HubSpot marketing campaign, returning the `hs_utm` token when " +
			"the response carries one. " +
			"**`utm` MAY BE ABSENT ON A SUCCESSFUL CREATE**, and that is not an error: the " +
			"marketing create is not documented to return the property, and this route does no " +
			"follow-up read — a second call after a non-idempotent write is another failure " +
			"point whose failure would make a campaign that EXISTS look like one that was never " +
			"created. An absent token means only that this response did not carry one, NOT that " +
			"the campaign has none configured; the ordinary lookup reads it back. What IS " +
			"required is the id: an id-less 2xx is refused as unconfirmed, because a campaign " +
			"that cannot be addressed is not a usable answer. " +
			"**THIS WRITE IS VISIBLE PORTAL-WIDE.** The campaign namespace is the whole HubSpot " +
			"portal this project's connection authenticates against, so a campaign created here " +
			"appears for everyone working in that portal however this path is scoped. WHICH portal " +
			"depends on the connection — they are stored per project with their own token and " +
			"`portal_id`, and the LF system fallback is refused for HubSpot — so this is not " +
			"necessarily every foundation, and projects on different portals do not see each " +
			"other's campaigns. A caller MUST warn before invoking it, and must not put anything " +
			"project-sensitive in the name. " +
			"**It does not check for an existing campaign first, and that is deliberate.** A " +
			"search-then-create inside one call would still race any concurrent caller and could not " +
			"prevent a duplicate; the check belongs with the human who can read the candidate names. " +
			"Search first, show the matches, create only if the operator confirms none is right. " +
			"This method always creates. " +
			"`hs_utm` is assigned by HubSpot, never supplied here, and is read back from the create " +
			"response rather than re-fetched — so the returned token is the one HubSpot actually " +
			"assigned rather than one this service guessed. " +
			"**A 2xx carrying no id is reported as an error**, because the campaign may or may not " +
			"exist and cannot be addressed either way: the caller must check HubSpot rather than " +
			"retry into a second copy. " +
			"Other failures fall into FOUR classes, and the status tells them apart. " +
			"**400 — nothing was created, and the request is correctable.** Either HubSpot rejected " +
			"it on the merits (a definite non-429 4xx), or the stored connection EXISTS but is not " +
			"usable as configured. A 401/403 says so in its own words, because retrying another " +
			"NAME cannot fix a permission problem. " +
			"**404 — no HubSpot connection is configured for this project.** Distinct from the 400 " +
			"above, which means one exists and is broken: the remedy is to connect HubSpot, not to " +
			"fix a credential. " +
			"**500 — the stored credential could not be decrypted**, or the service is otherwise " +
			"faulted BEFORE the request went out. Not the operator's to fix, and not retryable by " +
			"them. 500 is reserved for that pre-send position: a fault discovered AFTER the create " +
			"returned without error is a 503, because by then the campaign may exist and only this " +
			"service's reading of the outcome failed. " +
			"Those three prove nothing reached HubSpot, which is why they are reported as themselves " +
			"rather than as an unconfirmed outcome: sending an operator to look for a campaign that " +
			"was never attempted hides the remedy they actually need. " +
			"**503 — the outcome could not be confirmed, OR the request never left this service.** " +
			"Those two share a status because both are retryable-when-things-recover rather than " +
			"correctable by the caller, and the MESSAGE distinguishes them: a pre-send failure " +
			"(DNS, dial, an already-cancelled context) can promise nothing was created, which the " +
			"unconfirmed case cannot. Everything else lands here too, including " +
			"any failure this service cannot positively classify: a non-idempotent write into a " +
			"shared namespace fails CLOSED, so an unrecognised error is treated as possibly-committed " +
			"rather than reported as a clean failure. HubSpot marks mutating transport, 429, 3xx and " +
			"5xx failures as possibly-committed, and so is a 2xx whose body could not be decoded. " +
			"Verify in HubSpot before creating it again.")
		Payload(func() {
			bearerToken()
			projectIDAttr()
			Attribute("name", String, "The campaign name. Visible to everyone on the connection's HubSpot portal — do not include project-sensitive information. Must contain a non-whitespace character.", func() {
				// See the search `q` attribute: MinLength(1) admits a whitespace-only name that
				// the handler refuses with a 400.
				MinLength(1)
				MaxLength(255)
				Pattern(`\S`)
				Example("KubeCon NA 2026")
			})
			Required("project_id", "name")
		})
		Result(HubSpotCampaign)
		Error("NotFound", NotFoundError, "Resource not found")
		authErrors()
		Error("InternalServerError", InternalServerError, "Internal server error")
		Error("ServiceUnavailable", ConnServiceUnavailableError, "Service unavailable")
		HTTP(func() {
			POST("/projects/{project_id}/connection-hubspot/campaigns")
			Header("bearer_token:Authorization")
			Response(StatusCreated)
			Response("NotFound", StatusNotFound)
			connectionAuthErrorResponses()
			Response("InternalServerError", StatusInternalServerError)
			Response("ServiceUnavailable", StatusServiceUnavailable)
		})
	})
})
