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
		// Optional for Google Ads alone (design/connection.go explains why): omitting it
		// stores "" and creates a credentials-only connection, which is the first step of
		// the discovery bootstrap. The column is NOT NULL TEXT, so "" is a legal value and
		// no migration is involved — "unfinished" is spelled as an empty string, not NULL.
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
// the discovery handler logs. It exists because the errors themselves cannot be logged: one
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
	case errors.Is(err, domain.ErrAccountNotSelected):
		return "account_not_selected"
	default:
		return "unclassified"
	}
}

func (s *ConnectionService) ListGoogleAdsAccounts(ctx context.Context, p *conn.ListGoogleAdsAccountsPayload) (*conn.ListGoogleAdsAccountsResult, error) {
	_, _, orch, err := s.resolveBackendWithOrch()
	if err != nil {
		return nil, err
	}
	accounts, aerr := orch.ReadAccounts(ctx, p.ProjectID, model.ProviderGoogleAds)
	if aerr != nil {
		switch {
		case errors.Is(aerr, ErrAccountsUnsupported):
			return nil, &conn.BadRequestError{Code: "400", Message: "account discovery is not supported for this platform"}
		case errors.Is(aerr, domain.ErrNotFound):
			// The project has no stored Google Ads connection. That is a client-side
			// state error, not a platform outage — reporting 503 would tell the caller
			// to retry something that can never succeed until a connection exists.
			return nil, &conn.NotFoundError{Code: "404", Message: "no google ads connection configured for this project"}
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
			slog.ErrorContext(ctx, "stored credentials failed authenticated decryption; check the application encryption key, and whether this is one row or every connection",
				"project_id", p.ProjectID, "provider", string(model.ProviderGoogleAds), "error", aerr)
			return nil, &conn.InternalServerError{Code: "500", Message: "account discovery could not be completed"}
		case errors.Is(aerr, domain.ErrConnectionNotUsable):
			// The connection EXISTS but cannot be used as it stands — inactive, an
			// incomplete credential blob, or a malformed stored config value such as a
			// dashed login_customer_id. Google is never contacted, and none of these
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
			slog.WarnContext(ctx, "google ads connection is not usable for account discovery",
				"project_id", p.ProjectID, "reason", unusableConnectionReason(aerr))
			return nil, &conn.BadRequestError{
				Code: "400",
				Message: "the stored google ads connection cannot be used as configured: check that it " +
					"is active, that the stored credential is valid json with every field set, and " +
					"that login_customer_id is digits only",
			}
		default:
			slog.WarnContext(ctx, "account discovery failed on google ads",
				"project_id", p.ProjectID, "error", aerr)
			return nil, &conn.ConnServiceUnavailableError{Code: "503", Message: "account discovery could not be completed"}
		}
	}
	// Convert model.AccessibleAccount to generated conn type. Preallocated with make so an
	// empty result serializes as `[]`, not `null` — a nil slice here would undo the
	// dispatcher's deliberate make([]model.AccessibleAccount, 0, len(customers)) one layer
	// down and hand every client a null it has to special-case.
	connAccounts := make([]*conn.AccessibleAccount, 0, len(accounts))
	for _, acct := range accounts {
		label := acct.Label // Convert to pointer
		connAccounts = append(connAccounts, &conn.AccessibleAccount{
			ID:    acct.ID,
			Label: &label,
		})
	}
	return &conn.ListGoogleAdsAccountsResult{Accounts: connAccounts}, nil
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

func (s *ConnectionService) CreateLinkedinAds(ctx context.Context, p *conn.CreateLinkedinAdsPayload) (*conn.LinkedinAdsConnection, error) {
	if err := validateConnectionProjectSlug(p.ProjectID); err != nil {
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
		AccountID: cfg.AccountID,
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
		AccountID: cfg.AccountID,
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
	return r
}

func (s *ConnectionService) CreateRedditAds(ctx context.Context, p *conn.CreateRedditAdsPayload) (*conn.RedditAdsConnection, error) {
	if err := validateConnectionProjectSlug(p.ProjectID); err != nil {
		return nil, err
	}
	cfg := p.Config
	m := &model.Connection{
		ProjectID:      p.ProjectID,
		Provider:       model.ProviderRedditAds,
		Label:          strVal(cfg.Label),
		AccountID:      cfg.AccountID,
		ProviderConfig: map[string]string{},
		CreatedBy:      actorFromCtx(ctx),
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
		ProjectID:      p.ProjectID,
		Provider:       model.ProviderRedditAds,
		Label:          strVal(cfg.Label),
		AccountID:      cfg.AccountID,
		ProviderConfig: map[string]string{},
		UpdatedBy:      actorFromCtx(ctx),
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
		AccountID: cfg.AccountID,
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
		AccountID: cfg.AccountID,
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
