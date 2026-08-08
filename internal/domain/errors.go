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

	// ErrCampaignAccountMismatch indicates the campaign was created under one ad
	// account but the project's CURRENT connection for that platform resolves to a
	// different one. Platform campaign ids are unique only WITHIN an account, so an
	// account-scoped request issued under the wrong account is not merely unauthorized —
	// it is silently WRONG: the id most often matches nothing (indistinguishable from a
	// campaign with genuinely zero activity) and, on a collision, matches somebody
	// else's campaign. The platform is never contacted. It is a state error, not a
	// transport one — a retry now fails identically — so it maps to 409, not 503.
	ErrCampaignAccountMismatch = errors.New("the campaign belongs to a different ad account than the project's current connection")

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

	// The five sentinels below name WHICH stored-connection defect made a connection
	// unusable. Each is wrapped ALONGSIDE ErrConnectionNotUsable at the point the defect
	// is detected, so the HTTP status is decided by that one sentinel while the reason
	// stays machine-readable.
	//
	// They exist for the log line, and the log line is the reason they must be sentinels
	// rather than message text. An operator debugging a 400 needs to know which of these
	// five it was, but the errors themselves cannot be logged: one of them is produced by
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
)
