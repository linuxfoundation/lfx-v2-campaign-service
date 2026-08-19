// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/infrastructure/crypto"
)

// fakeRepo is an in-memory ConnectionRepository for handler tests.
type fakeRepo struct {
	store     map[string]*model.Connection // key: projectID|provider
	createErr error
	getErr    error
	updateErr error
	// gotUpdateVersion and gotUpdateCreds record the last Update call. The real repository
	// enforces the version check in SQL and leaves the credential column alone, so a fake
	// cannot reproduce either — but a test CAN observe what the handler PASSED, which is
	// the half that lives in the service layer.
	gotUpdateVersion int64
	// Snapshotted, not a pointer to the argument: Update mutates the struct it is given
	// (as the real repository's RETURNING does), so a retained pointer would report the
	// post-call state and the assertion would be about the fake, not the handler.
	gotUpdateCreds []byte
}

func newFakeRepo() *fakeRepo { return &fakeRepo{store: map[string]*model.Connection{}} }

func repoKey(projectID string, p model.Provider) string { return projectID + "|" + string(p) }

func (r *fakeRepo) Get(_ context.Context, projectID string, p model.Provider) (*model.Connection, error) {
	if r.getErr != nil {
		return nil, r.getErr
	}
	c, ok := r.store[repoKey(projectID, p)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return c, nil
}

func (r *fakeRepo) Create(_ context.Context, c *model.Connection) (*model.Connection, error) {
	if r.createErr != nil {
		return nil, r.createErr
	}
	k := repoKey(c.ProjectID, c.Provider)
	if _, exists := r.store[k]; exists {
		return nil, domain.ErrConflict
	}
	c.ID = "generated-id"
	c.Status = model.StatusActive
	c.Version = 1
	r.store[k] = c
	return c, nil
}

func (r *fakeRepo) Update(_ context.Context, c *model.Connection, expectedVersion int64) (*model.Connection, error) {
	r.gotUpdateVersion = expectedVersion
	r.gotUpdateCreds = c.EncryptedCredentials
	if r.updateErr != nil {
		return nil, r.updateErr
	}
	k := repoKey(c.ProjectID, c.Provider)
	existing, ok := r.store[k]
	if !ok {
		return nil, domain.ErrNotFound
	}
	c.ID = existing.ID
	c.Status = model.StatusActive
	c.Version = existing.Version + 1
	c.EncryptedCredentials = existing.EncryptedCredentials
	r.store[k] = c
	return c, nil
}

func (r *fakeRepo) SetCredential(_ context.Context, projectID string, p model.Provider, ct []byte, _ *model.Actor) (*model.Connection, error) {
	c, ok := r.store[repoKey(projectID, p)]
	if !ok {
		return nil, domain.ErrNotFound
	}
	c.EncryptedCredentials = ct
	c.Version++
	return c, nil
}

func (r *fakeRepo) UpdateWithCredential(ctx context.Context, c *model.Connection, ct []byte, expectedVersion int64) (*model.Connection, error) {
	upd, err := r.Update(ctx, c, expectedVersion)
	if err != nil {
		return nil, err
	}
	upd.EncryptedCredentials = ct
	return upd, nil
}

func (r *fakeRepo) Delete(_ context.Context, projectID string, p model.Provider, _ *model.Actor) error {
	if _, ok := r.store[repoKey(projectID, p)]; !ok {
		return domain.ErrNotFound
	}
	delete(r.store, repoKey(projectID, p))
	return nil
}

func newTestService(t *testing.T, repo domain.ConnectionRepository) *ConnectionService {
	t.Helper()
	k := make([]byte, crypto.KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("key: %v", err)
	}
	enc, err := crypto.NewAESGCM(k)
	if err != nil {
		t.Fatalf("enc: %v", err)
	}
	return NewConnectionService(repo, enc)
}

func TestCreateGoogleAds_HappyPath(t *testing.T) {
	s := newTestService(t, newFakeRepo())
	res, err := s.CreateGoogleAds(context.Background(), &conn.CreateGoogleAdsPayload{
		ProjectID: "cncf",
		Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("8666746580")},
		Credentials: &conn.GoogleAdsCredentials{
			RefreshToken: "rt", ClientID: "ci", ClientSecret: "cs", DeveloperToken: "dt",
		},
	})
	if err != nil {
		t.Fatalf("CreateGoogleAds: %v", err)
	}
	if res.AccountID != "8666746580" {
		t.Errorf("account_id = %q, want 8666746580", res.AccountID)
	}
	if !res.HasCredentials {
		t.Error("expected has_credentials = true")
	}
	if res.Etag != "1" {
		t.Errorf("etag = %q, want 1", res.Etag)
	}
}

// Connection CREATE must reject a UUID project_id (only a canonical slug is
// dispatchable — brief/campaign create require a slug and dispatch does an exact-match
// lookup). Get/update/delete/set/test stay permissive for historical UUID rows.
func TestCreateConnection_RejectsUUIDProjectID(t *testing.T) {
	s := newTestService(t, newFakeRepo())
	_, err := s.CreateGoogleAds(context.Background(), &conn.CreateGoogleAdsPayload{
		ProjectID: "a09410d0-0ec0-11ea-8e8f-416e2d8da950", // a UUID, not a slug
		Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("8666746580")},
		Credentials: &conn.GoogleAdsCredentials{
			RefreshToken: "rt", ClientID: "ci", ClientSecret: "cs", DeveloperToken: "dt",
		},
	})
	var bad *conn.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("a UUID project_id must be a BadRequestError, got %T (%v)", err, err)
	}
}

// A different provider's create path shares the same guard — spot-check reddit so the
// guard isn't accidentally applied to only one provider.
func TestCreateRedditAds_RejectsUUIDProjectID(t *testing.T) {
	s := newTestService(t, newFakeRepo())
	_, err := s.CreateRedditAds(context.Background(), &conn.CreateRedditAdsPayload{
		ProjectID:   "a09410d0-0ec0-11ea-8e8f-416e2d8da950",
		Config:      &conn.RedditAdsConnectionConfig{AccountID: "t2_gv9wtbfa"},
		Credentials: &conn.RedditAdsCredentials{ClientID: "c", ClientSecret: "s", RefreshToken: "r"},
	})
	var bad *conn.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("a UUID project_id must be a BadRequestError, got %T (%v)", err, err)
	}
}

func TestCreateGoogleAds_ConflictMapsToConflictError(t *testing.T) {
	repo := newFakeRepo()
	repo.store[repoKey("cncf", model.ProviderGoogleAds)] = &model.Connection{}
	s := newTestService(t, repo)
	_, err := s.CreateGoogleAds(context.Background(), &conn.CreateGoogleAdsPayload{
		ProjectID:   "cncf",
		Config:      &conn.GoogleAdsConnectionConfig{AccountID: strPtr("x")},
		Credentials: &conn.GoogleAdsCredentials{RefreshToken: "a", ClientID: "b", ClientSecret: "c", DeveloperToken: "d"},
	})
	if _, ok := err.(*conn.ConflictError); !ok {
		t.Fatalf("expected *conn.ConflictError, got %T (%v)", err, err)
	}
}

func TestGetGoogleAds_NotFoundMapsToNotFoundError(t *testing.T) {
	s := newTestService(t, newFakeRepo())
	_, err := s.GetGoogleAds(context.Background(), &conn.GetGoogleAdsPayload{ProjectID: "cncf"})
	if _, ok := err.(*conn.NotFoundError); !ok {
		t.Fatalf("expected *conn.NotFoundError, got %T (%v)", err, err)
	}
}

func TestNilRepo_ReturnsServiceUnavailable(t *testing.T) {
	// A service built without a repo (DATABASE_URL unset) must return the typed
	// 503 ServiceUnavailable for every route, not panic on a nil repo — this is
	// what keeps runtime behavior consistent with the published OpenAPI contract.
	s := NewConnectionService(nil, nil)

	if _, err := s.GetGoogleAds(context.Background(), &conn.GetGoogleAdsPayload{ProjectID: "cncf"}); !isServiceUnavailable(err) {
		t.Errorf("GetGoogleAds: expected *conn.ConnServiceUnavailableError, got %T (%v)", err, err)
	}
	if _, err := s.CreateGoogleAds(context.Background(), &conn.CreateGoogleAdsPayload{
		ProjectID:   "cncf",
		Config:      &conn.GoogleAdsConnectionConfig{AccountID: strPtr("x")},
		Credentials: &conn.GoogleAdsCredentials{RefreshToken: "a", ClientID: "b", ClientSecret: "c", DeveloperToken: "d"},
	}); !isServiceUnavailable(err) {
		t.Errorf("CreateGoogleAds: expected *conn.ConnServiceUnavailableError, got %T (%v)", err, err)
	}
	if err := s.DeleteGoogleAds(context.Background(), &conn.DeleteGoogleAdsPayload{ProjectID: "cncf"}); !isServiceUnavailable(err) {
		t.Errorf("DeleteGoogleAds: expected *conn.ConnServiceUnavailableError, got %T (%v)", err, err)
	}
}

func isServiceUnavailable(err error) bool {
	_, ok := err.(*conn.ConnServiceUnavailableError)
	return ok
}

// TestSetBackend_LateBinding verifies the container can inject the repo+encryptor
// after construction (the DB cold-start path): a service booted with a nil repo
// returns 503, and once SetBackend injects a live repo the same call succeeds —
// without rebuilding the service (its routes are already mounted).
func TestSetBackend_LateBinding(t *testing.T) {
	s := NewConnectionService(nil, nil)
	// Before the pool is ready: 503.
	if _, err := s.GetGoogleAds(context.Background(), &conn.GetGoogleAdsPayload{ProjectID: "cncf"}); !isServiceUnavailable(err) {
		t.Fatalf("expected 503 before backend is set, got %T (%v)", err, err)
	}

	// Inject a live repo+encryptor (as the background DB-init goroutine does).
	k := make([]byte, crypto.KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("key: %v", err)
	}
	enc, err := crypto.NewAESGCM(k)
	if err != nil {
		t.Fatalf("enc: %v", err)
	}
	s.SetBackend(newFakeRepo(), enc)

	// After the swap: the repo is consulted; a missing connection is NotFound, NOT
	// 503 — proving the backend went live.
	if _, err := s.GetGoogleAds(context.Background(), &conn.GetGoogleAdsPayload{ProjectID: "cncf"}); isServiceUnavailable(err) {
		t.Fatalf("expected the live repo to be consulted after SetBackend, still got 503")
	}
}

func TestUpdateGoogleAds_MissingIfMatchMapsToPreconditionRequired(t *testing.T) {
	s := newTestService(t, newFakeRepo())
	_, err := s.UpdateGoogleAds(context.Background(), &conn.UpdateGoogleAdsPayload{
		ProjectID: "cncf",
		Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("x")},
		IfMatch:   nil,
	})
	if _, ok := err.(*conn.PreconditionRequiredError); !ok {
		t.Fatalf("expected *conn.PreconditionRequiredError, got %T (%v)", err, err)
	}
}

func TestUpdateGoogleAds_StaleETagMapsToPreconditionFailed(t *testing.T) {
	// A version mismatch from the repo (stale If-Match) must surface as 412
	// Precondition Failed — the core of the optimistic-concurrency contract.
	repo := newFakeRepo()
	repo.store[repoKey("cncf", model.ProviderGoogleAds)] = &model.Connection{Version: 5}
	repo.updateErr = domain.ErrPreconditionFailed
	s := newTestService(t, repo)
	ifMatch := "3"
	_, err := s.UpdateGoogleAds(context.Background(), &conn.UpdateGoogleAdsPayload{
		ProjectID: "cncf",
		Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("x")},
		IfMatch:   &ifMatch,
	})
	if _, ok := err.(*conn.PreconditionFailedError); !ok {
		t.Fatalf("expected *conn.PreconditionFailedError, got %T (%v)", err, err)
	}
}

func TestLinkedInAds_RoundTripsOrgID(t *testing.T) {
	s := newTestService(t, newFakeRepo())
	res, err := s.CreateLinkedinAds(context.Background(), &conn.CreateLinkedinAdsPayload{
		ProjectID:   "tlf",
		Config:      &conn.LinkedinAdsConnectionConfig{AccountID: "538170226", OrgID: "208777"},
		Credentials: &conn.LinkedinAdsCredentials{AccessToken: "tok"},
	})
	if err != nil {
		t.Fatalf("CreateLinkedinAds: %v", err)
	}
	if res.OrgID == nil || *res.OrgID != "208777" {
		t.Errorf("org_id = %v, want 208777", res.OrgID)
	}
}

func TestJWTAuth_ExtractsActorFromToken(t *testing.T) {
	s := newTestService(t, newFakeRepo())
	s.SetTokenVerifier(verifierFor("abc-token", &model.Actor{Username: "abc", Email: "a@b.com"}))
	ctx, err := s.JWTAuth(context.Background(), "abc-token", nil)
	if err != nil {
		t.Fatalf("JWTAuth: %v", err)
	}
	a := actorFromCtx(ctx)
	if a == nil || a.Email != "a@b.com" || a.Username != "abc" {
		t.Fatalf("actor = %+v, want email a@b.com username abc", a)
	}
}

func TestJWTAuth_EmptyTokenRejected(t *testing.T) {
	s := newTestService(t, newFakeRepo())
	if _, err := s.JWTAuth(context.Background(), "", nil); err == nil {
		t.Fatal("expected error for empty token")
	}
}

// TestSystemScopeIsUnreachableThroughTheAPI: no API caller may read, rewrite, re-credential,
// test, delete the reserved scope, or enumerate the accounts it reaches. The cases cover EVERY
// endpoint taking a caller-supplied project_id — seven, not six: account discovery bypasses
// connection_handler.go, which is why it was missed; an eighth belongs here too. The row EXISTS
// for each case, or the repo's own "not found" would make a guarded service look unguarded.
func TestSystemScopeIsUnreachableThroughTheAPI(t *testing.T) {
	newRepoWithSystemRow := func() *fakeRepo {
		r := newFakeRepo()
		r.store[model.SystemProjectID+"|"+string(model.ProviderGoogleAds)] = &model.Connection{
			ProjectID: model.SystemProjectID, Provider: model.ProviderGoogleAds,
			AccountID: "8666746580", EncryptedCredentials: []byte("ct"),
			Status: model.StatusActive, Version: 1,
		}
		return r
	}
	etag := "1"
	cases := map[string]func(*ConnectionService) error{
		"get": func(s *ConnectionService) error {
			_, err := s.GetGoogleAds(context.Background(), &conn.GetGoogleAdsPayload{ProjectID: model.SystemProjectID})
			return err
		},
		"update": func(s *ConnectionService) error {
			_, err := s.UpdateGoogleAds(context.Background(), &conn.UpdateGoogleAdsPayload{
				ProjectID: model.SystemProjectID, IfMatch: &etag,
				Config: &conn.GoogleAdsConnectionConfig{AccountID: strPtr("1")},
			})
			return err
		},
		"set-credential": func(s *ConnectionService) error {
			return s.SetCredentialGoogleAds(context.Background(), &conn.SetCredentialGoogleAdsPayload{
				ProjectID: model.SystemProjectID,
				Credentials: &conn.GoogleAdsCredentials{
					RefreshToken: "rt", ClientID: "ci", ClientSecret: "cs", DeveloperToken: "dt",
				},
			})
		},
		"delete": func(s *ConnectionService) error {
			return s.DeleteGoogleAds(context.Background(), &conn.DeleteGoogleAdsPayload{ProjectID: model.SystemProjectID})
		},
		"test": func(s *ConnectionService) error {
			_, err := s.TestGoogleAds(context.Background(), &conn.TestGoogleAdsPayload{ProjectID: model.SystemProjectID})
			return err
		},
		// The SEVENTH endpoint. Given a WORKING orchestrator on purpose: without one the call
		// 503s before the guard matters and this would pass against an unguarded service.
		"list-accounts": func(s *ConnectionService) error {
			s.SetOrchestrator(&Orchestrator{
				dispatchers: map[model.Provider]PlatformDispatcher{
					model.ProviderGoogleAds: &mockAccountListerDispatcher{
						accounts: []model.AccessibleAccount{{ID: "customers/8666746580", Label: "Linux Foundation"}},
					},
				},
			})
			_, err := s.ListGoogleAdsAccounts(context.Background(), &conn.ListGoogleAdsAccountsPayload{
				ProjectID: model.SystemProjectID,
			})
			return err
		},
		"create": func(s *ConnectionService) error {
			_, err := s.CreateGoogleAds(context.Background(), &conn.CreateGoogleAdsPayload{
				ProjectID: model.SystemProjectID,
				Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("1")},
				Credentials: &conn.GoogleAdsCredentials{
					RefreshToken: "rt", ClientID: "ci", ClientSecret: "cs", DeveloperToken: "dt",
				},
			})
			return err
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			repo := newRepoWithSystemRow()
			err := call(newTestService(t, repo))
			if err == nil {
				t.Fatalf("%s at the reserved scope succeeded; it must be refused", name)
			}
			// Asserted PER ROUTE rather than as "either refusal", because which refusal
			// is the contract here. Create is rejected by the slug pattern before any
			// lookup, and a pattern violation is a 400. Every other route reaches its own
			// guard and must answer 404 specifically: 403 — or a 400 that says the id is
			// malformed — tells an unauthorized caller that something is at this scope,
			// which is the disclosure the guard exists to prevent. Accepting either status
			// would let a guard drift onto the wrong one and still pass.
			if name == "create" {
				if _, ok := err.(*conn.BadRequestError); !ok {
					t.Fatalf("create err = %T (%v), want BadRequestError", err, err)
				}
				return
			}
			if _, ok := err.(*conn.NotFoundError); !ok {
				t.Fatalf("%s err = %T (%v), want NotFoundError", name, err, err)
			}
		})
	}
}

// TestCreateGoogleAds_WithoutAccountID pins the create half of the credentials-first
// bootstrap: POST with credentials and no account id must SUCCEED and store "".
//
// Three assertions, each guarding a different way this could regress:
//
//  1. It is accepted at all — with the key OMITTED. Goa enforces Required at the transport
//     layer, and the Required("account_id") this change removed was a presence check on the
//     JSON key (`if body.AccountID == nil`), so it rejected only OMISSION; an explicit
//     `"account_id": ""` always got through. Omission is the shape the bootstrap flow
//     actually sends, which is why this test omits rather than empties the field.
//  2. status is ACTIVE. This is not cosmetic — validateGoogleAdsCredentials refuses a
//     non-active connection, so a "pending"-style status here would leave the connection
//     unable to reach the discovery endpoint that exists to finish it, and the bootstrap
//     would dead-end at step two.
//  3. account_id round-trips as "". The response type still declares it Required, which is
//     satisfied by an empty string because the Go field is a plain string; if it ever
//     becomes a pointer, the response contract has to change with it and this fails.
func TestCreateGoogleAds_WithoutAccountID(t *testing.T) {
	s := newTestService(t, newFakeRepo())
	res, err := s.CreateGoogleAds(context.Background(), &conn.CreateGoogleAdsPayload{
		ProjectID: "cncf",
		// AccountID is nil EXPLICITLY. The absence is the subject of this test, not an
		// incidental omission, and spelling it out keeps that legible if the fixture is
		// ever copied — a reader who does not notice a missing field will notice a nil one.
		Config: &conn.GoogleAdsConnectionConfig{Label: strPtr("TLF Main"), AccountID: nil},
		Credentials: &conn.GoogleAdsCredentials{
			RefreshToken: "rt", ClientID: "ci", ClientSecret: "cs", DeveloperToken: "dt",
		},
	})
	if err != nil {
		t.Fatalf("a credentials-only connection must be creatable: %v", err)
	}
	if res.AccountID != "" {
		t.Errorf("account_id = %q, want the empty string", res.AccountID)
	}
	if res.Status != string(model.StatusActive) {
		t.Errorf("status = %q, want %q — discovery refuses a non-active connection, so any "+
			"other status would make the account unchoosable", res.Status, model.StatusActive)
	}
	if !res.HasCredentials {
		t.Error("expected has_credentials = true: the credentials are exactly what WAS supplied")
	}
}

// TestCreateMetaAds_WithoutAccountID is the Meta counterpart, and the half the transport test
// does not reach: that test proves Goa ACCEPTS an omitted `account_id`, which is a statement
// about the generated decoder, not about what this service then persists and returns.
//
// The assertions are the Google Ads three, and each means something slightly different here:
//
//  1. Accepted with the key OMITTED, which is what the bootstrap flow sends. An explicit
//     `"account_id": ""` always got through the Required presence check, so only omission
//     distinguishes the loosened contract from the old one.
//  2. status is ACTIVE. resolveMetaCredentials refuses a non-active connection, so any other
//     status would make GET .../connection-meta-ads/accounts unreachable and dead-end the
//     bootstrap at step two — the same trap as Google Ads, via a different helper.
//  3. account_id round-trips as "". `page_id` is supplied because it stays Required: it names
//     a Facebook page the operator already controls, so discovery never resolves it and it is
//     not part of what deferring account selection defers.
func TestCreateMetaAds_WithoutAccountID(t *testing.T) {
	s := newTestService(t, newFakeRepo())
	res, err := s.CreateMetaAds(context.Background(), &conn.CreateMetaAdsPayload{
		ProjectID: "cncf",
		// AccountID is nil EXPLICITLY — the absence is the subject of this test.
		Config: &conn.MetaAdsConnectionConfig{
			Label: strPtr("TLF Main"), AccountID: nil, PageID: "page-1",
		},
		Credentials: &conn.MetaAdsCredentials{AccessToken: "at", AppSecret: "as"},
	})
	if err != nil {
		t.Fatalf("a credentials-only connection must be creatable: %v", err)
	}
	if res.AccountID != "" {
		t.Errorf("account_id = %q, want the empty string", res.AccountID)
	}
	if res.Status != string(model.StatusActive) {
		t.Errorf("status = %q, want %q — resolveMetaCredentials refuses a non-active connection, "+
			"so any other status would make the account unchoosable", res.Status, model.StatusActive)
	}
	if !res.HasCredentials {
		t.Error("expected has_credentials = true: the credentials are exactly what WAS supplied")
	}
}

// TestUpdateGoogleAds_BindsDiscoveredAccountToCredentialsOnlyRow is the second half of the
// credentials-first bootstrap, and the step the existing update tests never exercised: they
// only cover missing and stale If-Match. Here the stored row is the state a POST-with-
// credentials leaves behind — active, credentials present, account_id empty — and the PUT
// carries the id the operator picked from the accounts endpoint.
//
// The credential assertion is on the ARGUMENT, not the stored row: preserving the column is
// the repository's job in SQL, and the fake reproduces that, so asserting the stored value
// would pass against a handler that overwrote it. What the service layer owns is not SENDING
// a credential — PUT deliberately does not accept one (set-credential is separately
// permissioned) — and a handler that populated the field with the payload's zero value would
// blank the very credentials that made discovery possible, dead-ending the bootstrap one step
// from the end.
func TestUpdateGoogleAds_BindsDiscoveredAccountToCredentialsOnlyRow(t *testing.T) {
	repo := newFakeRepo()
	repo.store[repoKey("cncf", model.ProviderGoogleAds)] = &model.Connection{
		ProjectID: "cncf", Provider: model.ProviderGoogleAds, Status: model.StatusActive,
		AccountID: "", Version: 4, EncryptedCredentials: []byte("ciphertext"),
	}
	s := newTestService(t, repo)
	ifMatch := "4"

	res, err := s.UpdateGoogleAds(context.Background(), &conn.UpdateGoogleAdsPayload{
		ProjectID: "cncf",
		Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("123-456-7890")},
		IfMatch:   &ifMatch,
	})
	if err != nil {
		t.Fatalf("UpdateGoogleAds: %v", err)
	}
	if res.AccountID != "123-456-7890" {
		t.Errorf("account_id = %q, want the discovered id to be bound", res.AccountID)
	}
	if repo.gotUpdateCreds != nil {
		t.Errorf("Update was passed credentials %q; a config-only PUT must leave the column to the repository",
			repo.gotUpdateCreds)
	}
	if repo.gotUpdateVersion != 4 {
		t.Errorf("expected version passed to Update = %d, want 4 from If-Match", repo.gotUpdateVersion)
	}
}

// TestUpdateGoogleAds_OmittedAccountIDClearsTheSelection pins the other direction, which the
// handler documents as intentional: PUT is a full replace, so omitting account_id UN-selects
// the account rather than leaving the previous one in place. That is the only way to undo a
// selection, and it is easy to "fix" into a merge by someone who reads the omission as
// "unchanged" — hence a test rather than only a comment. The credential and version
// assertions are on the Update ARGUMENT, for the reason given above.
func TestUpdateGoogleAds_OmittedAccountIDClearsTheSelection(t *testing.T) {
	repo := newFakeRepo()
	repo.store[repoKey("cncf", model.ProviderGoogleAds)] = &model.Connection{
		ProjectID: "cncf", Provider: model.ProviderGoogleAds, Status: model.StatusActive,
		AccountID: "123-456-7890", Version: 7, EncryptedCredentials: []byte("ciphertext"),
	}
	s := newTestService(t, repo)
	ifMatch := "7"

	res, err := s.UpdateGoogleAds(context.Background(), &conn.UpdateGoogleAdsPayload{
		ProjectID: "cncf",
		Config:    &conn.GoogleAdsConnectionConfig{Label: strPtr("relabelled")},
		IfMatch:   &ifMatch,
	})
	if err != nil {
		t.Fatalf("UpdateGoogleAds: %v", err)
	}
	if res.AccountID != "" {
		t.Errorf("account_id = %q, want an omitted account_id to clear the selection", res.AccountID)
	}
	if repo.gotUpdateCreds != nil {
		t.Errorf("Update was passed credentials %q; a config-only PUT must leave the column alone",
			repo.gotUpdateCreds)
	}
	if repo.gotUpdateVersion != 7 {
		t.Errorf("expected version passed to Update = %d, want 7 from If-Match", repo.gotUpdateVersion)
	}
}

// TestUpdateMetaAds_BindsDiscoveredAccountToCredentialsOnlyRow is the second half of the
// Meta credentials-first bootstrap, and the step the existing update tests never exercised: they
// only cover missing and stale If-Match. Here the stored row is the state a POST-with-
// credentials leaves behind — active, credentials present, account_id empty — and the PUT
// carries the id the operator picked from the accounts endpoint.
//
// The credential assertion is on the ARGUMENT, not the stored row: preserving the column is
// the repository's job in SQL, and the fake reproduces that, so asserting the stored value
// would pass against a handler that overwrote it. What the service layer owns is not SENDING
// a credential — PUT deliberately does not accept one (set-credential is separately
// permissioned) — and a handler that populated the field with the payload's zero value would
// blank the very credentials that made discovery possible, dead-ending the bootstrap one step
// from the end.
func TestUpdateMetaAds_BindsDiscoveredAccountToCredentialsOnlyRow(t *testing.T) {
	repo := newFakeRepo()
	repo.store[repoKey("cncf", model.ProviderMetaAds)] = &model.Connection{
		ProjectID: "cncf", Provider: model.ProviderMetaAds, Status: model.StatusActive,
		AccountID: "", Version: 4, EncryptedCredentials: []byte("ciphertext"),
	}
	s := newTestService(t, repo)
	ifMatch := "4"

	res, err := s.UpdateMetaAds(context.Background(), &conn.UpdateMetaAdsPayload{
		ProjectID: "cncf",
		Config: &conn.MetaAdsConnectionConfig{
			AccountID: strPtr("act_123456789"),
			// page_id is Required on the config type, and the config type is shared by
			// POST and PUT (gen/.../server/types.go ValidateUpdateMetaAdsRequestBody calls
			// the same ValidateMetaAdsConnectionConfigRequestBody). Omitting it here would
			// build a payload the transport rejects — and worse, PageID is a plain string
			// rather than a pointer, so the omission would silently write page_id="" and
			// the test would pin a state no request can produce.
			PageID: "123456789012345",
		},
		IfMatch: &ifMatch,
	})
	if err != nil {
		t.Fatalf("UpdateMetaAds: %v", err)
	}
	if res.AccountID != "act_123456789" {
		t.Errorf("account_id = %q, want the discovered id to be bound", res.AccountID)
	}
	// Binding the account must not cost the page: a PUT that blanked page_id would leave
	// the connection unable to attach the promoted object, i.e. unable to dispatch — the
	// same dead end this whole bootstrap exists to avoid.
	if derefStr(res.PageID) != "123456789012345" {
		t.Errorf("page_id = %q, want the PUT to carry it through", derefStr(res.PageID))
	}
	if repo.gotUpdateCreds != nil {
		t.Errorf("Update was passed credentials %q; a config-only PUT must leave the column to the repository",
			repo.gotUpdateCreds)
	}
	if repo.gotUpdateVersion != 4 {
		t.Errorf("expected version passed to Update = %d, want 4 from If-Match", repo.gotUpdateVersion)
	}
}

// TestUpdateMetaAds_OmittedAccountIDClearsTheSelection pins the other direction, which the
// handler documents as intentional: PUT is a full replace, so omitting account_id UN-selects
// the account rather than leaving the previous one in place. That is the only way to undo a
// selection, and it is easy to "fix" into a merge by someone who reads the omission as
// "unchanged" — hence a test rather than only a comment. The credential and version
// assertions are on the Update ARGUMENT, for the reason given above.
func TestUpdateMetaAds_OmittedAccountIDClearsTheSelection(t *testing.T) {
	repo := newFakeRepo()
	repo.store[repoKey("cncf", model.ProviderMetaAds)] = &model.Connection{
		ProjectID: "cncf", Provider: model.ProviderMetaAds, Status: model.StatusActive,
		AccountID: "act_123456789", Version: 7, EncryptedCredentials: []byte("ciphertext"),
	}
	s := newTestService(t, repo)
	ifMatch := "7"

	res, err := s.UpdateMetaAds(context.Background(), &conn.UpdateMetaAdsPayload{
		ProjectID: "cncf",
		Config: &conn.MetaAdsConnectionConfig{
			Label: strPtr("relabelled"),
			// Required on the shared config type; see the sibling test above.
			PageID: "123456789012345",
		},
		IfMatch: &ifMatch,
	})
	if err != nil {
		t.Fatalf("UpdateMetaAds: %v", err)
	}
	if res.AccountID != "" {
		t.Errorf("account_id = %q, want an omitted account_id to clear the selection", res.AccountID)
	}
	// The clear is scoped to account_id alone. Supplying page_id is what makes that a real
	// assertion: with it omitted, "page_id survived" and "page_id was blanked" are the same
	// observation, and the test could not tell a targeted clear from a full wipe.
	if derefStr(res.PageID) != "123456789012345" {
		t.Errorf("page_id = %q, want clearing account_id to leave the page alone", derefStr(res.PageID))
	}
	if repo.gotUpdateCreds != nil {
		t.Errorf("Update was passed credentials %q; a config-only PUT must leave the column alone",
			repo.gotUpdateCreds)
	}
	if repo.gotUpdateVersion != 7 {
		t.Errorf("expected version passed to Update = %d, want 7 from If-Match", repo.gotUpdateVersion)
	}
}

// The conversion pixel must survive create → read-back, and a PUT that omits it must CLEAR
// it. Both halves matter and neither was pinned.
//
// Reddit requires the pixel on every campaign create, so an update that silently drops it
// turns the next dispatch into a refusal — an operator editing only the label would break
// campaign creation without touching anything that looks related. That is the documented
// full-replace semantic of PUT (see the Update handlers), not a bug, but it is exactly the
// kind of behaviour that needs a test recording it where someone will find it.
func TestRedditAdsConversionPixelRoundTripAndClear(t *testing.T) {
	repo := newFakeRepo()
	s := newTestService(t, repo)
	ctx := context.Background()

	created, err := s.CreateRedditAds(ctx, &conn.CreateRedditAdsPayload{
		ProjectID: "cncf",
		Config: &conn.RedditAdsConnectionConfig{
			AccountID: "t2_gv9wtbfa",
			// Deliberately DIFFERENT from AccountID: on the LF account the two share a
			// value, and a fixture that repeats it cannot tell the two fields apart.
			ConversionPixelID: strPtr("a2_pixel_round_trip"),
		},
		Credentials: &conn.RedditAdsCredentials{ClientID: "c", ClientSecret: "s", RefreshToken: "r"},
	})
	if err != nil {
		t.Fatalf("CreateRedditAds: %v", err)
	}
	if created.ConversionPixelID == nil || *created.ConversionPixelID != "a2_pixel_round_trip" {
		t.Fatalf("create did not return the stored pixel: %v", created.ConversionPixelID)
	}

	got, err := s.GetRedditAds(ctx, &conn.GetRedditAdsPayload{ProjectID: "cncf"})
	if err != nil {
		t.Fatalf("GetRedditAds: %v", err)
	}
	if got.ConversionPixelID == nil || *got.ConversionPixelID != "a2_pixel_round_trip" {
		t.Errorf("read-back pixel = %v, want a2_pixel_round_trip — a value that does not survive the round trip is unusable at dispatch", got.ConversionPixelID)
	}

	// PUT is a full replace: omitting the pixel clears it. Recorded rather than asserted as
	// desirable — the next dispatch refuses, which is the safe outcome but a surprising one.
	updated, err := s.UpdateRedditAds(ctx, &conn.UpdateRedditAdsPayload{
		ProjectID: "cncf",
		IfMatch:   strPtr(etag(got.Version)),
		Config:    &conn.RedditAdsConnectionConfig{AccountID: "t2_gv9wtbfa"},
	})
	if err != nil {
		t.Fatalf("UpdateRedditAds: %v", err)
	}
	if updated.ConversionPixelID != nil && *updated.ConversionPixelID != "" {
		t.Errorf("an update omitting conversion_pixel_id must CLEAR it (PUT is a full replace), got %v", *updated.ConversionPixelID)
	}
}

// TestLinkedInRefreshCredentialsAreAllOrNone pins the all-or-none rule on the optional
// refresh trio. LinkedIn's exchange needs refresh_token, client_id AND client_secret
// together, and CanRefresh() gates on all three — so storing a partial set would
// SILENTLY degrade the connection to bearer-only while the operator believes renewal is
// configured, recreating the very 60-day expiry this feature prevents.
func TestLinkedInRefreshCredentialsAreAllOrNone(t *testing.T) {
	s := func(v string) *string { return &v }

	cases := []struct {
		name    string
		creds   *conn.LinkedinAdsCredentials
		wantErr bool
	}{
		{"bearer-only (the common non-MDP case)", &conn.LinkedinAdsCredentials{AccessToken: "at"}, false},
		{"all three present", &conn.LinkedinAdsCredentials{
			AccessToken: "at", RefreshToken: s("rt"), ClientID: s("ci"), ClientSecret: s("cs")}, false},
		{"refresh token alone", &conn.LinkedinAdsCredentials{
			AccessToken: "at", RefreshToken: s("rt")}, true},
		{"refresh token without secret", &conn.LinkedinAdsCredentials{
			AccessToken: "at", RefreshToken: s("rt"), ClientID: s("ci")}, true},
		{"client credentials without refresh token", &conn.LinkedinAdsCredentials{
			AccessToken: "at", ClientID: s("ci"), ClientSecret: s("cs")}, true},
		// Whitespace is not presence: a blank field cannot participate in an exchange.
		{"whitespace-only companions", &conn.LinkedinAdsCredentials{
			AccessToken: "at", RefreshToken: s("rt"), ClientID: s("  "), ClientSecret: s("\t")}, true},
		{"nil payload", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLinkedInRefreshCredentials(tc.creds)
			if tc.wantErr && err == nil {
				t.Fatal("expected a 400 for a partial refresh credential set")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr {
				var bad *conn.BadRequestError
				if !errors.As(err, &bad) {
					t.Errorf("err = %T, want *conn.BadRequestError so the caller gets a 400", err)
				}
			}
		})
	}
}
