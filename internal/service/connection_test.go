// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
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
	// createCalls and setCredentialCalls count writes that REACHED the repository.
	//
	// A rejected payload must not persist, and the error alone cannot show that: every
	// write method here also fails on its own for an unrelated reason (Create returns
	// ErrConflict on a duplicate, SetCredential returns ErrNotFound when the row is
	// absent), so a test that only asserted a 400 would pass identically against a
	// handler that validated nothing and merely tripped over the fixture. Counting the
	// call separates "refused before the write" from "attempted the write and lost".
	createCalls        int
	setCredentialCalls int
	// gets counts Get calls. updateConn's current-row read is CONDITIONAL on the
	// force-system guard applying, and that conditionality is a real behaviour (an extra
	// database round-trip on every paid-ads update, in every deployment, when the flag is
	// off) that no assertion on the RESULT can observe — both the guarded and unguarded
	// shapes return the same connection. Counting the calls is what makes it testable.
	gets int
	// nilNilGet makes Get return (nil, nil) — the shape domain.ConnectionReader permits and
	// does not forbid. A fake that only ever reports absence as ErrNotFound cannot exercise
	// a caller's nil-row handling, so a missing guard reads as covered.
	nilNilGet bool
}

func newFakeRepo() *fakeRepo { return &fakeRepo{store: map[string]*model.Connection{}} }

func repoKey(projectID string, p model.Provider) string { return projectID + "|" + string(p) }

func (r *fakeRepo) Get(_ context.Context, projectID string, p model.Provider) (*model.Connection, error) {
	r.gets++
	if r.nilNilGet {
		return nil, nil
	}
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
	r.createCalls++
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
	r.setCredentialCalls++
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

// TestTestLinkedinAds_NoOrchestratorIs503 pins that, once the shared testConn baseline
// passes, verifying the account/org pairing is treated the SAME way account discovery is —
// requiring an orchestrator — rather than silently skipping the check when none is wired.
func TestTestLinkedinAds_NoOrchestratorIs503(t *testing.T) {
	s := newTestService(t, newFakeRepo())
	if _, err := s.CreateLinkedinAds(context.Background(), &conn.CreateLinkedinAdsPayload{
		ProjectID:   "tlf",
		Config:      &conn.LinkedinAdsConnectionConfig{AccountID: "538170226", OrgID: "208777"},
		Credentials: &conn.LinkedinAdsCredentials{AccessToken: "tok"},
	}); err != nil {
		t.Fatalf("CreateLinkedinAds: %v", err)
	}
	_, err := s.TestLinkedinAds(context.Background(), &conn.TestLinkedinAdsPayload{ProjectID: "tlf"})
	if _, ok := err.(*conn.ConnServiceUnavailableError); !ok {
		t.Fatalf("expected *conn.ConnServiceUnavailableError with no orchestrator wired, got %T (%v)", err, err)
	}
}

// TestTestLinkedinAds_NoCredentialsSkipsUpstreamVerification pins that a connection which
// fails the shared testConn baseline (no credentials) is reported as-is — OK: false — without
// ever reaching the orchestrator's upstream check. There is nothing to verify an org pairing
// against without credentials, and the orchestrator being unset here (it would 503, per the
// test above) proves the short-circuit actually happened rather than merely succeeding too.
func TestTestLinkedinAds_NoCredentialsSkipsUpstreamVerification(t *testing.T) {
	repo := newFakeRepo()
	repo.store[repoKey("tlf", model.ProviderLinkedInAds)] = &model.Connection{
		ProjectID: "tlf", Provider: model.ProviderLinkedInAds,
		AccountID: "538170226", ProviderConfig: map[string]string{"org_id": "208777"},
		Status: model.StatusActive, Version: 1,
		// EncryptedCredentials deliberately empty — HasCredentials() must be false.
	}
	s := newTestService(t, repo)
	res, err := s.TestLinkedinAds(context.Background(), &conn.TestLinkedinAdsPayload{ProjectID: "tlf"})
	if err != nil {
		t.Fatalf("TestLinkedinAds: %v", err)
	}
	if res.OK {
		t.Error("OK = true for a connection with no stored credentials")
	}
	// The message must name the real reason. testConn is shared across providers and its
	// generic text says upstream verification is "not yet implemented" — false here, since
	// TestLinkedinAds implements it — so an operator handed that text goes looking for a
	// missing feature instead of authorizing the connection.
	if res.Message == nil {
		t.Fatal("Message = nil; the operator is told nothing about why the test failed")
	}
	if strings.Contains(*res.Message, "not yet implemented") {
		t.Errorf("Message = %q, still claims upstream verification is unimplemented", *res.Message)
	}
	if !strings.Contains(*res.Message, "no credentials are stored") {
		t.Errorf("Message = %q, does not name the absent credential as the reason", *res.Message)
	}
}

// TestTestLinkedinAds_UpstreamVerification exercises the new behavior beyond the testConn
// baseline: once a credentialed connection passes testConn, TestLinkedinAds must additionally
// consult Orchestrator.VerifyAccountOrg and fold a confirmed mismatch into OK: false rather
// than returning it as a transport-level error.
func TestTestLinkedinAds_UpstreamVerification(t *testing.T) {
	newConn := func(t *testing.T) *ConnectionService {
		s := newTestService(t, newFakeRepo())
		if _, err := s.CreateLinkedinAds(context.Background(), &conn.CreateLinkedinAdsPayload{
			ProjectID:   "tlf",
			Config:      &conn.LinkedinAdsConnectionConfig{AccountID: "538170226", OrgID: "208777"},
			Credentials: &conn.LinkedinAdsCredentials{AccessToken: "tok"},
		}); err != nil {
			t.Fatalf("CreateLinkedinAds: %v", err)
		}
		return s
	}

	t.Run("agreement reports OK", func(t *testing.T) {
		s := newConn(t)
		verifier := &orgReferenceVerifierStub{}
		s.SetOrchestrator(&Orchestrator{
			dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: verifier},
		})
		res, err := s.TestLinkedinAds(context.Background(), &conn.TestLinkedinAdsPayload{ProjectID: "tlf"})
		if err != nil {
			t.Fatalf("TestLinkedinAds: %v", err)
		}
		if !res.OK {
			t.Errorf("OK = false, want true; message = %v", res.Message)
		}
		if verifier.gotPlatform != model.ProviderLinkedInAds {
			t.Errorf("verifier saw platform %q, want %q", verifier.gotPlatform, model.ProviderLinkedInAds)
		}
	})

	t.Run("confirmed mismatch reports OK: false, not a transport error", func(t *testing.T) {
		s := newConn(t)
		// Carries domain.ErrOrgVerificationFailed because the dispatcher attaches it to every
		// confirmed verdict; the echo arm is an allowlist keyed on it, so an untagged error
		// would (correctly) get the fixed text instead — see the subtest below.
		mismatch := fmt.Errorf("%w: %w", domain.ErrOrgVerificationFailed,
			errors.New("linkedin ad account 538170226 advertises on behalf of organization 999, not the configured organization 208777"))
		verifier := &orgReferenceVerifierStub{err: mismatch}
		s.SetOrchestrator(&Orchestrator{
			dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: verifier},
		})
		res, err := s.TestLinkedinAds(context.Background(), &conn.TestLinkedinAdsPayload{ProjectID: "tlf"})
		if err != nil {
			t.Fatalf("TestLinkedinAds: %v, want a normal (nil, result) failed test, not a transport error", err)
		}
		if res.OK {
			t.Fatal("OK = true despite a confirmed org mismatch")
		}
		if res.Message == nil || !strings.Contains(*res.Message, "999") || !strings.Contains(*res.Message, "208777") {
			t.Errorf("message = %v, want it to name both org ids", res.Message)
		}
	})

	// The reason the echo is an allowlist rather than a default. Three classes that reach
	// this switch were found carrying a datastore query, a request URL and decrypted bytes,
	// each after a change elsewhere routed a new error here; every one of them inherited the
	// echo by simply not matching an arm above it. An unrecognised error is now silent by
	// construction, so the next such class leaks nothing while it waits for its own arm.
	t.Run("an unclassified verification error is not echoed to the caller", func(t *testing.T) {
		s := newConn(t)
		const canary = "DO-NOT-LEAK-pq: SELECT * FROM connections WHERE token='secret'"
		verifier := &orgReferenceVerifierStub{err: fmt.Errorf("reading connection: %w", errors.New(canary))}
		s.SetOrchestrator(&Orchestrator{
			dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: verifier},
		})
		res, err := s.TestLinkedinAds(context.Background(), &conn.TestLinkedinAdsPayload{ProjectID: "tlf"})
		if err != nil {
			t.Fatalf("TestLinkedinAds: %v, want a normal (nil, result) failed test", err)
		}
		if res.OK {
			t.Fatal("OK = true for an error that proves no cross-check succeeded")
		}
		if res.Message == nil || strings.Contains(*res.Message, "DO-NOT-LEAK") {
			t.Errorf("message = %v, want fixed text with no part of the unclassified error", res.Message)
		}
	})

	t.Run("inconclusive enumeration failure reports OK: true, not a failed test", func(t *testing.T) {
		s := newConn(t)
		inconclusive := fmt.Errorf("%w: %v", domain.ErrOrgVerificationInconclusive, "transport failure contacting linkedin ad-account discovery")
		verifier := &orgReferenceVerifierStub{err: inconclusive}
		s.SetOrchestrator(&Orchestrator{
			dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: verifier},
		})
		res, err := s.TestLinkedinAds(context.Background(), &conn.TestLinkedinAdsPayload{ProjectID: "tlf"})
		if err != nil {
			t.Fatalf("TestLinkedinAds: %v", err)
		}
		if !res.OK {
			t.Errorf("OK = false, want true: an enumeration failure proves nothing about the org pairing")
		}
		if res.Message == nil || !strings.Contains(*res.Message, "inconclusive") {
			t.Errorf("message = %v, want it to say the check was inconclusive", res.Message)
		}
	})

	// Defence in depth. The dispatcher is what strips the platform error's chain before this
	// sentinel is ever attached (TestLinkedInVerifyAccountOrg_InconclusiveCarriesNoRequestURL
	// in internal/dispatch pins that), so a real inconclusive error reaching this layer has
	// nothing to leak. This injects one that does anyway, and pins the separate guarantee that
	// the RESPONSE is a fixed advisory built from no part of the error either way.
	t.Run("inconclusive enumeration failure never echoes the transport error's URL into the response", func(t *testing.T) {
		s := newConn(t)
		const leakyURL = "https://api.linkedin.com/rest/adAccounts?q=search&pageToken=SECRET-CURSOR-9f2a"
		inconclusive := fmt.Errorf("%w: %v", domain.ErrOrgVerificationInconclusive, fmt.Errorf("linkedin GET /adAccounts: Get %q: EOF", leakyURL))
		verifier := &orgReferenceVerifierStub{err: inconclusive}
		s.SetOrchestrator(&Orchestrator{
			dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: verifier},
		})
		res, err := s.TestLinkedinAds(context.Background(), &conn.TestLinkedinAdsPayload{ProjectID: "tlf"})
		if err != nil {
			t.Fatalf("TestLinkedinAds: %v", err)
		}
		if res.Message == nil || strings.Contains(*res.Message, leakyURL) || strings.Contains(*res.Message, "SECRET-CURSOR") {
			t.Errorf("message = %v, leaked the transport error's request URL/query into the HTTP response", res.Message)
		}
	})

	// A datastore outage is not a verdict. Before this arm existed it fell to the default and
	// was reported as OK: false — telling the caller their connection is broken because this
	// service could not read it, with the raw repo error concatenated in to explain why.
	t.Run("a connection load failure is a retryable 503, not a failed test", func(t *testing.T) {
		s := newConn(t)
		const marker = "pq: SELECT connections WHERE project_id = 'tlf' -- DO-NOT-LEAK"
		loadErr := fmt.Errorf("load linkedin-ads connection: %w: %w", domain.ErrConnectionLoadFailed, errors.New(marker))
		s.SetOrchestrator(&Orchestrator{
			dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: &orgReferenceVerifierStub{err: loadErr}},
		})
		res, err := s.TestLinkedinAds(context.Background(), &conn.TestLinkedinAdsPayload{ProjectID: "tlf"})
		if res != nil {
			t.Errorf("result = %+v, want nil: nothing was learned about the connection", res)
		}
		var unavail *conn.ConnServiceUnavailableError
		if !errors.As(err, &unavail) {
			t.Fatalf("error = %v (%T), want a 503 — unlike every other arm here, waiting genuinely helps", err, err)
		}
		if strings.Contains(err.Error(), marker) || strings.Contains(err.Error(), "DO-NOT-LEAK") {
			t.Errorf("error = %v, leaked the repo error text; a repo error can quote the query or the row", err)
		}
	})

	// OK: false is right — the connection really is unusable — but the default arm's message
	// was not: one condition behind this sentinel is found by decoding the DECRYPTED credential
	// blob, and an unmarshal error quotes its input.
	t.Run("an unusable stored connection fails the test without echoing the credential-derived error", func(t *testing.T) {
		s := newConn(t)
		const marker = "invalid character 'x' looking for beginning of value in DECRYPTED-BLOB-BYTES"
		unusable := fmt.Errorf("linkedin credentials: %w: %w: %w",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsUndecodable, errors.New(marker))
		s.SetOrchestrator(&Orchestrator{
			dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: &orgReferenceVerifierStub{err: unusable}},
		})
		res, err := s.TestLinkedinAds(context.Background(), &conn.TestLinkedinAdsPayload{ProjectID: "tlf"})
		if err != nil {
			t.Fatalf("TestLinkedinAds: %v, want an ordinary failed test", err)
		}
		if res.OK {
			t.Error("OK = true for a connection that cannot be used as configured")
		}
		if res.Message == nil || strings.Contains(*res.Message, marker) || strings.Contains(*res.Message, "DECRYPTED-BLOB-BYTES") {
			t.Errorf("message = %v, put credential-derived bytes into the HTTP response", res.Message)
		}
		if res.Message == nil || !strings.Contains(*res.Message, "cannot be used as configured") {
			t.Errorf("message = %v, want it to name the remedy surface", res.Message)
		}
	})

	t.Run("credential decryption failure returns a redacted 500, never the marker text", func(t *testing.T) {
		s := newConn(t)
		const marker = "AES-GCM-CIPHERTEXT-DO-NOT-LEAK-77b3"
		decryptErr := fmt.Errorf("decrypt linkedin credentials: %w: %w", domain.ErrCredentialDecryptionFailed, errors.New(marker))
		verifier := &orgReferenceVerifierStub{err: decryptErr}
		s.SetOrchestrator(&Orchestrator{
			dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: verifier},
		})
		res, err := s.TestLinkedinAds(context.Background(), &conn.TestLinkedinAdsPayload{ProjectID: "tlf"})
		if res != nil {
			t.Errorf("result = %+v, want nil on a service-side decryption failure", res)
		}
		ise, ok := err.(*conn.InternalServerError)
		if !ok {
			t.Fatalf("err = %#v (%T), want *conn.InternalServerError", err, err)
		}
		if strings.Contains(ise.Message, marker) {
			t.Errorf("InternalServerError.Message = %q leaked the decrypt error's marker text %q", ise.Message, marker)
		}
	})

	t.Run("a service defect returns a typed 500, not an ordinary failed test", func(t *testing.T) {
		s := newConn(t)
		// The middle link stands in for a platform client's own typed setup error, which is what
		// the dispatcher wraps in ErrServiceDefect in production. It is deliberately NOT the real
		// linkedin.ErrTokenRequestRejected: this package classifies on domain sentinels alone, so
		// importing the platform package even from a test would make the layering claim in
		// internal-service.md false for package service's own import graph. The arm under test
		// matches ErrServiceDefect and nothing else, so a stand-in exercises the same path.
		defect := fmt.Errorf("%w: %w: %w", domain.ErrServiceDefect, errors.New("linkedin token request rejected"), errors.New("malformed refresh request"))
		verifier := &orgReferenceVerifierStub{err: defect}
		s.SetOrchestrator(&Orchestrator{
			dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderLinkedInAds: verifier},
		})
		res, err := s.TestLinkedinAds(context.Background(), &conn.TestLinkedinAdsPayload{ProjectID: "tlf"})
		if res != nil {
			t.Errorf("result = %+v, want nil on a service defect", res)
		}
		if _, ok := err.(*conn.InternalServerError); !ok {
			t.Fatalf("err = %#v, want *conn.InternalServerError — a defect in this service, not the stored connection, must not be reported as a failed test", err)
		}
	})
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

// TestCreateTwitterAds_WithoutAccountID is the X counterpart, added by LFXV2-3319 alongside the
// removal of Required("account_id") from TwitterAdsConnectionConfig.
//
// X earned this the way the bar demands — BOTH halves, not either: discovery
// (twitter.ListAdAccounts / TwitterDispatcher.ListAccounts) plus a create path that NAMES the
// missing choice, because Dispatch itself resolves through validateTwitterConnection, which tags
// an empty account id with ErrAccountNotSelected. That is the Microsoft shape, not the LinkedIn
// one, and it is why X could be relaxed while LinkedIn cannot.
//
// The assertions are the Google Ads three, plus the one X adds:
//
//  1. Accepted with the key OMITTED, which is the shape the bootstrap flow sends. Goa enforces
//     Required at the transport layer as a presence check on the JSON KEY, so only OMISSION
//     moved from rejection to acceptance. An explicit `"account_id": ""` did NOT "always get
//     through": Pattern("^[A-Za-z0-9]+$") runs independently of Required and rejects the empty
//     string either way, which is why the apivalidation table still lists that case as a
//     rejection. An explicit null is the one other accepted form, decoding identically to an
//     absent key (see TwitterAdsConnectionConfig in design/connection.go).
//  2. status is ACTIVE. validateTwitterConnection refuses a non-active connection, so any other
//     status would make GET .../connection-twitter-ads/accounts unreachable and dead-end the
//     bootstrap at step two.
//  3. account_id round-trips as "".
//  4. funding_instrument_id is supplied because it stays Required, and that is the point rather
//     than fixture noise: it has NO discovery endpoint, so relaxing it would create a row nothing
//     in this API could finish. Credentials-first defers the ACCOUNT choice only.
func TestCreateTwitterAds_WithoutAccountID(t *testing.T) {
	s := newTestService(t, newFakeRepo())
	res, err := s.CreateTwitterAds(context.Background(), &conn.CreateTwitterAdsPayload{
		ProjectID: "cncf",
		// AccountID is nil EXPLICITLY — the absence is the subject of this test.
		Config: &conn.TwitterAdsConnectionConfig{
			Label: strPtr("TLF Main"), AccountID: nil, FundingInstrumentID: "lygyi",
		},
		Credentials: &conn.TwitterAdsCredentials{
			ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats",
		},
	})
	if err != nil {
		t.Fatalf("a credentials-only connection must be creatable: %v", err)
	}
	if res.AccountID != "" {
		t.Errorf("account_id = %q, want the empty string", res.AccountID)
	}
	if res.Status != string(model.StatusActive) {
		t.Errorf("status = %q, want %q — validateTwitterConnection refuses a non-active "+
			"connection, so any other status would make the account unchoosable",
			res.Status, model.StatusActive)
	}
	if !res.HasCredentials {
		t.Error("expected has_credentials = true: the credentials are exactly what WAS supplied")
	}
	if res.FundingInstrumentID == nil || *res.FundingInstrumentID != "lygyi" {
		t.Errorf("funding_instrument_id did not round-trip: %v — it stays Required because no "+
			"discovery endpoint can supply it later", res.FundingInstrumentID)
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

// TestLinkedInRefreshCredentialsRejectSuppliedButEmpty pins the state the all-or-none rule
// silently swallowed: a field SUPPLIED holding no credential.
//
// `present` counted only non-blank values, so `{"refresh_token": ""}` scored 0 and matched
// the `present == 0` arm — the arm that means "all three omitted, a supported bearer-only
// connection". It was accepted and stored. CanRefresh() then read the blank as absent, so
// the connection was bearer-only, while the operator had watched the field be accepted and
// reasonably believed renewal was configured. They meet the same 60-day expiry this feature
// exists to prevent, with nothing anywhere reporting a fault. That is verbatim the failure
// validateLinkedInRefreshCredentials's own docstring claims to prevent.
//
// `present == 0` was carrying two incompatible meanings — "three nil pointers" (legitimate)
// and "supplied but empty" (a defect) — so nil and non-nil-but-blank are now distinguished
// rather than collapsed. The sibling boundary already refused this shape:
// internal/bootstrap/sysacct.go calls a supplied-but-blank string "a supplied key holding no
// credential" and faults on it. Two boundaries write the same trio; agreeing is the point.
func TestLinkedInRefreshCredentialsRejectSuppliedButEmpty(t *testing.T) {
	s := func(v string) *string { return &v }

	cases := []struct {
		name  string
		creds *conn.LinkedinAdsCredentials
	}{
		// The reported shape: ONE field supplied empty, the other two genuinely absent. It
		// scored present == 0 and took the bearer-only arm.
		{"empty refresh token alone", &conn.LinkedinAdsCredentials{
			AccessToken: "at", RefreshToken: s("")}},
		{"whitespace-only refresh token alone", &conn.LinkedinAdsCredentials{
			AccessToken: "at", RefreshToken: s("   ")}},
		{"empty client id alone", &conn.LinkedinAdsCredentials{
			AccessToken: "at", ClientID: s("")}},
		{"empty client secret alone", &conn.LinkedinAdsCredentials{
			AccessToken: "at", ClientSecret: s("")}},
		// All three supplied empty also scored 0. It reads as "the operator configured
		// refresh" far more strongly than one blank field does, and passed just as quietly.
		{"all three supplied empty", &conn.LinkedinAdsCredentials{
			AccessToken: "at", RefreshToken: s(""), ClientID: s(""), ClientSecret: s("")}},
		{"all three whitespace-only", &conn.LinkedinAdsCredentials{
			AccessToken: "at", RefreshToken: s(" "), ClientID: s("\t"), ClientSecret: s("\n")}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLinkedInRefreshCredentials(tc.creds)
			if err == nil {
				t.Fatal("a supplied-but-empty refresh credential was accepted; it stores, reads " +
					"back as absent through CanRefresh(), and leaves the connection bearer-only " +
					"while the operator believes renewal is configured")
			}
			var bad *conn.BadRequestError
			if !errors.As(err, &bad) {
				t.Fatalf("err = %T, want *conn.BadRequestError so the caller gets a 400", err)
			}
			// The message must name the shape, or the operator re-sends the same payload: the
			// all-or-none text ("supplied together, or all omitted") reads as satisfied to
			// someone who supplied all three, just emptily.
			if !strings.Contains(bad.Message, "empty") {
				t.Errorf("message = %q, want it to name the supplied-but-empty shape", bad.Message)
			}
		})
	}

	// The narrowing half. Absence must stay legitimate — all three nil is the supported
	// bearer-only case, and a rule that refused it would break every non-MDP connection.
	for _, ok := range []*conn.LinkedinAdsCredentials{
		{AccessToken: "at"},
		{AccessToken: "at", RefreshToken: s("rt"), ClientID: s("ci"), ClientSecret: s("cs")},
	} {
		if err := validateLinkedInRefreshCredentials(ok); err != nil {
			t.Errorf("a legitimate payload was refused: %v", err)
		}
	}
}

// TestLinkedInRefreshCredentialsRejectPadding pins the API half of the whitespace class.
// Every validator in the system gates on the TRIMMED value — this one, and
// Credentials.CanRefresh() in the platform client — while the store keeps the value
// VERBATIM. So " ci " passed validation, encrypted cleanly, and was later sent raw to
// LinkedIn's token endpoint, which rejects it as invalid_client on every refresh. No
// reconnect repairs that, because the row looks correctly configured.
//
// Padding is REFUSED rather than trimmed: a credential is opaque to this service, so
// silently rewriting one would hide a truncated paste, and no provider issues a secret
// whose surrounding whitespace is significant. This mirrors the bootstrap installer's
// rule (canonicalCredentials, internal/bootstrap/sysacct.go).
func TestLinkedInRefreshCredentialsRejectPadding(t *testing.T) {
	s := func(v string) *string { return &v }

	cases := []struct {
		name  string
		creds *conn.LinkedinAdsCredentials
	}{
		{"padded client id", &conn.LinkedinAdsCredentials{
			AccessToken: "at", RefreshToken: s("rt"), ClientID: s(" ci"), ClientSecret: s("cs")}},
		{"padded client secret", &conn.LinkedinAdsCredentials{
			AccessToken: "at", RefreshToken: s("rt"), ClientID: s("ci"), ClientSecret: s("cs\n")}},
		{"padded refresh token", &conn.LinkedinAdsCredentials{
			AccessToken: "at", RefreshToken: s("\trt"), ClientID: s("ci"), ClientSecret: s("cs")}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The padded trio is COMPLETE, so the all-or-none rule is satisfied and cannot
			// be what rejects it — only the padding check can.
			err := validateLinkedInRefreshCredentials(tc.creds)
			if err == nil {
				t.Fatal("a padded refresh credential was accepted; it stores verbatim, satisfies " +
					"CanRefresh() because that trims, and then fails at LinkedIn as invalid_client forever")
			}
			var bad *conn.BadRequestError
			if !errors.As(err, &bad) {
				t.Errorf("err = %T, want *conn.BadRequestError so the caller gets a 400", err)
			}
		})
	}

	// The narrowing half: an unpadded complete trio is still accepted.
	if err := validateLinkedInRefreshCredentials(&conn.LinkedinAdsCredentials{
		AccessToken: "at", RefreshToken: s("rt"), ClientID: s("ci"), ClientSecret: s("cs"),
	}); err != nil {
		t.Errorf("an unpadded trio must be accepted, got %v", err)
	}
}

// ─── The validator is tested; the CALL SITE is not ───
//
// The tests above call validateLinkedInRefreshCredentials and validateConnectionProjectSlug
// directly, so they pin what the rules DECIDE. Nothing pinned that a handler ASKS. Deleting
// either call from CreateLinkedinAds or SetCredentialLinkedinAds compiled and left this whole
// package green, while the endpoint went back to persisting a partial refresh trio and
// silently degrading it to bearer-only — the exact defect the validator exists to prevent.
//
// A rule reachable only from a test is not enforced. The tests below drive the real handler
// and assert BOTH halves: the caller gets a 400, AND nothing reached the repository. The
// second half is the load-bearing one — a 400 accompanied by a silent partial write is worse
// than no validation at all, because the operator sees a rejection and the row exists anyway.

// TestCreateLinkedinAds_PartialRefreshTrioIsRejectedBeforeAnyWrite binds the handler call at
// CreateLinkedinAds. The payload carries a refresh token with no client_id/client_secret: the
// all-or-none rule refuses it, and no connection row may be created.
func TestCreateLinkedinAds_PartialRefreshTrioIsRejectedBeforeAnyWrite(t *testing.T) {
	repo := newFakeRepo()
	s := newTestService(t, repo)

	_, err := s.CreateLinkedinAds(context.Background(), &conn.CreateLinkedinAdsPayload{
		ProjectID: "tlf",
		Config:    &conn.LinkedinAdsConnectionConfig{AccountID: "538170226", OrgID: "208777"},
		Credentials: &conn.LinkedinAdsCredentials{
			AccessToken: "at", RefreshToken: strPtr("rt"), // client_id and client_secret absent
		},
	})

	var bad *conn.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("a partial refresh trio must reach the caller as *conn.BadRequestError (400), got %T (%v)", err, err)
	}
	// The half that matters: refused BEFORE persisting. The fake would have accepted this
	// create (no existing row, so no ErrConflict), so a non-zero count here means the
	// handler validated nothing and the partial trio was stored and degraded to bearer-only.
	if repo.createCalls != 0 {
		t.Errorf("repo.Create was called %d times; a rejected payload must never be persisted — "+
			"a stored partial trio reads back as bearer-only through CanRefresh() while the "+
			"operator believes renewal is configured", repo.createCalls)
	}
	if len(repo.store) != 0 {
		t.Errorf("store holds %d connection(s) after a rejected create, want 0", len(repo.store))
	}
}

// TestSetCredentialLinkedinAds_PartialRefreshTrioIsRejectedBeforeAnyWrite binds the second
// call site. The row is seeded first ON PURPOSE: fakeRepo.SetCredential returns ErrNotFound
// for a missing row, so against an empty store an unvalidated handler would still return an
// error and a 400-only assertion would pass with the guard deleted. Seeding removes that
// alternative explanation — with the row present the write would SUCCEED, so the only thing
// that can produce a 400 and a zero call count is the validator running first.
func TestSetCredentialLinkedinAds_PartialRefreshTrioIsRejectedBeforeAnyWrite(t *testing.T) {
	repo := newFakeRepo()
	repo.store[repoKey("tlf", model.ProviderLinkedInAds)] = &model.Connection{
		ID: "c1", ProjectID: "tlf", Provider: model.ProviderLinkedInAds, Version: 1,
	}
	s := newTestService(t, repo)

	err := s.SetCredentialLinkedinAds(context.Background(), &conn.SetCredentialLinkedinAdsPayload{
		ProjectID: "tlf",
		Credentials: &conn.LinkedinAdsCredentials{
			AccessToken: "at", ClientID: strPtr("ci"), ClientSecret: strPtr("cs"), // refresh_token absent
		},
	})

	var bad *conn.BadRequestError
	if !errors.As(err, &bad) {
		t.Fatalf("a partial refresh trio must reach the caller as *conn.BadRequestError (400), got %T (%v)", err, err)
	}
	if repo.setCredentialCalls != 0 {
		t.Errorf("repo.SetCredential was called %d times; a rejected credential set must never "+
			"be persisted — it would overwrite a working credential with an unusable partial trio",
			repo.setCredentialCalls)
	}
	// The seeded row must be untouched: version unchanged and no ciphertext written.
	got := repo.store[repoKey("tlf", model.ProviderLinkedInAds)]
	if got.Version != 1 {
		t.Errorf("version = %d, want 1 — the rejected call must not bump the row", got.Version)
	}
	if got.EncryptedCredentials != nil {
		t.Error("the rejected call wrote ciphertext onto the existing row")
	}
}

// TestCreateConnection_UUIDProjectIDRejectedBeforeAnyWrite_AllProviders sweeps the OTHER half
// of the same class. validateConnectionProjectSlug is called from all seven create handlers,
// and deleting the call was survivable in five of them (linkedin, meta, twitter, microsoft,
// hubspot) — only the google and reddit sites were pinned, by the two named tests above.
//
// Every provider gets a case, so adding a provider and forgetting the guard fails here rather
// than shipping an undispatchable UUID-scoped row. Table-driven over the handlers because the
// payload types differ; each closure adapts one signature to a common shape.
func TestCreateConnection_UUIDProjectIDRejectedBeforeAnyWrite_AllProviders(t *testing.T) {
	const uuid = "a09410d0-0ec0-11ea-8e8f-416e2d8da950" // a UUID, not a dispatchable slug

	cases := []struct {
		provider string
		create   func(*ConnectionService, string) error
	}{
		{"google", func(s *ConnectionService, pid string) error {
			_, err := s.CreateGoogleAds(context.Background(), &conn.CreateGoogleAdsPayload{
				ProjectID: pid,
				Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("8666746580")},
				Credentials: &conn.GoogleAdsCredentials{
					RefreshToken: "rt", ClientID: "ci", ClientSecret: "cs", DeveloperToken: "dt"},
			})
			return err
		}},
		{"linkedin", func(s *ConnectionService, pid string) error {
			_, err := s.CreateLinkedinAds(context.Background(), &conn.CreateLinkedinAdsPayload{
				ProjectID:   pid,
				Config:      &conn.LinkedinAdsConnectionConfig{AccountID: "538170226", OrgID: "208777"},
				Credentials: &conn.LinkedinAdsCredentials{AccessToken: "at"},
			})
			return err
		}},
		{"meta", func(s *ConnectionService, pid string) error {
			_, err := s.CreateMetaAds(context.Background(), &conn.CreateMetaAdsPayload{
				ProjectID:   pid,
				Config:      &conn.MetaAdsConnectionConfig{AccountID: strPtr("act_123")},
				Credentials: &conn.MetaAdsCredentials{AccessToken: "at"},
			})
			return err
		}},
		{"reddit", func(s *ConnectionService, pid string) error {
			_, err := s.CreateRedditAds(context.Background(), &conn.CreateRedditAdsPayload{
				ProjectID:   pid,
				Config:      &conn.RedditAdsConnectionConfig{AccountID: "t2_gv9wtbfa"},
				Credentials: &conn.RedditAdsCredentials{ClientID: "c", ClientSecret: "s", RefreshToken: "r"},
			})
			return err
		}},
		{"twitter", func(s *ConnectionService, pid string) error {
			_, err := s.CreateTwitterAds(context.Background(), &conn.CreateTwitterAdsPayload{
				ProjectID:   pid,
				Config:      &conn.TwitterAdsConnectionConfig{AccountID: strPtr("18ce54d4x5t")},
				Credentials: &conn.TwitterAdsCredentials{AccessToken: "at", AccessTokenSecret: "ats", ConsumerKey: "ck", ConsumerSecret: "cse"},
			})
			return err
		}},
		{"microsoft", func(s *ConnectionService, pid string) error {
			_, err := s.CreateMicrosoftAds(context.Background(), &conn.CreateMicrosoftAdsPayload{
				ProjectID:   pid,
				Config:      &conn.MicrosoftAdsConnectionConfig{AccountID: "123456789"},
				Credentials: &conn.MicrosoftAdsCredentials{RefreshToken: "rt", ClientID: "ci", ClientSecret: "cs", DeveloperToken: "dt"},
			})
			return err
		}},
		{"hubspot", func(s *ConnectionService, pid string) error {
			_, err := s.CreateHubspot(context.Background(), &conn.CreateHubspotPayload{
				ProjectID:   pid,
				Config:      &conn.HubspotConnectionConfig{AccountID: "9876543", PortalID: strPtr("12345")},
				Credentials: &conn.HubspotCredentials{PrivateAppToken: "pat"},
			})
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			repo := newFakeRepo()
			s := newTestService(t, repo)

			err := tc.create(s, uuid)

			var bad *conn.BadRequestError
			if !errors.As(err, &bad) {
				t.Fatalf("a UUID project_id must be refused with *conn.BadRequestError (400), got %T (%v)", err, err)
			}
			if repo.createCalls != 0 {
				t.Errorf("repo.Create was called %d times; a UUID-scoped row must never be "+
					"persisted — dispatch looks connections up by exact slug, so the row would "+
					"be invisible to it while appearing configured to the operator", repo.createCalls)
			}
			if len(repo.store) != 0 {
				t.Errorf("store holds %d connection(s) after a rejected create, want 0", len(repo.store))
			}

			// The narrowing half: the same handler must ACCEPT a canonical slug. Without this
			// a guard that refused every project_id would satisfy the assertions above.
			repo2 := newFakeRepo()
			s2 := newTestService(t, repo2)
			if err := tc.create(s2, "cncf"); err != nil {
				t.Fatalf("a canonical slug must be accepted, got %T (%v)", err, err)
			}
			if repo2.createCalls != 1 {
				t.Errorf("repo.Create called %d times for a valid slug, want 1", repo2.createCalls)
			}
		})
	}
}
