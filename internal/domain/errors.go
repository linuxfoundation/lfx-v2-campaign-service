// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package domain holds the core domain model, port interfaces, and sentinel
// errors for the campaign service. It has no infrastructure dependencies.
package domain

import "errors"

// Sentinel errors returned by repositories and mapped to HTTP status codes at
// the service/handler boundary.
var (
	// ErrNotFound indicates the requested resource does not exist (or has been
	// soft-deleted). Maps to 404.
	ErrNotFound = errors.New("resource not found")

	// ErrConflict indicates a uniqueness violation — for connections, that the
	// project already holds a connection for this provider (singleton). Maps to
	// 409.
	ErrConflict = errors.New("resource already exists")

	// ErrStaleApproval indicates the approve→dispatch guard fired: the brief was
	// no longer approved at the expected version when the job was created (a
	// concurrent replace/archive committed in the window, or it lost approval).
	// It is distinct from ErrConflict (a uniqueness violation) because the client
	// remedy differs — refresh and re-approve, then retry — even though both map
	// to 409. Maps to 409.
	ErrStaleApproval = errors.New("brief is no longer approved at the expected version")

	// ErrAudienceBuildInFlight indicates another build for the same (brief, platform)
	// is already in progress — the 'building' row holds the lease (migration 000018).
	// Distinct from ErrConflict, which both map to 409, because the remedy is
	// different and the generic "resource already exists" is actively misleading here:
	// nothing the caller asked for exists yet, and the answer is to wait for the
	// in-flight build rather than to change the request. A build that DIED holding the
	// lease reports the same thing, which is intended — its HubSpot lists exist, so the
	// operator must reconcile the portal and fail the row rather than build again.
	// Maps to 409.
	ErrAudienceBuildInFlight = errors.New("an audience build for this brief and platform is already in progress")

	// ErrPreconditionFailed indicates an optimistic-concurrency version
	// mismatch on a conditional update (stale If-Match). Maps to 412.
	ErrPreconditionFailed = errors.New("version precondition failed")

	// ErrToggleUnsupported indicates the campaign's platform has no status-toggle
	// capability wired (no dispatcher, or the dispatcher is not a StatusToggler).
	// The platform is never contacted. Maps to 400. Lives here (not in the service
	// layer) so a platform dispatcher can return it directly without importing the
	// orchestration layer, and the service still maps it to an HTTP status.
	ErrToggleUnsupported = errors.New("status toggle is not supported for this platform")

	// ErrCampaignNotProvisioned indicates a status toggle was requested on a
	// campaign that is not fully provisioned for the requested change — no upstream
	// platform campaign id yet, or (on ACTIVATE) a missing child ad group/ad so the
	// tree cannot be made servable. It is a client/state error: the platform is
	// never contacted, and a retry now would fail the same way. Maps to 409.
	ErrCampaignNotProvisioned = errors.New("campaign is not fully provisioned for this status change")

	// ErrMetricsUnsupported indicates the campaign's platform has no metrics-read
	// capability wired (no dispatcher, or the dispatcher is not a MetricsReader).
	// The platform is never contacted. Maps to 400. Lives here for the same reason
	// as ErrToggleUnsupported: a platform dispatcher can return it directly without
	// importing the orchestration layer.
	ErrMetricsUnsupported = errors.New("metrics reads are not supported for this platform")

	// ErrMetricsWindowUnsupported indicates the requested window is one of the seven
	// closed model.MetricsWindow values but this platform's MetricsReader does not
	// support it (e.g. X Ads caps windows at 7 days and rejects last_30_days). This is
	// caller input, not an upstream failure — the platform is never contacted (X's
	// client validates before making a request) or the platform rejects synchronously.
	// Maps to 400, distinct from ErrMetricsUnsupported (platform has no MetricsReader
	// at all) — this platform IS a MetricsReader, just not for this window. A platform
	// adapter wraps its own typed "unsupported window" error with this sentinel
	// (%w) so the service layer can map it without importing every platform package.
	ErrMetricsWindowUnsupported = errors.New("this window is not supported for the campaign's platform")

	// ErrNoMetricsInWindow indicates the platform answered successfully and reported no
	// data for this campaign in this window. It is NOT an upstream failure, so the 503
	// default would be a false outage report, and it is NOT zeros, because the adapter
	// that raises it cannot tell "no activity" from "no such campaign in scope" — see
	// hubspot.ErrNoSentEmailInWindow, which reasons that out at length.
	//
	// The email channel makes this the ORDINARY case rather than an edge one: Dispatch
	// stages the cloned email as a DRAFT for a human to send, so every metrics read
	// between staging and the send lands here. Maps to 409 — the campaign's state, not
	// the platform's health, is why there is nothing to return.
	ErrNoMetricsInWindow = errors.New("the platform reported no data for this campaign in the requested window")

	// ErrCampaignAccountMismatch indicates the campaign was created under one platform
	// tenant — a Google Ads customer, a HubSpot portal — but the project's CURRENT
	// connection for that platform resolves to a different one, or records none at all.
	// Platform campaign ids are unique only WITHIN a tenant, so a tenant-scoped request
	// issued under the wrong one is not merely unauthorized — it is silently WRONG: the
	// id most often matches nothing (indistinguishable from a campaign with genuinely
	// zero activity) and, on a collision, matches somebody else's campaign. On HubSpot the
	// mismatch is caught only AFTER the token's own portal has been resolved via
	// AuthenticatedPortalID, so "the platform is never contacted" no longer holds for
	// every path to this sentinel — what is true of all of them is that the tenant-scoped
	// campaign metrics read itself is never attempted. It is a state error, not a
	// transport one — a retry now fails identically — so it maps to 409, not 503.
	//
	// "Tenant" rather than "ad account" deliberately: this sentinel reaches the email
	// channel too, where the operator has no ad account to reconnect and being told to
	// go find one is a wrong instruction, not just an imprecise word.
	ErrCampaignAccountMismatch = errors.New("the campaign belongs to a different platform account than the project's current connection")

	// ErrCampaignProvenanceUnknown indicates a campaign row records NO creating tenant at
	// all — not a mismatch, an absence. It is joined with ErrCampaignAccountMismatch (never
	// returned alone) so existing errors.Is(err, ErrCampaignAccountMismatch) callers keep
	// matching, while a handler that wants to tell the two apart can check this sentinel
	// first.
	//
	// The remedy differs from an actual mismatch: "reconnect the original account" tells
	// the operator to point the connection back at a tenant this row never named, which
	// they cannot do — there is nothing recorded to reconnect to. The only way to give this
	// row a provenance is to re-dispatch it, which is what a row written before provenance
	// tracking existed, or under a connection with no tenant id at all, needs.
	ErrCampaignProvenanceUnknown = errors.New("the campaign does not record which platform tenant it was created under")

	// ErrCampaignWriteInProgress indicates another writer already holds the claim for this
	// campaign, so this request did not acquire it. Maps to 409.
	//
	// This is why the claim is a TRY and not a wait. The winning claim is held across the
	// ad-platform call — up to 45 seconds — and it holds a pooled connection for that whole
	// span. A blocking pg_advisory_lock would make every loser hold a SECOND pooled
	// connection for the same span, so a small burst against one campaign could exhaust a
	// finite pool and stall unrelated requests and the readiness probe. Failing fast keeps
	// contention costing one connection per campaign rather than one per request.
	//
	// Distinct from ErrPreconditionFailed: the caller's ETag may be perfectly current. The
	// correct client response is to retry shortly, not to refetch and rebuild the request.
	ErrCampaignWriteInProgress = errors.New("another write to this campaign is already in progress")

	// ErrEmailSearchUnsupported indicates the platform has no email-search capability
	// wired. The platform is never contacted.
	//
	// `Orchestrator.SearchEmails` returns it when the platform's dispatcher does not
	// implement the service-side `EmailSearcher` interface, and the handler maps it to 400 —
	// as with account discovery, asking a platform this service cannot search is a caller
	// error, not a transient upstream failure. Distinct from ErrAccountsUnsupported because
	// the two capabilities are independent: HubSpot searches emails and has no ad accounts
	// to enumerate, while Google Ads and Meta list accounts and search no emails. Named
	// providers rather than "every ad platform": only those two implement AccountLister —
	// LinkedIn, Reddit, X and Microsoft implement neither capability — so the sweeping form
	// was false when written, and the independence argument never needed it.
	ErrEmailSearchUnsupported = errors.New("email search is not supported for this platform")

	// ErrAccountsUnsupported indicates the platform has no account-listing capability
	// wired. The platform is never contacted.
	//
	// `Orchestrator.ReadAccounts` returns it when the platform's dispatcher does not
	// implement the service-side `AccountLister` interface, and the account-discovery
	// handler maps it to 400 — a request for a platform this service cannot enumerate is
	// a caller error, not a transient upstream failure.
	//
	// It lives here rather than in internal/service for the same reason as
	// ErrToggleUnsupported: a platform dispatcher must be able to return it without
	// importing the orchestration layer.
	ErrAccountsUnsupported = errors.New("account discovery is not supported for this platform")

	// ErrKeyUnavailable indicates this service could not obtain the JWT signing keys
	// (Heimdall's JWKS) needed to check a bearer token. It is NOT a verdict on the token:
	// nothing was learned about it, because it was never checked.
	//
	// It lives here rather than in internal/infrastructure/auth for the same reason
	// ErrConnectionNotUsable does — the service layer classifies without importing the
	// package that produced the failure. auth.Verifier wraps it (%w).
	//
	// Maps to 503, not the 400 every token-side refusal gets. A JWKS outage is reachable
	// on a cold cache and at every 5-minute TTL expiry, and answering it with "invalid
	// bearer token" tells a caller holding a perfectly good credential that theirs is
	// bad — and 400 tells them not to retry a condition that clears on its own.
	ErrKeyUnavailable = errors.New("the token signing keys could not be retrieved")

	// ErrConnectionNotUsable indicates the project's stored connection cannot be used as
	// it stands: it is not active, its credential blob is incomplete or undecodable, or a
	// stored config value is malformed. The ad platform is never contacted.
	//
	// It exists to keep these OUT of the retryable bucket. Without it, the discovery
	// handler's default arm answers 503 for a connection that is missing a refresh token
	// or carries a dashed `login_customer_id` — telling the caller to retry a request that
	// cannot succeed until someone edits the connection, and burying the one thing that
	// would let them fix it. A 503 is a promise that waiting might help; none of these
	// improve with time.
	//
	// Distinct from ErrNotFound (no connection at all — 404) and from an upstream failure
	// (the platform was reached and did not answer — 503). Platform adapters wrap their
	// own typed setup errors with this sentinel (%w), the same arrangement as
	// ErrMetricsWindowUnsupported, so the service layer classifies without importing every
	// platform package.
	ErrConnectionNotUsable = errors.New("the stored connection is not usable as configured")

	// ErrSystemConnectionNotUsable marks a defect in the LF-owned SYSTEM connection rather
	// than in the project's own. It is wrapped alongside ErrConnectionNotUsable, not
	// instead of it, so nothing that merely asks "was this refused before the platform?"
	// has to learn about it.
	//
	// The two need separating because the remedy has a different owner. A project whose
	// own connection is broken is told to edit it — correct, and it can. A project with NO
	// connection falls back to the system scope, which no request can address
	// (rejectSystemScope) and no project can edit, so the same 400 sends the caller to fix
	// something that is neither theirs nor reachable, while the operator who could fix it
	// hears nothing. This is the operator's page, so it is a 5xx and an ERROR log.
	ErrSystemConnectionNotUsable = errors.New("the LF system connection is not usable as configured")

	// ErrSystemConnectionOrigin records WHICH ROW the credentials came from, independently
	// of how the failure is classified. ErrSystemConnectionNotUsable answers a different
	// question — who has to fix it — and the two do not coincide: a blob that fails
	// authenticated decryption is not a usability defect and gets neither marker, yet it
	// is still the system row that failed. The operator log for that arm asks whether one
	// row or every connection is broken, so naming the CALLER's project there sends whoever
	// is paged to inspect a row that project does not have.
	ErrSystemConnectionOrigin = errors.New("credentials came from the LF system connection")

	// ErrCredentialsMalformed indicates the stored credential blob is structurally
	// invalid — the Encryptor could not even ATTEMPT to authenticate it (for the
	// AES-GCM implementation: shorter than a nonce PLUS the authentication tag, since
	// Seal appends a tag of Overhead() bytes to every message including an empty one,
	// so anything below that minimum is provably truncated and no key could open it).
	// Getting this boundary wrong is not cosmetic: a blob between those two lengths
	// falls through to Open, fails authentication, and is then classified as the
	// deployment-key condition below — one truncated row would send someone to look at
	// the application key. That is proven bad ROW data: the
	// row must be re-saved before this connection can work again, and nothing about
	// the deployment is wrong. The credential-resolution path wraps it with
	// ErrConnectionNotUsable, so it reaches the caller as a 400.
	ErrCredentialsMalformed = errors.New("the stored credential blob is malformed")

	// ErrCredentialDecryptionFailed indicates a well-formed blob that failed
	// authenticated decryption. It is deliberately NOT ErrConnectionNotUsable: a GCM
	// authentication failure means a wrong or ROTATED APPLICATION KEY, or tampered or
	// corrupted data (internal/infrastructure/crypto/aesgcm.go states both).
	//
	// GCM CANNOT TELL THOSE TWO APART, and the blast radius differs: a wrong deployment
	// key fails every project's connection in the same instant, while one tampered or
	// corrupted row fails exactly that row. The tag check returns the same failure
	// either way, so this sentinel means "authentication failed, blast radius not yet
	// determined" — never "the deployment is broken". A responder's FIRST question is
	// which of the two it is, and the cheap discriminator is the count: one project
	// failing is a row, every project failing at once is the key.
	//
	// It still maps to 500 rather than 400 because the ambiguity is asymmetric, not
	// because the deployment case is the likely one. Answering 400 would tell an
	// operator to go fix a connection that may be entirely fine and would suppress the
	// only signal that surfaces a key rotation gone wrong; answering 500 for a single
	// tampered row over-escalates one project. Over-escalating is the recoverable
	// direction.
	//
	// It is also the DEFAULT for an unrecognised decrypt failure. An Encryptor that
	// proves nothing about the row must not be read as proving the row is at fault:
	// mistaking an outage for user error is the expensive direction of this call.
	ErrCredentialDecryptionFailed = errors.New("stored credentials could not be decrypted")

	// The sentinels below name WHICH stored-connection defect made a connection
	// unusable. Each is wrapped ALONGSIDE ErrConnectionNotUsable at the point the defect
	// is detected, so the HTTP status is decided by that one sentinel while the reason
	// stays machine-readable.
	//
	// They exist for the log line, and the log line is the reason they must be sentinels
	// rather than message text. The status they accompany is not fixed — discovery answers
	// 400 and the synchronous campaign handlers answer 409 — so what an operator needs from
	// these is WHICH defect was rejected, independent of how it was reported. The errors
	// themselves cannot be logged: one of them is produced by
	// decoding the DECRYPTED credential blob, and an error derived from plaintext must
	// never reach centralized logs. `errors.Is` over a fixed vocabulary carries the
	// diagnosis with no payload attached to carry secrets in.

	// ErrConnectionInactive — the connection row exists and its credentials may be fine,
	// but its status is not "active". Nothing was validated beyond that.
	ErrConnectionInactive = errors.New("the stored connection is not active")

	// ErrCredentialsAbsent — the connection row exists but its credential column is
	// EMPTY. Nothing was decrypted because there was nothing to decrypt.
	//
	// It needs its own sentinel rather than riding on ErrConnectionNotUsable alone
	// because the reason vocabulary is what an operator greps: without it this known,
	// fully-diagnosed condition logs as "unclassified", which reads as "we do not know",
	// and the one state that is trivially fixable becomes the one that looks mysterious.
	// It is also the state a half-finished connection sits in, so it is not rare.
	//
	// Distinct from ErrCredentialsMalformed (bytes present, structurally unopenable) and
	// from ErrCredentialsIncomplete (decrypted and decoded, one field empty): only this
	// one means the operator never got as far as saving a credential.
	ErrCredentialsAbsent = errors.New("the stored connection has no credentials")

	// ErrCredentialsUndecodable — the decrypted blob is not valid JSON for the platform's
	// credential shape. Its cause is DERIVED FROM PLAINTEXT and is deliberately dropped
	// at the point of detection rather than wrapped; see the producing validator.
	ErrCredentialsUndecodable = errors.New("the stored credential blob could not be decoded")

	// ErrCredentialsIncomplete — the blob decoded, but a required field is empty.
	// Deliberately does not name which: the field names are a fixed, non-secret list that
	// belongs in the API response, and naming the missing one per project adds nothing a
	// log reader can act on.
	ErrCredentialsIncomplete = errors.New("the stored credentials are missing a required field")

	// ErrProviderConfigInvalid — a non-secret provider_config column holds a value the
	// platform will not accept (a dashed login_customer_id, say). Distinct from the
	// credential cases because the fix is a different form field.
	ErrProviderConfigInvalid = errors.New("a stored provider config value is invalid")

	// ErrAccountNotSelected — the connection is complete except that no ad account has
	// been chosen. Every other sentinel here describes something WRONG with stored state;
	// this one describes state that is merely UNFINISHED, and it is the only one a caller
	// reaches by doing exactly what the API told them to do.
	//
	// It became a SUPPORTED state when GoogleAdsConnectionConfig dropped
	// Required("account_id") to allow credentials-first bootstrap (design/connection.go),
	// and MetaAdsConnectionConfig now does the same — it requires only page_id, so a Meta
	// connection can be created with credentials alone and its account chosen afterwards
	// from GET .../connection-meta-ads/accounts. Do not read this sentinel as Google-only:
	// it is the shared name for an unfinished connection on every provider that supports a
	// credentials-first create, and the list is expected to keep growing.
	//
	// On Google Ads it was not, however, previously impossible: Required checked only that the JSON key
	// was present (the generated validator was `if body.AccountID == nil`) and the Go field
	// is a plain string, so `"account_id": ""` was accepted and stored. The guard that
	// produces this sentinel was therefore reachable before — via an unintended, unnamed
	// state — and it returned a bare error carrying no sentinel at all. That is precisely
	// the shape of defect this vocabulary exists to prevent: with no sentinel the condition
	// fell to the default arm and reported 503, telling an operator to wait for a state that
	// changes only when a human picks an account. Bootstrap did not create the defect; it
	// made it the common path and gave the state a name.
	//
	// It is wrapped ALONGSIDE ErrConnectionNotUsable, and the two have distinct jobs:
	// ErrConnectionNotUsable selects the HTTP status, this one supplies the reason token
	// (unusableConnectionReason -> "account_not_selected") and the specific message.
	//
	// It reaches two SYNCHRONOUS handlers, the campaign status toggle and the per-campaign
	// metrics read (internal/service/brief.go), both of which answer 409: the campaign is
	// the resource there, and an unfinished connection is a precondition conflict, matching
	// how those handlers already classify ErrCampaignNotProvisioned. Non-retryable is the
	// property that actually matters and the one 503 got wrong. Those two are fed by the
	// credential resolution behind ToggleStatus and ReadMetrics — Google Ads, Microsoft,
	// Reddit and Twitter all tag the sentinel there.
	//
	// 409 is NOT this sentinel's universal fate, and code that maps errors must not assume
	// it is. Meta tags it from requireMetaAccountID (internal/dispatch/meta.go), whose only
	// caller is Dispatch — queued work, where dispatchPlatform collapses every dispatcher
	// error into one job-result string. On that path the sentinel is a CLASSIFICATION whose
	// reason token reaches an operator through the dispatch-failure log line and nothing
	// else; no status code is derived from it and no caller sees its text. Meta has no
	// synchronous producer at all today, because its toggle and metrics reads target the
	// campaign node by id and need no account id.
	//
	// Account discovery does NOT map this sentinel, on any provider, because no discovery
	// path produces it: Google's calls validateGoogleAdsCredentials, which deliberately
	// omits the account-id check, and Meta's resolveMetaDiscoveryClient simply never calls
	// requireMetaAccountID. Accepting an account-less connection is precisely what makes the
	// bootstrap possible, since discovery is how the operator finds the account to select.
	// Discovery's own 400 covers its other unusable states.
	ErrAccountNotSelected = errors.New("no ad account has been selected for the stored connection")

	// ErrAdoptionUnsupported indicates the platform has no campaign-adoption capability
	// wired (no dispatcher, or the dispatcher is not a CampaignAdopter). The platform is
	// never contacted. Maps to 400, exactly as ErrMetricsUnsupported and
	// ErrAccountsUnsupported do for their capabilities, and it lives here for the same
	// reason: a platform dispatcher can return it without importing the orchestration layer.
	ErrAdoptionUnsupported = errors.New("campaign adoption is not supported for this platform")

	// ErrPlatformCampaignAbsent indicates the platform answered the lookup and there is no
	// such campaign under the project's connection. Maps to 404.
	//
	// It exists to keep ABSENCE separate from UNVERIFIABLE, which is the whole safety
	// property of verify-before-bind. Every other lookup failure — a transport error, a
	// malformed row, a filter the platform did not honour — must surface as an ordinary
	// error (503), because binding is a claim about upstream reality and an unanswered
	// question is not evidence. Only this sentinel means "we asked, and the answer was no".
	ErrPlatformCampaignAbsent = errors.New("no such campaign exists on the ad platform")

	// ErrInvalidPlatformCampaignID indicates the id could not name a campaign on that
	// platform at all, so no query was issued. A PERMANENT input fault (400), never the
	// "could not verify" 503 — retrying malformed input can only fail the same way.
	ErrInvalidPlatformCampaignID = errors.New("not a valid platform campaign id")

	// ErrPlatformCampaignAlreadyBound indicates ANOTHER live row already binds the same
	// upstream campaign. Distinct from the ordinary ErrConflict this brief gets for its own
	// platform slot: the 409 has to name the OTHER brief's binding, or the caller reads
	// "already has a campaign" about a brief that does not.
	//
	// The other row need not be in the caller's project. Google Ads is one shared upstream
	// account across every foundation, so a project-scoped guard would let two projects bind
	// one live campaign; the 409 message says so without identifying the other project, which
	// the caller may not be able to see. Maps to 409.
	ErrPlatformCampaignAlreadyBound = errors.New("this platform campaign is already bound to another brief")

	// ErrAdoptionRequiresOwnConnection indicates the project has no ad-platform connection of
	// its own. Maps to 409.
	//
	// It does NOT mean the credentials resolved to the shared LF system account: adoption calls
	// credsSource.resolveOwned, which consults the project scope alone, so the LF row is never
	// loaded on this path. The sentinel means the project-scoped lookup found nothing, and the
	// fallback that every other platform call would have taken was declined rather than taken
	// and rejected — see internal/dispatch for why resolving it first would misreport a broken
	// LF row as this project's problem.
	//
	// Every other platform call names a campaign the project already has a ROW for, and the
	// row is the authorization. Adoption's caller names an ARBITRARY upstream id, so under the
	// system fallback — where many projects share ONE account — it would let any project bind,
	// meter and pause a campaign another project created there. Upstream metadata cannot settle
	// ownership either: names, labels and budgets are all set by whoever created the campaign.
	// The refusal costs nothing real, because a project with no ad account of its own has no
	// campaign of its own to adopt.
	ErrAdoptionRequiresOwnConnection = errors.New("adoption requires a connection owned by this project")
)
