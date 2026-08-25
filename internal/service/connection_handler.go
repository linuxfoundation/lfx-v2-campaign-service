// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"

	"goa.design/goa/v3/security"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// JWTAuth verifies the bearer token and records the authenticated actor in the
// context for attribution.
//
// It used to base64-decode the payload and believe it. Heimdall does validate
// the token at the gateway, and every route is covered there (the chart's parity
// test fails the build if one is not) — but that guarantee ends at the cluster
// boundary, and these claims are written to created_by/updated_by. Trusting them
// meant anything able to reach the pod directly could name whoever it liked as
// the principal who authorized paid ad spend. Verification now happens here too,
// against Heimdall's JWKS.
func (s *ConnectionService) JWTAuth(ctx context.Context, token string, _ *security.JWTScheme) (context.Context, error) {
	ctx, msg, unavailable := s.authenticate(ctx, token)
	switch {
	case unavailable:
		// The check could not be PERFORMED — no verifier wired, or Heimdall's JWKS is
		// unreachable. Nothing was established about the caller's token, so 401 would
		// blame a caller who may be holding a perfectly good one and send them to refresh
		// a credential that was never the problem, for an outage that clears on its own.
		return ctx, &conn.ConnServiceUnavailableError{Code: "503", Message: msg}
	case msg != "":
		// 401, not 400: the request is well-formed and it is the CREDENTIAL that must be
		// replaced. 400 conflated an expired token with an invalid payload — opposite
		// client handling (refresh and retry vs. do not retry unchanged) — and made every
		// auth failure read as a malformed-request spike in status-based alerting.
		// The message is unchanged and stays reason-independent: the status says which
		// KIND of thing is wrong, never which check failed.
		return ctx, &conn.UnauthorizedError{Code: "401", Message: msg, WwwAuthenticate: bearerChallenge}
	}
	return ctx, nil
}

// ConnectionService implements the generated connection service interface by
// delegating to the domain repository and encryptor. Per-provider methods are
// thin adapters (see connection.go) that convert the typed Goa payloads to and
// from the generic domain model and call the core helpers here.
type ConnectionService struct {
	authGuard

	// mu guards repo, enc, and orch, which can be swapped in after construction: during
	// a database cold start the service boots with nil values (every method returns 503)
	// and the container injects the live values once the pool opens. Probe/handler
	// requests read them concurrently with that swap, so access is guarded.
	//
	// The injection is TWO separate locked swaps, not one: SetBackend writes repo+enc,
	// SetOrchestrator writes orch. A reader can therefore observe repo+enc installed
	// while orch is still nil — which is why every orchestrator-backed method nil-checks
	// orch under the read lock instead of assuming a backend implies an orchestrator.
	mu   sync.RWMutex
	repo domain.ConnectionRepository
	enc  domain.Encryptor
	orch *Orchestrator
}

var (
	_ conn.Service = (*ConnectionService)(nil)
	_ conn.Auther  = (*ConnectionService)(nil)
)

// NewConnectionService constructs a ConnectionService. A nil repo is valid: it
// signals the database is not configured OR not yet ready, so every method
// returns the typed 503 ServiceUnavailable (see resolveBackend) instead of
// panicking on a nil repo. A nil orchestrator is also valid for the account-listing
// methods (they fail with 503 until it is wired).
func NewConnectionService(repo domain.ConnectionRepository, enc domain.Encryptor) *ConnectionService {
	return &ConnectionService{repo: repo, enc: enc}
}

// SetBackend swaps in (or clears) the repository and encryptor after
// construction. Used by the container to inject the database-backed repo once
// the pool opens during a cold start, flipping the connection endpoints from 503
// to live. Safe for concurrent use with the request handlers.
func (s *ConnectionService) SetBackend(repo domain.ConnectionRepository, enc domain.Encryptor) {
	s.mu.Lock()
	s.repo = repo
	s.enc = enc
	s.mu.Unlock()
}

// SetOrchestrator injects the orchestrator for account-listing operations.
// Called by the container after the orchestrator is constructed.
func (s *ConnectionService) SetOrchestrator(orch *Orchestrator) {
	s.mu.Lock()
	s.orch = orch
	s.mu.Unlock()
}

// backend returns the current repo, encryptor, and orchestrator under the read lock.
func (s *ConnectionService) backend() (domain.ConnectionRepository, domain.Encryptor, *Orchestrator) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repo, s.enc, s.orch
}

// resolveBackend returns the repo+encryptor for one request, or the typed 503
// ServiceUnavailable error when the service has no database wired (DATABASE_URL
// unset) or the pool is still coming up. Reading both once per request (rather
// than re-reading s.repo/s.enc field-by-field) avoids racing the container's
// SetBackend swap mid-handler. The routes are still mounted in the unavailable
// mode so runtime matches the published OpenAPI contract.
func (s *ConnectionService) resolveBackend() (domain.ConnectionRepository, domain.Encryptor, error) {
	repo, enc, _ := s.backend()
	if repo == nil {
		return nil, nil, &conn.ConnServiceUnavailableError{Code: "503", Message: "connection storage is unavailable"}
	}
	return repo, enc, nil
}

// resolveBackendWithOrch returns the repo, encryptor, and orchestrator for the operations that
// reach a platform through a dispatcher, or a typed 503 when any of them is unavailable. The
// orchestrator is only required by those; repo is needed by every connection operation.
//
// `operation` names what the caller was attempting, and exists because this helper now serves
// two of them. Account discovery was the first, so the message was hard-coded — and an email
// search hitting the cold-start window was told "account discovery service is unavailable",
// describing something the caller never asked for. The same reasoning as
// accountDiscovery.operation one layer up: a 503 is read by someone deciding whether to retry,
// and naming the wrong operation sends them to check the wrong thing.
func (s *ConnectionService) resolveBackendWithOrch(operation string) (domain.ConnectionRepository, domain.Encryptor, *Orchestrator, error) {
	repo, enc, orch := s.backend()
	if repo == nil {
		return nil, nil, nil, &conn.ConnServiceUnavailableError{Code: "503", Message: "connection storage is unavailable"}
	}
	if orch == nil {
		return nil, nil, nil, &conn.ConnServiceUnavailableError{Code: "503", Message: operation + " service is unavailable"}
	}
	return repo, enc, orch, nil
}

// rejectSystemScope refuses a request aimed at model.SystemProjectID, the reserved
// scope holding the LF-owned fallback credentials.
//
// Create is already closed by validateConnectionProjectSlug, so this guard exists for the
// OTHER five, deliberately permissive on project_id to keep historical UUID-keyed rows
// reachable. Without it any caller could update, re-credential or delete the account every
// unconnected project dispatches through. Those five funnel through the helpers here;
// account discovery does not, and calls this directly (see connection.go). 404 not 403:
// "forbidden" would confirm to an unauthorized caller that something is there.
func rejectSystemScope(projectID string) error {
	if projectID == model.SystemProjectID {
		return &conn.NotFoundError{Code: "404", Message: "connection not found"}
	}
	return nil
}

// forcedSystemAdsAccount reports whether LFX_FORCE_SYSTEM_ADS_ACCOUNT is on, using the same
// exact-match parse as internal/dispatch/creds.go: only the literal "true" enables it. Read per
// call rather than cached at construction because this guard is about what the environment is
// doing RIGHT NOW — a deployment that turns the flag off should stop rejecting writes without a
// restart, which is the opposite of the dispatch-side field, where a value changing mid-process
// would make two campaigns in one job resolve different accounts.
func forcedSystemAdsAccount() bool {
	return os.Getenv(constants.EnvForceSystemAdsAccount) == "true"
}

// rejectForcedSystemAccountWrite refuses to persist an ad account id onto a PROJECT's connection
// row while forced-system mode is on, which is what makes the rollout reversible.
//
// While the flag is on, account discovery resolves the LF system credential, so every id the
// picker can offer names an LF-owned ad account (see internal/dispatch/creds.go's
// resolveForcedSystem). Storing one of those onto the project's own row outlives the flag: after
// it is turned off, dispatch resolves the PROJECT's credential again but the row still points at
// the LF account id — credentials from one account, target from another. Nothing upstream
// reconciles that, so the connection is silently broken by an operation the operator believed was
// a rollback, and the spec's promise that the flag is "reversible without a code change" would be
// false.
//
// It rejects the WRITE rather than filtering the discovery response, because the id is not the
// only way one arrives: the PUT body is caller-supplied and nothing obliges it to have come from
// the picker. Closing the persistence boundary covers every route to the same row.
//
// 400 rather than 409, and the choice is forced rather than stylistic: the update endpoints
// declare BadRequest but NOT Conflict (design/connection.go's "update-" method block), and Goa
// maps an undeclared error type to a 500. A 409 here would therefore have reported an operator
// policy as a server fault. The rejected value is a field in the request body, which is what
// BadRequest describes anyway.
//
// Clearing the account id (the empty string) is deliberately still allowed. PUT is a full
// replace, so un-selecting is expressed as an absent account_id, and refusing that would trap a
// connection in whatever state the flag found it in — the opposite of reversible. The other
// fields on the row (label, provider config, credentials) are untouched by this guard; only the
// account selection is affected by which account discovery could see.
//
// currentAccountID is what the row ALREADY stores, and the guard fires on a CHANGE rather than
// on presence, which is what makes the paragraph above true rather than aspirational. account_id
// is still Required on LinkedIn, Reddit and Microsoft (design/connection.go's
// Required("account_id") on each config type, generated as a non-pointer string), so a caller
// editing only the label MUST resend the id it already stored — PUT is a full replace and
// omitting it is not an option the schema offers. A presence check therefore returned 400 for
// every update those providers can express, leaving their update endpoints dead while the flag is
// on, and forced credentials-first callers to CLEAR a selection to rename a connection. X left
// that group in LFXV2-3319 and is now credentials-first like Google Ads and Meta, so its
// account_id CAN be omitted, reaching this guard as "". What that means depends on the ROW, and
// both outcomes are correct: against a row with no selection it is the both-empty no-op below;
// against a row that HAS one it is a CLEAR, which is permitted deliberately (PUT is a full
// replace, and refusing to clear would trap the connection in the state the flag found it in).
// Do not read the roster as the reason for the change-based check: it holds for a
// credentials-first provider too, since re-sending a stored id must stay a no-op. Worse, a presence check destroyed the very
// thing it exists to protect: the project's own pre-flag account selection, which a rollback
// needs, could only be preserved by never touching the connection again.
//
// Re-sending the stored value persists nothing — the row ends with the id it started with — so
// the invariant is untouched by allowing it. What the invariant forbids is a SYSTEM-discovered
// id ARRIVING on a project row, and every route to that is a change: a new non-empty value, or a
// different one replacing the old.
//
// "Unchanged" is decided against the STORED VALUE, not against recorded provenance, and the
// choice is forced rather than preferred: model.Connection carries no provenance for the account
// selection and no column records which credential discovered it, so there is nothing to compare
// against. Adding one would be a schema change for no gain here — the stored value answers the
// only question this guard asks, which is whether this write moves the row.
//
// It is scoped to PAID-ADS providers, and the scoping is load-bearing rather than tidy.
// model.Connection.AccountID is a SHARED field: HubSpot's required account_id (its list/audience
// id) is copied into the very same struct field by CreateHubspot and UpdateHubspot. A guard
// reading that field without asking which provider owns it rejects every HubSpot create and
// update while the flag is on, blocking CRM connection setup entirely — and the id it would be
// refusing is a HubSpot list id, which no ad-account discovery ever produced and which the flag
// cannot strand. The forced path itself gates on Provider.IsPaidAds() (internal/dispatch/
// creds.go's resolve) precisely so HubSpot/email is never redirected, so rejecting HubSpot here
// would contradict FR-003.
//
// The provider is taken as a PARAMETER rather than checked at the two call sites, so the
// compiler enforces the pairing: a future provider cannot reach this guard without its own
// classification travelling with the account id. Asking IsPaidAds() rather than
// `provider != ProviderHubSpot` follows Kind()'s own guidance — an unclassified provider added
// later answers false and is left alone, rather than inheriting a paid-ads policy by default.
// forcedSystemGuardApplies is the guard's SCOPE, named once so the two places that need it cannot
// drift apart. rejectForcedSystemAccountWrite asks it whether to inspect the id at all; updateConn
// asks it whether to READ the current row, which is a database round-trip whose result the guard
// would otherwise discard. Duplicating the expression at the call site was the obvious
// alternative and is the shape that lets one copy be edited and the other left behind — costing
// either a guard that silently stops firing or a read nothing consumes.
func forcedSystemGuardApplies(provider model.Provider) bool {
	return provider.IsPaidAds() && forcedSystemAdsAccount()
}

func rejectForcedSystemAccountWrite(provider model.Provider, accountID, currentAccountID string) error {
	if !forcedSystemGuardApplies(provider) {
		return nil
	}
	// The comparison is on the TRIMMED values, and both sides are trimmed for the same reason
	// the guard trims at all: " 8666746580" and "8666746580" name one account, so treating
	// them as a change would reject a resubmission that alters nothing, while treating a
	// trim-only edit as unchanged persists no different account either way.
	//
	// It is EXACT beyond that trim, deliberately not internal/dispatch's matchesAccount, and the
	// difference is not stylistic. That helper answers "may this credential act on this
	// campaign", so it is permissive by design in two ways this guard must not be: an empty
	// creation id returns true (here, empty→non-empty is precisely the newly-set case that must
	// be refused), and it folds Meta's "act_" prefix so act_123 and 123 compare equal. Meta's
	// account_id is pinned to the canonical act_<digits> form (design/connection.go's Pattern),
	// so accepting a bare-digit variant as "unchanged" would wave through a write that DOES
	// change the stored value into a shape the schema rejects. A write either leaves the column
	// byte-identical or it does not, and only the first is a no-op.
	incoming := strings.TrimSpace(accountID)
	// Unchanged, including the both-empty case, so clearing an already-clear selection is a
	// no-op rather than a rejection.
	if incoming == strings.TrimSpace(currentAccountID) {
		return nil
	}
	// Clearing a selection stays allowed (spec, "Clearing a selection stays allowed"): PUT is
	// a full replace, so un-selecting is expressed as an absent/empty account_id, and refusing
	// it would trap a connection in whatever state the flag found it in.
	if incoming == "" {
		return nil
	}
	return &conn.BadRequestError{
		Code: "400",
		Message: "ad account selection is temporarily disabled: this deployment runs paid-ads campaigns " +
			"on the shared LF ad account, so the accounts visible to this project are not its own and " +
			"must not be saved onto its connection",
	}
}

// createConn encrypts credentials, persists a new connection, and returns the
// generic domain result. Adapters build the *model.Connection (minus
// credentials) and pass the plaintext credential JSON separately.
func (s *ConnectionService) createConn(ctx context.Context, c *model.Connection, creds any) (*model.Connection, error) {
	if err := rejectSystemScope(c.ProjectID); err != nil {
		return nil, err
	}
	// Create is guarded as well as update: a connection can be created WITH an account id in
	// one call, so guarding only the PUT would leave the same LF id persistable by a different
	// verb. Create declares BadRequest too, so the status maps identically.
	//
	// The current value passed is "" and that is not a placeholder — it is the accurate answer.
	// Create has no prior row by construction (the repository answers ErrConflict if one
	// exists), so there is no stored selection to preserve and every non-empty id in the body
	// is by definition NEWLY set. Create therefore keeps rejecting on presence, which is the
	// same rule as update's, evaluated against an empty current value.
	//
	// The consequence is deliberate and worth stating: while the flag is on, LinkedIn, Reddit
	// and Microsoft cannot be CONNECTED at all, because account_id is Required on their
	// create bodies too. X left that set in LFXV2-3319 — its account_id is now optional, so an
	// account-less X connection CAN be created while the flag is on, and the guard is satisfied
	// because "" is not a newly-set id. That is the invariant working, not a second instance of the update
	// defect — the id would be landing fresh on a project row, which is exactly the write that
	// outlives the flag. Update differs only because a value already sitting on the row is not
	// something this write is putting there.
	if err := rejectForcedSystemAccountWrite(c.Provider, c.AccountID, ""); err != nil {
		return nil, err
	}
	repo, enc, err := s.resolveBackend()
	if err != nil {
		return nil, err
	}
	plain, err := credentialJSON(creds)
	if err != nil {
		return nil, err
	}
	ct, err := enc.Encrypt(plain)
	if err != nil {
		return nil, &conn.InternalServerError{Code: "500", Message: "failed to encrypt credentials"}
	}
	c.EncryptedCredentials = ct
	created, cerr := repo.Create(ctx, c)
	return created, mapErr(cerr)
}

// getConn fetches the project's connection for a provider.
func (s *ConnectionService) getConn(ctx context.Context, projectID string, p model.Provider) (*model.Connection, error) {
	if err := rejectSystemScope(projectID); err != nil {
		return nil, err
	}
	repo, _, err := s.resolveBackend()
	if err != nil {
		return nil, err
	}
	c, gerr := repo.Get(ctx, projectID, p)
	if gerr == nil && c == nil {
		// (nil, nil) is a shape domain.ConnectionReader permits. Returning it verbatim would
		// hand the adapters a nil connection with a nil error, which they marshal as a
		// success — a 200 describing a connection that does not exist. Render it as the
		// absence it is, matching the ErrNotFound arm mapErr already produces.
		return nil, mapErr(domain.ErrNotFound)
	}
	return c, mapErr(gerr)
}

// updateConn replaces config, gated on the If-Match version.
func (s *ConnectionService) updateConn(ctx context.Context, c *model.Connection, ifMatch *string) (*model.Connection, error) {
	if err := rejectSystemScope(c.ProjectID); err != nil {
		return nil, err
	}
	repo, _, err := s.resolveBackend()
	if err != nil {
		return nil, err
	}
	version, err := parseIfMatch(ifMatch)
	if err != nil {
		return nil, err
	}
	// The forced-system guard needs the CURRENT selection to tell a changed account id from a
	// merely resent one, so the row is read before the write — but ONLY when that answer can
	// change the outcome. Gating on forcedSystemGuardApplies — the guard's OWN first line — keeps
	// the ordinary deployment (flag off, which is the default and every environment today)
	// byte-for-byte on the path it had before this guard existed: one statement, and a missing
	// row still surfacing through repo.Update's error rather than through a read that now
	// precedes the version check. An unconditional read would have made every connection update
	// on every deployment pay for a policy that is off.
	//
	// Ordering within the guarded branch is deliberate on both sides: AFTER parseIfMatch, so a
	// caller who omitted If-Match still gets 428 rather than paying for a read; and BEFORE
	// repo.Update, because a rejected write must not reach the database.
	//
	// A read error is returned as-is rather than being treated as "no current selection".
	// Defaulting to "" on failure would be the reverse of fail-closed: an unreadable row would
	// make every incoming id look newly set and turn a transient database fault into a 400
	// blaming the caller's body. mapErr renders a genuinely absent row as the 404 the update
	// would have produced anyway, and the version conflict is still the repository's to decide.
	if forcedSystemGuardApplies(c.Provider) {
		current, gerr := repo.Get(ctx, c.ProjectID, c.Provider)
		// `current == nil` is checked ALONGSIDE the error, not instead of it.
		// domain.ConnectionReader (internal/domain/port.go) does not forbid a (nil, nil)
		// return, so a reader that reports absence that way would panic here and take down
		// connection updates for every paid-ads provider while the flag is on. The same
		// branch already defends against exactly this shape in internal/dispatch/creds.go's
		// systemCreated (`err != nil || conn == nil`); treating the contract as reachable in
		// one new call site and unreachable in the other is the drift worth closing.
		//
		// A nil row is rendered as the absence it is (domain.ErrNotFound → the 404 the update
		// would have produced anyway), NOT as an empty current selection. Defaulting to "" on
		// an unreadable row is the reverse of fail-closed: it would make every incoming id
		// look newly set and turn the absence into a 400 blaming the caller's body.
		if gerr != nil {
			return nil, mapErr(gerr)
		}
		if current == nil {
			return nil, mapErr(domain.ErrNotFound)
		}
		if err := rejectForcedSystemAccountWrite(c.Provider, c.AccountID, current.AccountID); err != nil {
			return nil, err
		}
	}
	updated, uerr := repo.Update(ctx, c, version)
	return updated, mapErr(uerr)
}

// setCredential encrypts and replaces the stored credential.
func (s *ConnectionService) setCredential(ctx context.Context, projectID string, p model.Provider, creds any, by *model.Actor) error {
	if err := rejectSystemScope(projectID); err != nil {
		return err
	}
	repo, enc, err := s.resolveBackend()
	if err != nil {
		return err
	}
	plain, err := credentialJSON(creds)
	if err != nil {
		return err
	}
	ct, err := enc.Encrypt(plain)
	if err != nil {
		return &conn.InternalServerError{Code: "500", Message: "failed to encrypt credentials"}
	}
	// The repo returns the updated connection (with the bumped version) so the
	// new ETag is available; the set-credential response is 204 today, so it is
	// not emitted here — surfacing it is a small design follow-up.
	_, serr := repo.SetCredential(ctx, projectID, p, ct, by)
	return mapErr(serr)
}

// deleteConn soft-deletes the connection.
func (s *ConnectionService) deleteConn(ctx context.Context, projectID string, p model.Provider) error {
	if err := rejectSystemScope(projectID); err != nil {
		return err
	}
	repo, _, err := s.resolveBackend()
	if err != nil {
		return err
	}
	// Record who performed the soft delete for the inline audit trail, consistent
	// with Create/Update/SetCredential (connections are not indexed, so attribution
	// lives inline in updated_by).
	return mapErr(repo.Delete(ctx, projectID, p, actorFromCtx(ctx)))
}

// testConn verifies the stored credential against the provider. Upstream
// verification is not yet implemented; it reports the connection exists and is
// pending real verification (LFXV2-2556 follow-up / provider adapters).
func (s *ConnectionService) testConn(ctx context.Context, projectID string, p model.Provider) (*conn.ConnectionTestResult, error) {
	if err := rejectSystemScope(projectID); err != nil {
		return nil, err
	}
	repo, _, err := s.resolveBackend()
	if err != nil {
		return nil, err
	}
	c, err := repo.Get(ctx, projectID, p)
	if err != nil {
		return nil, mapErr(err)
	}
	// HasCredentials reads c.EncryptedCredentials, so a (nil, nil) read panics here rather
	// than reporting the absence the caller asked about.
	if c == nil {
		return nil, mapErr(domain.ErrNotFound)
	}
	msg := "connection found; upstream verification not yet implemented"
	return &conn.ConnectionTestResult{OK: c.HasCredentials(), Message: &msg}, nil
}

// ─── helpers ───

// parseIfMatch converts the If-Match header to a version. The header is
// optional in the design (so a missing value reaches the service instead of
// being rejected by the decoder as 400), letting us return the HTTP-correct
// 428 Precondition Required when it is absent, and 400 when present but
// non-numeric.
func parseIfMatch(ifMatch *string) (int64, error) {
	if ifMatch == nil || *ifMatch == "" {
		return 0, &conn.PreconditionRequiredError{Code: "428", Message: "an If-Match header is required"}
	}
	v, err := strconv.ParseInt(*ifMatch, 10, 64)
	if err != nil {
		return 0, &conn.BadRequestError{Code: "400", Message: "If-Match must be an integer version"}
	}
	return v, nil
}

// mapErr converts a domain sentinel error to the matching generated Goa error.
func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, domain.ErrNotFound):
		return &conn.NotFoundError{Code: "404", Message: "the connection was not found"}
	case errors.Is(err, domain.ErrConflict):
		return &conn.ConflictError{Code: "409", Message: "a connection for this provider already exists on the project"}
	case errors.Is(err, domain.ErrPreconditionFailed):
		return &conn.PreconditionFailedError{Code: "412", Message: "the supplied ETag does not match the current version"}
	default:
		return &conn.InternalServerError{Code: "500", Message: "an internal server error occurred"}
	}
}

// etag renders a version as its ETag string.
func etag(version int64) string { return strconv.FormatInt(version, 10) }

// credentialJSON marshals a typed credential payload for encryption, surfacing
// a marshal failure as a bad request rather than silently encrypting an empty
// object. In practice the generated credential structs always marshal, but the
// error is propagated so a failure is never masked.
func credentialJSON(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, &conn.BadRequestError{Code: "400", Message: "invalid credentials payload"}
	}
	return b, nil
}

// strVal dereferences an optional string.
func strVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// optStr returns a pointer to s, or nil if empty.
func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
