// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package service — per-provider connection adapters.
//
// Each provider's six methods are thin adapters: they convert the typed Goa
// payload to the generic *model.Connection (plus plaintext credential JSON),
// call the shared core helpers in connection_handler.go, and convert the
// generic result back to the provider's typed Goa result. The repetitive shape
// across providers is intentional; the interesting logic lives in
// connection_handler.go.
package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// validateConnectionProjectSlug guards the connection CREATE endpoints: project_id
// must be a canonical slug, not a UUID. A connection is stored keyed by project_id and
// that value is the EXACT-MATCH key for the dispatch lookup (ConnectionRepo.Get), while
// brief/campaign create already require a slug — so a UUID-keyed connection could never
// be joined to a dispatched campaign. Reuses the shared projectSlugProblem logic and
// wraps it as a connections BadRequestError. Get/update/delete/set-credential/test stay
// permissive (historical UUID rows). The generated HTTP decoder validates the same
// Pattern/MaxLength on the create routes; this guard duplicates it for direct/non-HTTP
// callers (belt-and-suspenders), and is where the connections-flavored 400 is produced.
func validateConnectionProjectSlug(projectID string) error {
	if msg := projectSlugProblem(projectID); msg != "" {
		return &conn.BadRequestError{Code: "400", Message: msg}
	}
	return nil
}

// ─── GoogleAds ───

func (s *ConnectionService) buildGoogleAdsResult(c *model.Connection) *conn.GoogleAdsConnection {
	r := &conn.GoogleAdsConnection{
		ID:             c.ID,
		ProjectID:      c.ProjectID,
		Label:          optStr(c.Label),
		AccountID:      c.AccountID,
		HasCredentials: c.HasCredentials(),
		Status:         string(c.Status),
		Version:        c.Version,
		Etag:           etag(c.Version),
	}
	r.LoginCustomerID = optStr(c.ProviderConfig["login_customer_id"])
	return r
}

func (s *ConnectionService) CreateGoogleAds(ctx context.Context, p *conn.CreateGoogleAdsPayload) (*conn.GoogleAdsConnection, error) {
	if err := validateConnectionProjectSlug(p.ProjectID); err != nil {
		return nil, err
	}
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderGoogleAds,
		Label:     strVal(cfg.Label),
		// Optional for Google Ads, Meta (LFXV2-3061) and X (LFXV2-3319) — the providers that
		// hold BOTH halves: an account-discovery endpoint to finish the bootstrap from, and a
		// create path that names the missing choice (design/connection.go explains why the
		// other three still require it; see CreateMetaAds below for the sibling).
		// Omitting it stores "" and creates a credentials-only connection, which
		// is the first step of the discovery bootstrap. The column is NOT NULL TEXT, so ""
		// is a legal value and no migration is involved — "unfinished" is spelled as an
		// empty string, not NULL.
		AccountID: strVal(cfg.AccountID),
		ProviderConfig: map[string]string{
			"login_customer_id": strVal(cfg.LoginCustomerID),
		},
		CreatedBy: actorFromCtx(ctx),
	}
	created, err := s.createConn(ctx, m, p.Credentials)
	if err != nil {
		return nil, err
	}
	return s.buildGoogleAdsResult(created), nil
}

func (s *ConnectionService) GetGoogleAds(ctx context.Context, p *conn.GetGoogleAdsPayload) (*conn.GoogleAdsConnection, error) {
	c, err := s.getConn(ctx, p.ProjectID, model.ProviderGoogleAds)
	if err != nil {
		return nil, err
	}
	return s.buildGoogleAdsResult(c), nil
}

func (s *ConnectionService) UpdateGoogleAds(ctx context.Context, p *conn.UpdateGoogleAdsPayload) (*conn.GoogleAdsConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderGoogleAds,
		Label:     strVal(cfg.Label),
		// PUT is a full replace, so omitting account_id CLEARS a previously chosen one — the
		// same semantics label and login_customer_id have always had on this handler. That is
		// the intended way to un-select an account, and it is why this handler is also the
		// second half of the bootstrap: the caller PUTs back the id chosen from discovery.
		AccountID: strVal(cfg.AccountID),
		ProviderConfig: map[string]string{
			"login_customer_id": strVal(cfg.LoginCustomerID),
		},
		UpdatedBy: actorFromCtx(ctx),
	}
	updated, err := s.updateConn(ctx, m, p.IfMatch)
	if err != nil {
		return nil, err
	}
	return s.buildGoogleAdsResult(updated), nil
}

func (s *ConnectionService) DeleteGoogleAds(ctx context.Context, p *conn.DeleteGoogleAdsPayload) error {
	return s.deleteConn(ctx, p.ProjectID, model.ProviderGoogleAds)
}

func (s *ConnectionService) TestGoogleAds(ctx context.Context, p *conn.TestGoogleAdsPayload) (*conn.ConnectionTestResult, error) {
	return s.testConn(ctx, p.ProjectID, model.ProviderGoogleAds)
}

func (s *ConnectionService) SetCredentialGoogleAds(ctx context.Context, p *conn.SetCredentialGoogleAdsPayload) error {
	return s.setCredential(ctx, p.ProjectID, model.ProviderGoogleAds, p.Credentials, actorFromCtx(ctx))
}

// unusableConnectionReason maps an ErrConnectionNotUsable chain onto the fixed vocabulary
// logged by the callers that surface such a chain. There are three, and they do NOT all
// answer a status code: account discovery, the synchronous campaign handlers (metrics and
// status toggle), and — since LFXV2-3061 — the ASYNCHRONOUS pre-create dispatch path
// (Orchestrator.dispatchPlatform). Discovery is not the only consumer, and one case below —
// account_not_selected — is unreachable from discovery, which skips the account-ID check by
// design.
//
// The async caller is the one that makes this function load-bearing rather than convenient.
// dispatchPlatform runs after the 202, so it has no response to classify: it collapses every
// dispatcher error into one generic string in the job result, and the reason token produced
// here is the ONLY place the specific defect is recorded. For the synchronous two a wrong or
// missing reason costs log quality; for that path it costs the diagnosis outright.
//
// It exists because the errors themselves cannot be logged: one
// of these conditions is detected by decoding the decrypted credential blob, and an
// encoding/json error quotes its input. The dispatch layer therefore wraps a reason sentinel
// alongside the status sentinel (internal/domain/errors.go), and this reads it.
//
// The returned strings are a stable, greppable vocabulary — treat them as an interface, not
// as prose, and add a case here rather than a new free-text log line when a new reason
// appears. "unclassified" is the honest answer for a chain carrying no reason sentinel: an
// invented guess would be worse than the absence, because it would read as a diagnosis.
func unusableConnectionReason(err error) string {
	switch {
	case errors.Is(err, domain.ErrConnectionInactive):
		return "connection_inactive"
	case errors.Is(err, domain.ErrCredentialsUndecodable):
		return "credentials_undecodable"
	case errors.Is(err, domain.ErrCredentialsIncomplete):
		return "credentials_incomplete"
	case errors.Is(err, domain.ErrProviderConfigInvalid):
		return "provider_config_invalid"
	case errors.Is(err, domain.ErrCredentialsMalformed):
		return "credential_blob_malformed"
	case errors.Is(err, domain.ErrCredentialsAbsent):
		return "credentials_absent"
	case errors.Is(err, domain.ErrTokenRequestRejected):
		// Before BOTH credential arms. This one means neither credential was evaluated, so
		// reporting either of theirs would name a remedy nobody outside this codebase can
		// apply. It is the only reason token in this vocabulary that points at us.
		return "token_request_rejected"
	case errors.Is(err, domain.ErrApplicationCredentialsInvalid):
		// Before the expired arm: an error carrying both must report the OPERATOR-actionable
		// reason, since "re-authorize the member" cannot repair an application credential.
		return "application_credentials_invalid"
	case errors.Is(err, domain.ErrCredentialsExpired):
		return "credentials_expired"
	case errors.Is(err, domain.ErrAccountNotSelected):
		return "account_not_selected"
	default:
		return "unclassified"
	}
}

// accountDiscovery is everything about the account-discovery endpoints that differs
// between providers, which is the provider constant and the caller-facing wording.
//
// The wording is in here rather than shared because it is the one part that MUST differ:
// the not-usable 400 names the fields an operator has to go and check, and Google's set
// (login_customer_id) and Meta's set (access_token) have nothing in common. Handing a Meta
// operator Google's remedy sends them looking for a field their connection does not have,
// and every status-code assertion in the tests passes while it does — which is why
// TestListMetaAdsAccounts_MessagesNameMetaNotGoogleAds asserts the text.
//
// Nothing about the STATUS MAPPING is in here, deliberately: that is the part which must
// not vary (see listAccounts).
type accountDiscovery struct {
	provider    model.Provider
	displayName string
	// notUsableRemedy completes "the stored <displayName> connection cannot be used as
	// configured: ..." and is the only actionable thing the caller is told, since the
	// underlying cause is credential-derived and never leaves the process.
	//
	// It names PUBLISHED API field names (design/connection.go's set-credential payloads),
	// not the Go field names of the persisted blob. The two differ for Meta —
	// `access_token` on the wire, `AccessToken` in storage — and the caller can only act
	// on the name it sends. Dispatch-layer messages name the Go field, correctly: their
	// audience is this codebase, not the operator.
	notUsableRemedy string
	// operation names what is being attempted, in the response and log messages this helper
	// emits ("<operation> could not be completed", "<operation> failed upstream"). Empty
	// means "account discovery", which is what every ad platform passes.
	//
	// It exists because the email channel reuses this helper's status mapping without being
	// account discovery: HubSpot has no ad account to enumerate, and telling a caller that
	// "account discovery could not be completed" when they searched for an email template
	// describes an operation they did not perform. The mapping itself is shared verbatim —
	// per this helper's contract, a provider gets all of the arms reasoned about here or
	// none of them — and only the noun changes.
	operation string
}

// label returns the operation name for messages, defaulting to account discovery.
func (d accountDiscovery) label() string {
	if d.operation == "" {
		return "account discovery"
	}
	return d.operation
}

var googleAdsAccountDiscovery = accountDiscovery{
	provider:    model.ProviderGoogleAds,
	displayName: "google ads",
	notUsableRemedy: "check that it is active, that the stored credential is valid json with " +
		"every field set, and that login_customer_id is digits only",
}

var metaAdsAccountDiscovery = accountDiscovery{
	provider:    model.ProviderMetaAds,
	displayName: "meta ads",
	notUsableRemedy: "check that it is active and that the stored credential is valid json " +
		"with access_token set",
}

var linkedInAdsAccountDiscovery = accountDiscovery{
	provider:    model.ProviderLinkedInAds,
	displayName: "linkedin ads",
	notUsableRemedy: "check that it is active and that the stored credential is valid json " +
		"with access_token set",
}

var microsoftAdsAccountDiscovery = accountDiscovery{
	provider:    model.ProviderMicrosoftAds,
	displayName: "microsoft ads",
	notUsableRemedy: "check that it is active and that the stored credential is valid json " +
		"with client_id, client_secret, developer_token and refresh_token set",
}

// twitterAdsAccountDiscovery names X's OAuth 1.0a FOUR-tuple, not a single token. Every
// other ad platform here authenticates with one credential field, so a copied remedy would
// send an X operator looking for an access_token that is only a quarter of what their
// connection stores — and the status-code assertions would all still pass while it did.
// These are the PUBLISHED wire names from design/connection.go's TwitterAdsCredentials,
// which is what a caller can act on, not the PascalCase Go keys the blob is persisted under.
var twitterAdsAccountDiscovery = accountDiscovery{
	provider:    model.ProviderTwitterAds,
	displayName: "x/twitter ads",
	notUsableRemedy: "check that it is active and that the stored credential is valid json " +
		"with consumer_key, consumer_secret, access_token and access_token_secret set",
}

// hubspotEmailDiscovery reuses the account-discovery status mapping for the email-template
// search. Not account discovery — HubSpot has no ad account to choose, since the connection is
// scoped to the portal its token authenticates against — but every arm of that mapping applies
// unchanged: the connection can be missing (404), unusable as configured (400), undecryptable
// (500, logged not echoed), or the platform can be down (503). Only the noun differs, which is
// what `operation` carries.
var hubspotEmailDiscovery = accountDiscovery{
	provider:    model.ProviderHubSpot,
	displayName: "hubspot",
	operation:   "email search",
	// private_app_token — the PUBLISHED wire name (`design/connection.go`'s
	// HubSpotCredentials), not the Go/JSON shape `privateAppToken` the blob is persisted
	// under. This string is the only actionable thing the caller is told, and it must name
	// a field they can actually send; the persisted spelling would send them looking for a
	// key that appears in no request they can make.
	notUsableRemedy: "check that it is active and that the stored credential is valid json " +
		"with private_app_token set",
}

// classifyDiscoveryError maps an orchestrator discovery error onto the connections API's
// status codes. Lifted out of listAccounts UNCHANGED so the email search shares the exact
// arms rather than growing a second copy of them — see listAccounts' contract below: a
// provider gets the judgements reasoned about here, or none of them.
//
// Returns nil when aerr is nil.
func (s *ConnectionService) classifyDiscoveryError(ctx context.Context, projectID string, d accountDiscovery, aerr error) error {
	if aerr == nil {
		return nil
	}
	switch {
	case errors.Is(aerr, ErrAccountsUnsupported):
		return &conn.BadRequestError{Code: "400", Message: d.label() + " is not supported for this platform"}
	case errors.Is(aerr, domain.ErrSystemConnectionMissing):
		// ABOVE the ErrNotFound arm, and load-bearing: the forced-system resolver wraps this
		// sentinel ALONGSIDE ErrNotFound, so the broad arm below would win and answer 404
		// "connect your project" for a fault the project cannot fix — forced mode ignores the
		// project's own connection by definition, so connecting one would change nothing.
		// The LF system row is simply not installed for this provider, which only an operator
		// can repair, so this is their page: a 500 and an ERROR log, matching the treatment
		// ErrSystemConnectionNotUsable already gets below.
		slog.ErrorContext(ctx, "the LF system connection is not installed; "+d.label()+" is failing for every project while force-system mode is on",
			"project_id", projectID, "provider", string(d.provider))
		return &conn.InternalServerError{Code: "500", Message: d.label() + " is unavailable"}
	case errors.Is(aerr, domain.ErrNotFound):
		// The project has no stored connection for this provider. That is a client-side
		// state error, not a platform outage — reporting 503 would tell the caller
		// to retry something that can never succeed until a connection exists.
		return &conn.NotFoundError{Code: "404", Message: "no " + d.displayName + " connection configured for this project"}
	case errors.Is(aerr, domain.ErrCredentialDecryptionFailed):
		// A blob long enough to BE our own output that nonetheless failed authenticated
		// decryption. Two causes reach here and they have opposite blast radii:
		//
		//   - a wrong or rotated APPLICATION KEY, which is deployment-wide, so every
		//     project's connection is failing at this instant; or
		//   - THIS ONE ROW's ciphertext being corrupted or tampered with, in which
		//     case every other connection is fine.
		//
		// GCM cannot tell them apart — an authentication failure is an authentication
		// failure — so this arm must not assert either one. An earlier revision claimed
		// the deployment-wide reading as a certainty, which misdirects incident response
		// down a key-rotation path when the real fault is a single damaged row.
		// (Provably truncated blobs no longer arrive here at all: they are rejected as
		// malformed by the length guard in internal/infrastructure/crypto, which is a
		// 400 about one row.)
		//
		// Either way it is not the caller's problem and there is nothing for them to
		// edit: 400 would blame their row for what may be an outage; 503 would promise
		// that waiting helps. Neither is true.
		//
		// Logged at ERROR because this is the arm that should page someone — the first
		// question for whoever answers is whether OTHER projects are failing too, which
		// is what separates the two causes. The cause is safe to log: it is produced by
		// the encryptor from ciphertext and key material only, never from plaintext.
		//
		// The row logged is the one that FAILED, which is not the caller's project when
		// the credentials came from the system fallback — that project has no row at
		// all, and naming it would send whoever answers to inspect something that does
		// not exist while repeated failures of one corrupt system row looked like
		// failures spread across many projects, i.e. the deployment-wide reading this
		// arm must not assert. Origin is carried by ErrSystemConnectionOrigin, separate
		// from the usability sentinels, because this error is not a usability defect.
		credentialProject := projectID
		if errors.Is(aerr, domain.ErrSystemConnectionOrigin) {
			credentialProject = model.SystemProjectID
		}
		// NO ERROR TEXT. The sentinel itself is built from ciphertext and key material only,
		// but what reaches the log is the whole CHAIN, and domain.Encryptor is an interface
		// whose implementations are free to quote the ciphertext or the key they failed on.
		// internal/dispatch/creds.go says exactly this, and names this arm as the one path
		// that still logged the cause — closed here, because the campaign CREATE now routes
		// its setup failures through this classifier. The operator-facing message already
		// carries the remedy, and the cause adds nothing an operator can act on.
		slog.ErrorContext(ctx, "stored credentials failed authenticated decryption; check the application encryption key, and whether this is one row or every connection",
			"project_id", credentialProject, "requested_by_project_id", projectID,
			"provider", string(d.provider))
		return &conn.InternalServerError{Code: "500", Message: d.label() + " could not be completed"}
	case errors.Is(aerr, domain.ErrServiceDefect):
		// ABOVE both connection arms below. The 400 below completes "the stored <provider>
		// connection cannot be used as configured" with a remedy naming the fields to go and
		// check — and here every one of those fields is correct. LinkedIn refused the SHAPE of
		// a request this service built, evaluating no stored credential, so an operator sent
		// to audit their client_id audits something that was never at fault. That is the exact
		// outcome the reason sentinel behind this one was split out to prevent.
		//
		// A 500 and an ERROR log, like the decryption arm above: the only actor who can repair
		// this reads the log. The response body names no remedy because the caller has none.
		//
		// The reason token is logged, never the error — one condition reachable behind this
		// classification decodes a DECRYPTED credential blob (see unusableConnectionReason).
		slog.ErrorContext(ctx, "a defect in this service is blocking "+d.label()+"; the stored connection is NOT at fault and needs no repair",
			"project_id", projectID, "provider", string(d.provider),
			"reason", unusableConnectionReason(aerr))
		return &conn.InternalServerError{Code: "500", Message: d.label() + " could not be completed"}
	case errors.Is(aerr, domain.ErrSystemConnectionNotUsable):
		// The project has no connection of its own and the LF system row it fell back
		// to is unusable. The 400 below would tell this caller to edit "the stored
		// connection" — they have none, and the system scope is unaddressable. Nobody
		// but an operator can act, so page one and say nothing specific to the caller.
		// The reason is safe to log; the error itself is not (see the arm below).
		slog.ErrorContext(ctx, "the LF system connection is not usable; "+d.label()+" is failing for every project without its own connection",
			"provider", string(d.provider), "reason", unusableConnectionReason(aerr))
		return &conn.InternalServerError{Code: "500", Message: d.label() + " could not be completed"}
	case errors.Is(aerr, domain.ErrConnectionNotUsable):
		// The connection EXISTS but cannot be used as it stands — inactive, an
		// incomplete credential blob, or a malformed stored config value such as a
		// dashed login_customer_id. The provider is never contacted, and none of these
		// improve with time, so the 503 below would be a false promise: it tells the
		// caller to retry a request that cannot succeed until a human edits the
		// connection. The dispatcher wraps every pre-send failure with this sentinel
		// precisely so this arm can exist (internal/dispatch/googleads.go,
		// resolveGoogleAdsDiscoveryClient) — the classification cannot be recovered
		// here, because a setup failure and an upstream one arrive as the same type.
		//
		// NEITHER the cause nor its text leaves this function — not in the response,
		// and not in the log. One of the conditions behind this arm is detected by
		// decoding the DECRYPTED credential blob, and an unmarshal error quotes its
		// input, so echoing aerr would put credential-derived bytes into an HTTP body
		// and into centralized logs for exactly the connection whose credentials are
		// malformed. A response body and a log line are different exposures, but the
		// material is the same and so is the answer.
		//
		// What is logged instead is a classification from a FIXED vocabulary
		// (unusableConnectionReason) plus project metadata. That is not a downgrade:
		// the dispatch layer wraps a reason sentinel precisely so this line stays
		// actionable, and a fixed token is what an alert or a grep wants anyway. The
		// response body names the remedy surface, which is the same for all of them.
		slog.WarnContext(ctx, "connection is not usable for "+d.label(),
			"project_id", projectID, "provider", string(d.provider),
			"reason", unusableConnectionReason(aerr))
		return &conn.BadRequestError{
			Code: "400",
			Message: "the stored " + d.displayName + " connection cannot be used as configured: " +
				d.notUsableRemedy,
		}
	default:
		slog.WarnContext(ctx, d.label()+" failed upstream",
			"project_id", projectID, "provider", string(d.provider), "error", aerr)
		return &conn.ConnServiceUnavailableError{Code: "503", Message: d.label() + " could not be completed"}
	}
}

// listAccounts is the whole of account discovery except the strings that name the provider.
//
// Sharing it is not only de-duplication: the status mapping below encodes several judgements
// that are easy to get subtly wrong per provider — a 404 rather than 503 for a connection
// that does not exist, a 500 that logs but does not echo a decryption failure, a 400 rather
// than 503 for a connection no amount of waiting will fix — and a second copy is where one
// of them quietly diverges. Every provider that gains discovery gets the arms that were
// reasoned about here, or none of them.
func (s *ConnectionService) listAccounts(ctx context.Context, projectID string, d accountDiscovery) ([]*conn.AccessibleAccount, error) {
	// One of the project_id-taking endpoints that does NOT reach the repo through a helper
	// in connection_handler.go — the property that made the first of them miss this guard,
	// and the reason it lives in the shared helper rather than per provider. Left open, GET on the reserved scope decrypts the LF credential and
	// enumerates the Linux Foundation's own ad accounts. A project with NO connection still
	// sees those accounts under its OWN id, deliberately (dispatch/creds.go); addressing
	// the reserved scope directly is the different thing, and it is rejected.
	if err := rejectSystemScope(projectID); err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch(d.label())
	if err != nil {
		return nil, err
	}
	accounts, aerr := orch.ReadAccounts(ctx, projectID, d.provider)
	if aerr != nil {
		return nil, s.classifyDiscoveryError(ctx, projectID, d, aerr)
	}
	// Convert model.AccessibleAccount to generated conn type. Preallocated with make so an
	// empty result serializes as `[]`, not `null` — a nil slice here would undo the
	// deliberate zero-length allocation each dispatcher makes one layer down (over Google's
	// `customers`, over Meta's ad accounts) and hand every client a null it has to
	// special-case.
	connAccounts := make([]*conn.AccessibleAccount, 0, len(accounts))
	for _, acct := range accounts {
		label := acct.Label // Convert to pointer
		connAccounts = append(connAccounts, &conn.AccessibleAccount{
			ID:    acct.ID,
			Label: &label,
		})
	}
	return connAccounts, nil
}

func (s *ConnectionService) ListGoogleAdsAccounts(ctx context.Context, p *conn.ListGoogleAdsAccountsPayload) (*conn.ListGoogleAdsAccountsResult, error) {
	accounts, err := s.listAccounts(ctx, p.ProjectID, googleAdsAccountDiscovery)
	if err != nil {
		return nil, err
	}
	return &conn.ListGoogleAdsAccountsResult{Accounts: accounts}, nil
}

// ListMetaAdsAccounts enumerates the ad accounts the stored Meta credential reaches.
//
// Meta's account ids arrive act_-prefixed and are returned that way: that is the form the
// connection stores and the form every account-scoped Meta path is built from, so the answer
// is directly assignable rather than needing the caller to reconstruct it.
func (s *ConnectionService) ListMetaAdsAccounts(ctx context.Context, p *conn.ListMetaAdsAccountsPayload) (*conn.ListMetaAdsAccountsResult, error) {
	accounts, err := s.listAccounts(ctx, p.ProjectID, metaAdsAccountDiscovery)
	if err != nil {
		return nil, err
	}
	return &conn.ListMetaAdsAccountsResult{Accounts: accounts}, nil
}

// ListLinkedinAdsAccounts enumerates the ad accounts the stored LinkedIn credential reaches.
//
// LinkedIn's ids arrive as bare digits and are returned that way: that is the form the
// connection's account_id takes (both are constrained to ^[0-9]+$), so the answer is directly
// assignable rather than needing the caller to reconstruct a URN.
func (s *ConnectionService) ListLinkedinAdsAccounts(ctx context.Context, p *conn.ListLinkedinAdsAccountsPayload) (*conn.ListLinkedinAdsAccountsResult, error) {
	accounts, err := s.listAccounts(ctx, p.ProjectID, linkedInAdsAccountDiscovery)
	if err != nil {
		return nil, err
	}
	return &conn.ListLinkedinAdsAccountsResult{Accounts: accounts}, nil
}

// ListMicrosoftAdsAccounts enumerates the Microsoft Advertising accounts the stored credential
// reaches, across every customer it can see.
//
// The id returned is the account id as digits — the form account_id takes. Microsoft's
// human-facing account NUMBER (e.g. "X1234567") rides in the label instead: it is what the
// Microsoft Advertising UI shows, so a user recognises the account by it, but it is not the
// value to store.
func (s *ConnectionService) ListMicrosoftAdsAccounts(ctx context.Context, p *conn.ListMicrosoftAdsAccountsPayload) (*conn.ListMicrosoftAdsAccountsResult, error) {
	accounts, err := s.listAccounts(ctx, p.ProjectID, microsoftAdsAccountDiscovery)
	if err != nil {
		return nil, err
	}
	return &conn.ListMicrosoftAdsAccountsResult{Accounts: accounts}, nil
}

// ListTwitterAdsAccounts enumerates the X Ads accounts the stored credential reaches.
//
// X's ids arrive as ALPHANUMERIC handles (e.g. "18ce54d4x5t"), not digits, and are returned
// that way: that is the form the connection's account_id takes (both are constrained to
// ^[A-Za-z0-9]+$), so the answer is directly assignable rather than needing the caller to
// reformat it. This is the one shape difference from LinkedIn and Microsoft, whose ids are
// digits — an assumption that X's are numeric would reject every real answer.
func (s *ConnectionService) ListTwitterAdsAccounts(ctx context.Context, p *conn.ListTwitterAdsAccountsPayload) (*conn.ListTwitterAdsAccountsResult, error) {
	accounts, err := s.listAccounts(ctx, p.ProjectID, twitterAdsAccountDiscovery)
	if err != nil {
		return nil, err
	}
	return &conn.ListTwitterAdsAccountsResult{Accounts: accounts}, nil
}

// ─── HubSpot (email channel) ───

// ListHubspotEmails searches the marketing emails reachable through the project's stored
// HubSpot connection, so a caller can choose which one an email campaign will CLONE.
//
// Not account discovery, and deliberately not modelled as it: a HubSpot connection is already
// scoped to the portal its private-app token authenticates against, so there is no account to
// pick. What has no default is hubspotConfig.SourceEmailID, and this is how a caller finds one.
// The status mapping IS shared with discovery, through classifyDiscoveryError — every arm of it
// applies unchanged, and only the noun in the messages differs.
func (s *ConnectionService) ListHubspotEmails(ctx context.Context, p *conn.ListHubspotEmailsPayload) (*conn.ListHubspotEmailsResult, error) {
	d := hubspotEmailDiscovery
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	_, _, orch, err := s.resolveBackendWithOrch(d.label())
	if err != nil {
		return nil, err
	}

	// An omitted q asks for a first screen rather than a search; HubSpot treats an empty
	// query as "no name/subject filter". That screen is BOUNDED
	// (hubspot.maxUnfilteredEmails) because an empty needle matches every row, so it is the
	// walk's worst case. The bound takes the first N in SERVER order and sorts those —
	// "recent emails to pick from", not a guarantee of the newest in the portal, which
	// under a bound is not available. A filtered search is not bounded, because truncating
	// one would report an email that exists as absent.
	query := ""
	if p.Q != nil {
		query = *p.Q
	}

	emails, serr := orch.SearchEmails(ctx, p.ProjectID, d.provider, query)
	if serr != nil {
		// ErrEmailSearchUnsupported is the one arm classifyDiscoveryError cannot carry: it
		// keys on ErrAccountsUnsupported, and the two are separate sentinels precisely
		// because the capabilities are independent — HubSpot searches emails and has no ad
		// accounts, while the AccountLister-capable platforms are the reverse — they enumerate
		// accounts and search no emails. Not "the ad platforms": Reddit is an ad platform
		// and implements neither capability, which is the membership distinction below. Stated
		// as the SHAPE rather than by naming which providers implement AccountLister: that
		// membership only grows, and an enumerating comment is falsified by the next provider
		// added without anything failing — which is why no roster is written here. For the
		// current membership see accountListerProviders in
		// internal/dispatch/accountlister_prose_parity_test.go, which DERIVES the roster by
		// type-asserting every candidate dispatcher against service.AccountLister, so it moves
		// with the code. The `var _ service.AccountLister` block in account_discovery_test.go
		// is NOT that list: it pins only the three providers that test exercises, and today
		// omits the Google Ads and Meta implementations.
		if errors.Is(serr, ErrEmailSearchUnsupported) {
			return nil, &conn.BadRequestError{Code: "400", Message: d.label() + " is not supported for this platform"}
		}
		return nil, s.classifyDiscoveryError(ctx, p.ProjectID, d, serr)
	}

	// make, not nil: an empty result must serialize as `[]` rather than `null`, matching the
	// account endpoints and preserving the zero-length allocation the dispatcher made below.
	out := make([]*conn.MarketingEmail, 0, len(emails))
	for _, e := range emails {
		name, subject, state, updatedAt := e.Name, e.Subject, e.State, e.UpdatedAt
		out = append(out, &conn.MarketingEmail{
			ID:        e.ID,
			Name:      &name,
			Subject:   &subject,
			State:     &state,
			UpdatedAt: &updatedAt,
		})
	}
	return &conn.ListHubspotEmailsResult{Emails: out}, nil
}

// ─── LinkedinAds ───

func (s *ConnectionService) buildLinkedinAdsResult(c *model.Connection) *conn.LinkedinAdsConnection {
	r := &conn.LinkedinAdsConnection{
		ID:             c.ID,
		ProjectID:      c.ProjectID,
		Label:          optStr(c.Label),
		AccountID:      c.AccountID,
		HasCredentials: c.HasCredentials(),
		Status:         string(c.Status),
		Version:        c.Version,
		Etag:           etag(c.Version),
	}
	r.OrgID = optStr(c.ProviderConfig["org_id"])
	return r
}

// validateLinkedInRefreshCredentials enforces all-or-none on the optional refresh
// trio. LinkedIn's exchange requires refresh_token, client_id AND client_secret
// together, so a payload carrying some-but-not-all is unusable for refresh — and
// because Credentials.CanRefresh() gates on all three, storing it would SILENTLY
// degrade the connection to bearer-only. The operator would see a saved refresh token
// and reasonably believe renewal was configured, then meet the same 60-day expiry this
// feature exists to prevent. Reject it at the door instead, where the caller can act.
//
// All three absent is the normal, supported case: LinkedIn issues programmatic refresh
// tokens only to approved Marketing Developer Platform partners.
func validateLinkedInRefreshCredentials(c *conn.LinkedinAdsCredentials) error {
	if c == nil {
		return nil
	}
	// Two distinct states, deliberately not collapsed. A nil pointer is the key ABSENT from
	// the payload; a non-nil pointer whose value trims to "" is the key SUPPLIED holding no
	// credential. Counting only the second as "not present" made them indistinguishable, and
	// the all-or-none guard below reads `present == 0` as "all three omitted" — so
	// `{"refresh_token": ""}` scored 0, passed, and was stored. CanRefresh() then read it as
	// absent and the connection was silently bearer-only, while the operator had watched the
	// field be accepted and reasonably believed renewal was configured. That is verbatim the
	// failure the paragraph above says this function prevents.
	//
	// The sibling boundary already refuses it: internal/bootstrap/sysacct.go treats a
	// supplied-but-blank string as "a supplied key holding no credential" and faults. Two
	// boundaries write the same trio, and one canonicalizing to absence while the other
	// refuses is a difference no operator can see until renewal silently never happens.
	present, blank := 0, []string(nil)
	for _, f := range []struct {
		name string
		val  *string
	}{
		{"refresh_token", c.RefreshToken},
		{"client_id", c.ClientID},
		{"client_secret", c.ClientSecret},
	} {
		switch {
		case f.val == nil:
			// Absent. The supported bearer-only case is all three like this.
		case strings.TrimSpace(*f.val) == "":
			blank = append(blank, f.name)
		default:
			present++
		}
	}
	// Checked BEFORE the all-or-none verdict, like the padding rule below, and for the same
	// reason: a blank field must not be able to reach a verdict that reads it as omitted.
	if len(blank) > 0 {
		return &conn.BadRequestError{
			Code: "400",
			Message: "linkedin refresh credentials must not be supplied empty: " +
				strings.Join(blank, ", ") +
				" carries no credential; omit the field entirely for a bearer-only connection, " +
				"or supply a real value — an empty one would be stored and silently disable renewal",
		}
	}
	// Surrounding whitespace is REFUSED, not trimmed away, and it is checked BEFORE the
	// all-or-none verdict so a padded-but-complete trio cannot pass. Every validator in
	// this package gates on the TRIMMED value while the store keeps the value VERBATIM,
	// so " id " satisfies CanRefresh() and is then sent raw to LinkedIn's token endpoint
	// (internal/platform/linkedin/token.go form.Set), which rejects it as invalid_client
	// forever. That is an unrecoverable state a validator claimed to prevent: the row
	// looks correctly configured and no reconnect changes it.
	//
	// Refused rather than canonicalized because a credential is opaque to this service:
	// silently rewriting one would hide a truncated paste, and no provider issues a
	// secret whose surrounding whitespace is significant. This mirrors the bootstrap
	// installer's identical rule (canonicalCredentials, internal/bootstrap/sysacct.go).
	var padded []string
	for _, f := range []struct {
		name string
		val  *string
	}{
		{"refresh_token", c.RefreshToken},
		{"client_id", c.ClientID},
		{"client_secret", c.ClientSecret},
	} {
		if f.val != nil && *f.val != strings.TrimSpace(*f.val) {
			padded = append(padded, f.name)
		}
	}
	if len(padded) > 0 {
		return &conn.BadRequestError{
			Code: "400",
			Message: "linkedin refresh credentials must not have leading or trailing whitespace in " +
				strings.Join(padded, ", ") +
				"; a secret is stored verbatim, so the padding would be sent to LinkedIn and rejected",
		}
	}
	if present == 0 || present == 3 {
		return nil
	}
	return &conn.BadRequestError{
		Code: "400",
		Message: "linkedin refresh credentials are all-or-none: refresh_token, client_id and " +
			"client_secret must be supplied together, or all omitted for a bearer-only connection",
	}
}

func (s *ConnectionService) CreateLinkedinAds(ctx context.Context, p *conn.CreateLinkedinAdsPayload) (*conn.LinkedinAdsConnection, error) {
	if err := validateConnectionProjectSlug(p.ProjectID); err != nil {
		return nil, err
	}
	if err := validateLinkedInRefreshCredentials(p.Credentials); err != nil {
		return nil, err
	}
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderLinkedInAds,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"org_id": cfg.OrgID,
		},
		CreatedBy: actorFromCtx(ctx),
	}
	created, err := s.createConn(ctx, m, p.Credentials)
	if err != nil {
		return nil, err
	}
	return s.buildLinkedinAdsResult(created), nil
}

func (s *ConnectionService) GetLinkedinAds(ctx context.Context, p *conn.GetLinkedinAdsPayload) (*conn.LinkedinAdsConnection, error) {
	c, err := s.getConn(ctx, p.ProjectID, model.ProviderLinkedInAds)
	if err != nil {
		return nil, err
	}
	return s.buildLinkedinAdsResult(c), nil
}

func (s *ConnectionService) UpdateLinkedinAds(ctx context.Context, p *conn.UpdateLinkedinAdsPayload) (*conn.LinkedinAdsConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderLinkedInAds,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"org_id": cfg.OrgID,
		},
		UpdatedBy: actorFromCtx(ctx),
	}
	updated, err := s.updateConn(ctx, m, p.IfMatch)
	if err != nil {
		return nil, err
	}
	return s.buildLinkedinAdsResult(updated), nil
}

func (s *ConnectionService) DeleteLinkedinAds(ctx context.Context, p *conn.DeleteLinkedinAdsPayload) error {
	return s.deleteConn(ctx, p.ProjectID, model.ProviderLinkedInAds)
}

func (s *ConnectionService) TestLinkedinAds(ctx context.Context, p *conn.TestLinkedinAdsPayload) (*conn.ConnectionTestResult, error) {
	return s.testConn(ctx, p.ProjectID, model.ProviderLinkedInAds)
}

func (s *ConnectionService) SetCredentialLinkedinAds(ctx context.Context, p *conn.SetCredentialLinkedinAdsPayload) error {
	if err := validateLinkedInRefreshCredentials(p.Credentials); err != nil {
		return err
	}
	return s.setCredential(ctx, p.ProjectID, model.ProviderLinkedInAds, p.Credentials, actorFromCtx(ctx))
}

// ─── MetaAds ───

func (s *ConnectionService) buildMetaAdsResult(c *model.Connection) *conn.MetaAdsConnection {
	r := &conn.MetaAdsConnection{
		ID:             c.ID,
		ProjectID:      c.ProjectID,
		Label:          optStr(c.Label),
		AccountID:      c.AccountID,
		HasCredentials: c.HasCredentials(),
		Status:         string(c.Status),
		Version:        c.Version,
		Etag:           etag(c.Version),
	}
	r.PageID = optStr(c.ProviderConfig["page_id"])
	r.AppID = optStr(c.ProviderConfig["app_id"])
	return r
}

func (s *ConnectionService) CreateMetaAds(ctx context.Context, p *conn.CreateMetaAdsPayload) (*conn.MetaAdsConnection, error) {
	if err := validateConnectionProjectSlug(p.ProjectID); err != nil {
		return nil, err
	}
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderMetaAds,
		Label:     strVal(cfg.Label),
		// Optional for Meta too (design/connection.go explains why): omitting it stores
		// "" and creates a credentials-only connection, the first step of the discovery
		// bootstrap — same semantics as Google Ads' AccountID above.
		AccountID: strVal(cfg.AccountID),
		ProviderConfig: map[string]string{
			"page_id": cfg.PageID, // required by the design
			"app_id":  strVal(cfg.AppID),
		},
		CreatedBy: actorFromCtx(ctx),
	}
	created, err := s.createConn(ctx, m, p.Credentials)
	if err != nil {
		return nil, err
	}
	return s.buildMetaAdsResult(created), nil
}

func (s *ConnectionService) GetMetaAds(ctx context.Context, p *conn.GetMetaAdsPayload) (*conn.MetaAdsConnection, error) {
	c, err := s.getConn(ctx, p.ProjectID, model.ProviderMetaAds)
	if err != nil {
		return nil, err
	}
	return s.buildMetaAdsResult(c), nil
}

func (s *ConnectionService) UpdateMetaAds(ctx context.Context, p *conn.UpdateMetaAdsPayload) (*conn.MetaAdsConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderMetaAds,
		Label:     strVal(cfg.Label),
		// PUT is a full replace, so omitting account_id CLEARS a previously chosen one —
		// same semantics as Google Ads' AccountID above, and the second half of the
		// bootstrap: the caller PUTs back the id chosen from discovery.
		AccountID: strVal(cfg.AccountID),
		ProviderConfig: map[string]string{
			"page_id": cfg.PageID, // required by the design
			"app_id":  strVal(cfg.AppID),
		},
		UpdatedBy: actorFromCtx(ctx),
	}
	updated, err := s.updateConn(ctx, m, p.IfMatch)
	if err != nil {
		return nil, err
	}
	return s.buildMetaAdsResult(updated), nil
}

func (s *ConnectionService) DeleteMetaAds(ctx context.Context, p *conn.DeleteMetaAdsPayload) error {
	return s.deleteConn(ctx, p.ProjectID, model.ProviderMetaAds)
}

func (s *ConnectionService) TestMetaAds(ctx context.Context, p *conn.TestMetaAdsPayload) (*conn.ConnectionTestResult, error) {
	return s.testConn(ctx, p.ProjectID, model.ProviderMetaAds)
}

func (s *ConnectionService) SetCredentialMetaAds(ctx context.Context, p *conn.SetCredentialMetaAdsPayload) error {
	return s.setCredential(ctx, p.ProjectID, model.ProviderMetaAds, p.Credentials, actorFromCtx(ctx))
}

// ─── RedditAds ───

func (s *ConnectionService) buildRedditAdsResult(c *model.Connection) *conn.RedditAdsConnection {
	r := &conn.RedditAdsConnection{
		ID:             c.ID,
		ProjectID:      c.ProjectID,
		Label:          optStr(c.Label),
		AccountID:      c.AccountID,
		HasCredentials: c.HasCredentials(),
		Status:         string(c.Status),
		Version:        c.Version,
		Etag:           etag(c.Version),
	}
	// Surfaced so a caller can see whether this connection can create campaigns at all: an
	// absent pixel means every Reddit create is refused.
	r.ConversionPixelID = optStr(c.ProviderConfig["conversion_pixel_id"])
	return r
}

func (s *ConnectionService) CreateRedditAds(ctx context.Context, p *conn.CreateRedditAdsPayload) (*conn.RedditAdsConnection, error) {
	if err := validateConnectionProjectSlug(p.ProjectID); err != nil {
		return nil, err
	}
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderRedditAds,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"conversion_pixel_id": strVal(cfg.ConversionPixelID),
		},
		CreatedBy: actorFromCtx(ctx),
	}
	created, err := s.createConn(ctx, m, p.Credentials)
	if err != nil {
		return nil, err
	}
	return s.buildRedditAdsResult(created), nil
}

func (s *ConnectionService) GetRedditAds(ctx context.Context, p *conn.GetRedditAdsPayload) (*conn.RedditAdsConnection, error) {
	c, err := s.getConn(ctx, p.ProjectID, model.ProviderRedditAds)
	if err != nil {
		return nil, err
	}
	return s.buildRedditAdsResult(c), nil
}

func (s *ConnectionService) UpdateRedditAds(ctx context.Context, p *conn.UpdateRedditAdsPayload) (*conn.RedditAdsConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderRedditAds,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"conversion_pixel_id": strVal(cfg.ConversionPixelID),
		},
		UpdatedBy: actorFromCtx(ctx),
	}
	updated, err := s.updateConn(ctx, m, p.IfMatch)
	if err != nil {
		return nil, err
	}
	return s.buildRedditAdsResult(updated), nil
}

func (s *ConnectionService) DeleteRedditAds(ctx context.Context, p *conn.DeleteRedditAdsPayload) error {
	return s.deleteConn(ctx, p.ProjectID, model.ProviderRedditAds)
}

func (s *ConnectionService) TestRedditAds(ctx context.Context, p *conn.TestRedditAdsPayload) (*conn.ConnectionTestResult, error) {
	return s.testConn(ctx, p.ProjectID, model.ProviderRedditAds)
}

func (s *ConnectionService) SetCredentialRedditAds(ctx context.Context, p *conn.SetCredentialRedditAdsPayload) error {
	return s.setCredential(ctx, p.ProjectID, model.ProviderRedditAds, p.Credentials, actorFromCtx(ctx))
}

// ─── TwitterAds ───

func (s *ConnectionService) buildTwitterAdsResult(c *model.Connection) *conn.TwitterAdsConnection {
	r := &conn.TwitterAdsConnection{
		ID:             c.ID,
		ProjectID:      c.ProjectID,
		Label:          optStr(c.Label),
		AccountID:      c.AccountID,
		HasCredentials: c.HasCredentials(),
		Status:         string(c.Status),
		Version:        c.Version,
		Etag:           etag(c.Version),
	}
	r.FundingInstrumentID = optStr(c.ProviderConfig["funding_instrument_id"])
	return r
}

func (s *ConnectionService) CreateTwitterAds(ctx context.Context, p *conn.CreateTwitterAdsPayload) (*conn.TwitterAdsConnection, error) {
	if err := validateConnectionProjectSlug(p.ProjectID); err != nil {
		return nil, err
	}
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderTwitterAds,
		Label:     strVal(cfg.Label),
		AccountID: strVal(cfg.AccountID),
		ProviderConfig: map[string]string{
			"funding_instrument_id": cfg.FundingInstrumentID,
		},
		CreatedBy: actorFromCtx(ctx),
	}
	created, err := s.createConn(ctx, m, p.Credentials)
	if err != nil {
		return nil, err
	}
	return s.buildTwitterAdsResult(created), nil
}

func (s *ConnectionService) GetTwitterAds(ctx context.Context, p *conn.GetTwitterAdsPayload) (*conn.TwitterAdsConnection, error) {
	c, err := s.getConn(ctx, p.ProjectID, model.ProviderTwitterAds)
	if err != nil {
		return nil, err
	}
	return s.buildTwitterAdsResult(c), nil
}

func (s *ConnectionService) UpdateTwitterAds(ctx context.Context, p *conn.UpdateTwitterAdsPayload) (*conn.TwitterAdsConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderTwitterAds,
		Label:     strVal(cfg.Label),
		AccountID: strVal(cfg.AccountID),
		ProviderConfig: map[string]string{
			"funding_instrument_id": cfg.FundingInstrumentID,
		},
		UpdatedBy: actorFromCtx(ctx),
	}
	updated, err := s.updateConn(ctx, m, p.IfMatch)
	if err != nil {
		return nil, err
	}
	return s.buildTwitterAdsResult(updated), nil
}

func (s *ConnectionService) DeleteTwitterAds(ctx context.Context, p *conn.DeleteTwitterAdsPayload) error {
	return s.deleteConn(ctx, p.ProjectID, model.ProviderTwitterAds)
}

func (s *ConnectionService) TestTwitterAds(ctx context.Context, p *conn.TestTwitterAdsPayload) (*conn.ConnectionTestResult, error) {
	return s.testConn(ctx, p.ProjectID, model.ProviderTwitterAds)
}

func (s *ConnectionService) SetCredentialTwitterAds(ctx context.Context, p *conn.SetCredentialTwitterAdsPayload) error {
	return s.setCredential(ctx, p.ProjectID, model.ProviderTwitterAds, p.Credentials, actorFromCtx(ctx))
}

// ─── MicrosoftAds ───

func (s *ConnectionService) buildMicrosoftAdsResult(c *model.Connection) *conn.MicrosoftAdsConnection {
	r := &conn.MicrosoftAdsConnection{
		ID:             c.ID,
		ProjectID:      c.ProjectID,
		Label:          optStr(c.Label),
		AccountID:      c.AccountID,
		HasCredentials: c.HasCredentials(),
		Status:         string(c.Status),
		Version:        c.Version,
		Etag:           etag(c.Version),
	}
	r.CustomerID = optStr(c.ProviderConfig["customer_id"])
	return r
}

func (s *ConnectionService) CreateMicrosoftAds(ctx context.Context, p *conn.CreateMicrosoftAdsPayload) (*conn.MicrosoftAdsConnection, error) {
	if err := validateConnectionProjectSlug(p.ProjectID); err != nil {
		return nil, err
	}
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderMicrosoftAds,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"customer_id": strVal(cfg.CustomerID),
		},
		CreatedBy: actorFromCtx(ctx),
	}
	created, err := s.createConn(ctx, m, p.Credentials)
	if err != nil {
		return nil, err
	}
	return s.buildMicrosoftAdsResult(created), nil
}

func (s *ConnectionService) GetMicrosoftAds(ctx context.Context, p *conn.GetMicrosoftAdsPayload) (*conn.MicrosoftAdsConnection, error) {
	c, err := s.getConn(ctx, p.ProjectID, model.ProviderMicrosoftAds)
	if err != nil {
		return nil, err
	}
	return s.buildMicrosoftAdsResult(c), nil
}

func (s *ConnectionService) UpdateMicrosoftAds(ctx context.Context, p *conn.UpdateMicrosoftAdsPayload) (*conn.MicrosoftAdsConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderMicrosoftAds,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"customer_id": strVal(cfg.CustomerID),
		},
		UpdatedBy: actorFromCtx(ctx),
	}
	updated, err := s.updateConn(ctx, m, p.IfMatch)
	if err != nil {
		return nil, err
	}
	return s.buildMicrosoftAdsResult(updated), nil
}

func (s *ConnectionService) DeleteMicrosoftAds(ctx context.Context, p *conn.DeleteMicrosoftAdsPayload) error {
	return s.deleteConn(ctx, p.ProjectID, model.ProviderMicrosoftAds)
}

func (s *ConnectionService) TestMicrosoftAds(ctx context.Context, p *conn.TestMicrosoftAdsPayload) (*conn.ConnectionTestResult, error) {
	return s.testConn(ctx, p.ProjectID, model.ProviderMicrosoftAds)
}

func (s *ConnectionService) SetCredentialMicrosoftAds(ctx context.Context, p *conn.SetCredentialMicrosoftAdsPayload) error {
	return s.setCredential(ctx, p.ProjectID, model.ProviderMicrosoftAds, p.Credentials, actorFromCtx(ctx))
}

// ─── Hubspot ───

func (s *ConnectionService) buildHubspotResult(c *model.Connection) *conn.HubspotConnection {
	r := &conn.HubspotConnection{
		ID:             c.ID,
		ProjectID:      c.ProjectID,
		Label:          optStr(c.Label),
		AccountID:      c.AccountID,
		HasCredentials: c.HasCredentials(),
		Status:         string(c.Status),
		Version:        c.Version,
		Etag:           etag(c.Version),
	}
	r.PortalID = optStr(c.ProviderConfig["portal_id"])
	r.SenderEmail = optStr(c.ProviderConfig["sender_email"])
	r.SenderName = optStr(c.ProviderConfig["sender_name"])
	r.BrandKit = optStr(c.ProviderConfig["brand_kit"])
	return r
}

func (s *ConnectionService) CreateHubspot(ctx context.Context, p *conn.CreateHubspotPayload) (*conn.HubspotConnection, error) {
	if err := validateConnectionProjectSlug(p.ProjectID); err != nil {
		return nil, err
	}
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderHubSpot,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"portal_id":    strVal(cfg.PortalID),
			"sender_email": strVal(cfg.SenderEmail),
			"sender_name":  strVal(cfg.SenderName),
			"brand_kit":    strVal(cfg.BrandKit),
		},
		CreatedBy: actorFromCtx(ctx),
	}
	created, err := s.createConn(ctx, m, p.Credentials)
	if err != nil {
		return nil, err
	}
	return s.buildHubspotResult(created), nil
}

func (s *ConnectionService) GetHubspot(ctx context.Context, p *conn.GetHubspotPayload) (*conn.HubspotConnection, error) {
	c, err := s.getConn(ctx, p.ProjectID, model.ProviderHubSpot)
	if err != nil {
		return nil, err
	}
	return s.buildHubspotResult(c), nil
}

func (s *ConnectionService) UpdateHubspot(ctx context.Context, p *conn.UpdateHubspotPayload) (*conn.HubspotConnection, error) {
	cfg := p.Config
	m := &model.Connection{
		ProjectID: p.ProjectID,
		Provider:  model.ProviderHubSpot,
		Label:     strVal(cfg.Label),
		AccountID: cfg.AccountID,
		ProviderConfig: map[string]string{
			"portal_id":    strVal(cfg.PortalID),
			"sender_email": strVal(cfg.SenderEmail),
			"sender_name":  strVal(cfg.SenderName),
			"brand_kit":    strVal(cfg.BrandKit),
		},
		UpdatedBy: actorFromCtx(ctx),
	}
	updated, err := s.updateConn(ctx, m, p.IfMatch)
	if err != nil {
		return nil, err
	}
	return s.buildHubspotResult(updated), nil
}

func (s *ConnectionService) DeleteHubspot(ctx context.Context, p *conn.DeleteHubspotPayload) error {
	return s.deleteConn(ctx, p.ProjectID, model.ProviderHubSpot)
}

func (s *ConnectionService) TestHubspot(ctx context.Context, p *conn.TestHubspotPayload) (*conn.ConnectionTestResult, error) {
	return s.testConn(ctx, p.ProjectID, model.ProviderHubSpot)
}

func (s *ConnectionService) SetCredentialHubspot(ctx context.Context, p *conn.SetCredentialHubspotPayload) error {
	return s.setCredential(ctx, p.ProjectID, model.ProviderHubSpot, p.Credentials, actorFromCtx(ctx))
}

// hubspotCampaignDiscovery reuses the account-discovery status mapping for the campaign
// lookup, exactly as hubspotEmailDiscovery does and for the same reasons: every arm applies
// unchanged (connection missing, unusable, undecryptable, platform down) and only the noun
// differs.
var hubspotCampaignDiscovery = accountDiscovery{
	provider:    model.ProviderHubSpot,
	displayName: "hubspot",
	operation:   "campaign search",
	notUsableRemedy: "check that it is active and that the stored credential is valid json " +
		"with private_app_token set",
}

// hubspotCampaignCreateDiscovery is the CREATE path's own descriptor.
//
// Separate from hubspotCampaignDiscovery only because `operation` reaches the operator: sharing
// it made the create endpoint report "campaign search service is unavailable" during cold start
// and "campaign search is not supported" on a capability gap. Both name an operation the caller
// did not ask for, sending them to look at the wrong thing.
var hubspotCampaignCreateDiscovery = accountDiscovery{
	provider:    model.ProviderHubSpot,
	displayName: "hubspot",
	operation:   "campaign creation",
	notUsableRemedy: "check that it is active and that the stored credential is valid json " +
		"with private_app_token set",
}

// SearchHubspotCampaigns finds LF HubSpot campaigns by name, so a caller can read back an
// existing campaign's utm token.
//
// THE ANSWER IS PORTAL-WIDE. `project_id` gates permission, not visibility — HubSpot's campaign
// namespace is the whole portal, so this returns every campaign in the connection's portal. That
// is stated on the design method too, because a reader who assumes project scoping would draw
// the wrong conclusion from an unexpected match.
func (s *ConnectionService) SearchHubspotCampaigns(ctx context.Context, p *conn.SearchHubspotCampaignsPayload) (*conn.SearchHubspotCampaignsResult, error) {
	d := hubspotCampaignDiscovery
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	// Trimmed-empty is a BAD REQUEST, refused before the platform is contacted. Goa's
	// MinLength(1) counts runes, so `q="   "` passes the generated decoder; the client then
	// refuses it with a sentinel-less error that classifyDiscoveryError would report as a
	// retryable 503 — telling the caller to retry a request that can never succeed.
	if strings.TrimSpace(p.Q) == "" {
		return nil, &conn.BadRequestError{Code: "400", Message: "a campaign search requires a non-empty query"}
	}
	_, _, orch, err := s.resolveBackendWithOrch(d.label())
	if err != nil {
		return nil, err
	}

	page, serr := orch.SearchCampaigns(ctx, p.ProjectID, d.provider, p.Q)
	if serr != nil {
		// Mirrors ListHubspotEmails: the unsupported-capability sentinel is the one arm
		// classifyDiscoveryError cannot carry, because that helper keys on
		// ErrAccountsUnsupported and the capabilities are independent.
		if errors.Is(serr, ErrCampaignSearchUnsupported) {
			return nil, &conn.BadRequestError{Code: "400", Message: d.label() + " is not supported for this platform"}
		}
		return nil, s.classifyDiscoveryError(ctx, p.ProjectID, d, serr)
	}

	// make, not nil: an empty result must serialize as `[]` rather than `null`. The caller
	// branches on empty-vs-found to decide whether to offer a create, so this is the one field
	// it must be able to read without a null check.
	out := make([]*conn.HubspotCampaign, 0, len(page.Campaigns))
	for _, c := range page.Campaigns {
		out = append(out, toWireHubspotCampaign(c))
	}
	// Capped travels with the results because it changes what an EMPTY result means. Dropped
	// here, the UI would read "no matches" as "no such campaign" and offer a create.
	return &conn.SearchHubspotCampaignsResult{Campaigns: out, Capped: page.Capped}, nil
}

// CreateHubspotCampaign creates a portal-wide HubSpot campaign and returns the token HubSpot
// assigned it.
//
// IT ALWAYS CREATES, and the created campaign is visible to everyone on that portal. Both facts are on
// the design method; neither is enforced here, because neither can be: a duplicate check would
// race, and visibility is HubSpot's data model. The caller searches first and warns.
func (s *ConnectionService) CreateHubspotCampaign(ctx context.Context, p *conn.CreateHubspotCampaignPayload) (*conn.HubspotCampaign, error) {
	d := hubspotCampaignCreateDiscovery
	if err := rejectSystemScope(p.ProjectID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Name) == "" {
		return nil, &conn.BadRequestError{Code: "400", Message: "a campaign creation requires a non-empty name"}
	}
	_, _, orch, err := s.resolveBackendWithOrch(d.label())
	if err != nil {
		return nil, err
	}

	created, cerr := orch.CreateCampaign(ctx, p.ProjectID, d.provider, p.Name)
	if cerr != nil {
		if errors.Is(cerr, ErrCampaignSearchUnsupported) {
			return nil, &conn.BadRequestError{Code: "400", Message: d.label() + " is not supported for this platform"}
		}
		// SETUP failures are reported as themselves, because they PROVE nothing was sent: a
		// missing or inactive connection, an undecryptable credential, an unsupported platform.
		// Calling those "may already exist" would send an operator to HubSpot to look for a
		// campaign that was never attempted, and hide the remedy they actually need — which is
		// to fix the connection. Only failures that could have reached HubSpot are unconfirmed.
		// ErrCredentialDecryptionFailed belongs here too, and its absence was a real defect:
		// credsSource.decryptConn returns it BEFORE any HubSpot request is built, so the campaign
		// provably does not exist — yet it fell through to the unconfirmed 503 telling the
		// operator to go and check.
		if errors.Is(cerr, domain.ErrNotFound) || errors.Is(cerr, domain.ErrSystemConnectionMissing) ||
			errors.Is(cerr, domain.ErrConnectionNotUsable) || errors.Is(cerr, domain.ErrCredentialDecryptionFailed) {
			return nil, s.classifyDiscoveryError(ctx, p.ProjectID, d, cerr)
		}
		// NOT classifyDiscoveryError for anything else. That classifier is written for READS: its default arm
		// reports a retryable "campaign search could not be completed" 503, which is wrong here
		// twice over. It names the wrong operation, and — far worse — it invites a retry of a
		// NON-IDEMPOTENT write into a namespace shared by everyone on that HubSpot portal. HubSpot marks mutating
		// transport, 429, 3xx and 5xx failures as unconfirmed precisely because the campaign may
		// already exist; collapsing them into "try again" is how a duplicate gets made.
		//
		// A DEFINITE rejection is reported as one. HubSpot answered on the merits — an invalid
		// name, a permission failure — and nothing was created, so telling the operator to go
		// hunt for a campaign that does not exist wastes their time and buries the actual
		// remedy. IsDefiniteRejection is the structural signal, and it is asked POSITIVELY
		// rather than as !IsUnconfirmed: the negation answers true for any error the platform
		// package cannot classify, which would report "nothing was created" about an outcome
		// nobody established. Unrecognised errors fall through to unconfirmed below.
		// A DECRYPT failure is logged without its cause, for the same reason the discovery
		// classifier drops it: the chain can carry ciphertext or key material, and
		// safeErrSummary is NOT a redactor — it normalises and truncates only. Every other
		// failure here is an upstream or transport error whose text is what an operator needs.
		if errors.Is(cerr, domain.ErrCredentialDecryptionFailed) {
			slog.ErrorContext(ctx, "hubspot campaign creation failed: stored credentials could not be decrypted",
				"project_id", p.ProjectID)
		} else {
			slog.ErrorContext(ctx, "hubspot campaign creation failed",
				"project_id", p.ProjectID, "definite_rejection", errors.Is(cerr, domain.ErrPlatformRejected), "error", safeErrSummary(cerr))
		}
		// FIRST, because ErrPlatformNeverSent no longer joins ErrPlatformRejected: the two are
		// mutually exclusive (HubSpot refused vs HubSpot never saw it), and reporting both made
		// `definite_rejection` true for a dial failure.
		//
		// A 503, not a 400: api-catalog rule 6 makes 400 mean "retrying unchanged will fail
		// again", and this retries successfully once connectivity returns. Distinguished from
		// the ordinary unconfirmed 503 by its MESSAGE, which can promise nothing was created.
		if errors.Is(cerr, domain.ErrPlatformNeverSent) {
			return nil, &conn.ConnServiceUnavailableError{
				Code:    "503",
				Message: "the campaign creation never reached hubspot; nothing was created — retry, and check connectivity if it persists",
			}
		}
		if errors.Is(cerr, domain.ErrPlatformRejected) {
			// A PERMISSION rejection gets its own remedy. Retrying with another name cannot fix
			// a 401/403, so the "check the name" message would send the operator to change the
			// one thing that was never at fault.
			if errors.Is(cerr, domain.ErrPlatformPermission) {
				return nil, &conn.BadRequestError{
					Code:    "400",
					Message: "hubspot refused the campaign creation on permissions; nothing was created — check that the connection's private app token is valid and has the marketing campaigns write scope",
				}
			}
			return nil, &conn.BadRequestError{
				Code:    "400",
				Message: "hubspot rejected the campaign creation; nothing was created — check the name and try again",
			}
		}
		// Everything else is UNCONFIRMED, and the message sends the operator to HubSpot rather
		// than back through the button: this is a NON-IDEMPOTENT write into a namespace every
		// foundation shares, so a retry on an unconfirmed outcome is how a duplicate gets made.
		return nil, &conn.ConnServiceUnavailableError{
			Code:    "503",
			Message: "the campaign creation could not be confirmed — check HubSpot before creating it again, as it may already exist",
		}
	}
	// The dispatcher refuses a nil-without-error, so this cannot fire on the current path.
	// Guarded because the alternative is a nil dereference one line down, and a create that
	// reports success while returning nothing is worse than a clear failure.
	if created == nil {
		return nil, &conn.InternalServerError{Code: "500", Message: "the campaign was not returned after creation"}
	}
	return toWireHubspotCampaign(*created), nil
}

// toWireHubspotCampaign maps the domain campaign onto the wire type.
//
// `utm` and `start_date` are OPTIONAL on the wire, and an empty domain value maps to an ABSENT
// key rather than an empty string. That distinction is the point: a campaign with no configured
// token is a real campaign, and a consumer must be able to tell "no token" from "" without
// guessing which the producer meant.
func toWireHubspotCampaign(c model.HubSpotCampaign) *conn.HubspotCampaign {
	out := &conn.HubspotCampaign{ID: c.ID, Name: c.Name}
	if c.UTM != "" {
		utm := c.UTM
		out.Utm = &utm
	}
	if c.StartDate != "" {
		start := c.StartDate
		out.StartDate = &start
	}
	return out
}
