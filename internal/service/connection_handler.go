// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"

	"goa.design/goa/v3/security"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// actorCtxKey is the context key under which the authenticated actor is stored.
type actorCtxKey struct{}

// JWTAuth authorizes a request and records the authenticated actor in the
// context for attribution. Authentication of the token signature and audience,
// and authorization on campaign_manager, are performed by Heimdall/OpenFGA at
// the gateway before the request reaches this service. Here we require the
// bearer token to be present and extract the principal claims for attribution.
//
// NOTE: full in-app JWKS signature verification is a follow-up; the token
// reaching this service has already been minted and validated by Heimdall.
func (s *ConnectionService) JWTAuth(ctx context.Context, token string, _ *security.JWTScheme) (context.Context, error) {
	if token == "" {
		return ctx, &conn.BadRequestError{Code: "400", Message: "missing bearer token"}
	}
	if a := actorFromToken(token); a != nil {
		ctx = context.WithValue(ctx, actorCtxKey{}, a)
	}
	return ctx, nil
}

// actorFromCtx returns the authenticated actor recorded by JWTAuth, or nil.
func actorFromCtx(ctx context.Context) *model.Actor {
	if a, ok := ctx.Value(actorCtxKey{}).(*model.Actor); ok {
		return a
	}
	return nil
}

// actorFromToken best-effort decodes the JWT payload to extract principal
// claims for attribution. It does not verify the signature (see JWTAuth).
func actorFromToken(token string) *model.Actor {
	token = strings.TrimPrefix(token, "Bearer ")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims struct {
		Name              string `json:"name"`
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		Sub               string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil
	}
	username := claims.PreferredUsername
	if username == "" {
		username = claims.Sub
	}
	if claims.Name == "" && claims.Email == "" && username == "" {
		return nil
	}
	return &model.Actor{Name: claims.Name, Email: claims.Email, Username: username}
}

// ConnectionService implements the generated connection service interface by
// delegating to the domain repository and encryptor. Per-provider methods are
// thin adapters (see connection.go) that convert the typed Goa payloads to and
// from the generic domain model and call the core helpers here.
type ConnectionService struct {
	// mu guards repo and enc, which can be swapped in after construction: during
	// a database cold start the service boots with a nil repo (every method
	// returns 503) and the container injects the live repo+encryptor via
	// SetBackend once the pool opens. Probe/handler requests read them
	// concurrently with that swap, so access is guarded.
	mu   sync.RWMutex
	repo domain.ConnectionRepository
	enc  domain.Encryptor
}

var (
	_ conn.Service = (*ConnectionService)(nil)
	_ conn.Auther  = (*ConnectionService)(nil)
)

// NewConnectionService constructs a ConnectionService. A nil repo is valid: it
// signals the database is not configured OR not yet ready, so every method
// returns the typed 503 ServiceUnavailable (see resolveBackend) instead of
// panicking on a nil repo.
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

// backend returns the current repo and encryptor under the read lock.
func (s *ConnectionService) backend() (domain.ConnectionRepository, domain.Encryptor) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repo, s.enc
}

// resolveBackend returns the repo+encryptor for one request, or the typed 503
// ServiceUnavailable error when the service has no database wired (DATABASE_URL
// unset) or the pool is still coming up. Reading both once per request (rather
// than re-reading s.repo/s.enc field-by-field) avoids racing the container's
// SetBackend swap mid-handler. The routes are still mounted in the unavailable
// mode so runtime matches the published OpenAPI contract.
func (s *ConnectionService) resolveBackend() (domain.ConnectionRepository, domain.Encryptor, error) {
	repo, enc := s.backend()
	if repo == nil {
		return nil, nil, &conn.ConnServiceUnavailableError{Code: "503", Message: "connection storage is unavailable"}
	}
	return repo, enc, nil
}

// createConn encrypts credentials, persists a new connection, and returns the
// generic domain result. Adapters build the *model.Connection (minus
// credentials) and pass the plaintext credential JSON separately.
func (s *ConnectionService) createConn(ctx context.Context, c *model.Connection, creds any) (*model.Connection, error) {
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
	repo, _, err := s.resolveBackend()
	if err != nil {
		return nil, err
	}
	c, gerr := repo.Get(ctx, projectID, p)
	return c, mapErr(gerr)
}

// updateConn replaces config, gated on the If-Match version.
func (s *ConnectionService) updateConn(ctx context.Context, c *model.Connection, ifMatch *string) (*model.Connection, error) {
	repo, _, err := s.resolveBackend()
	if err != nil {
		return nil, err
	}
	version, err := parseIfMatch(ifMatch)
	if err != nil {
		return nil, err
	}
	updated, uerr := repo.Update(ctx, c, version)
	return updated, mapErr(uerr)
}

// setCredential encrypts and replaces the stored credential.
func (s *ConnectionService) setCredential(ctx context.Context, projectID string, p model.Provider, creds any, by *model.Actor) error {
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
	repo, _, err := s.resolveBackend()
	if err != nil {
		return nil, err
	}
	c, err := repo.Get(ctx, projectID, p)
	if err != nil {
		return nil, mapErr(err)
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
