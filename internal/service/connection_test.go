// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"crypto/rand"
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

func (r *fakeRepo) Update(_ context.Context, c *model.Connection, _ int64) (*model.Connection, error) {
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
		Config:    &conn.GoogleAdsConnectionConfig{AccountID: "8666746580"},
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

func TestCreateGoogleAds_ConflictMapsToConflictError(t *testing.T) {
	repo := newFakeRepo()
	repo.store[repoKey("cncf", model.ProviderGoogleAds)] = &model.Connection{}
	s := newTestService(t, repo)
	_, err := s.CreateGoogleAds(context.Background(), &conn.CreateGoogleAdsPayload{
		ProjectID:   "cncf",
		Config:      &conn.GoogleAdsConnectionConfig{AccountID: "x"},
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
		Config:      &conn.GoogleAdsConnectionConfig{AccountID: "x"},
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

func TestUpdateGoogleAds_MissingIfMatchMapsToPreconditionRequired(t *testing.T) {
	s := newTestService(t, newFakeRepo())
	_, err := s.UpdateGoogleAds(context.Background(), &conn.UpdateGoogleAdsPayload{
		ProjectID: "cncf",
		Config:    &conn.GoogleAdsConnectionConfig{AccountID: "x"},
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
		Config:    &conn.GoogleAdsConnectionConfig{AccountID: "x"},
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
	// payload {"email":"a@b.com","preferred_username":"abc"} base64url-encoded.
	payload := "eyJlbWFpbCI6ImFAYi5jb20iLCJwcmVmZXJyZWRfdXNlcm5hbWUiOiJhYmMifQ"
	ctx, err := s.JWTAuth(context.Background(), "h."+payload+".s", nil)
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
