// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// metaConnFor builds a usable Meta connection row bound to a specific ad account, in the
// "act_<digits>" vocabulary verifyMetaAccountMatch normalises both sides into.
func metaConnFor(accountID string) *model.Connection {
	c := usableConn(`{"accessToken":"tok"}`, accountID)
	c.Provider = model.ProviderMetaAds
	return c
}

// preCutoverMetaCampaign models a campaign CREATED BEFORE the forced-system cutover: its
// persisted CampaignResult records the project's own ad account as the creation account,
// which is a historical fact no later flag flip can change.
func preCutoverMetaCampaign(t *testing.T, createdUnderAccount string) *model.Campaign {
	t.Helper()
	blob, err := json.Marshal(meta.CampaignResult{
		CampaignID: "23851234567890123",
		AdSetID:    "23851234567890999",
		AccountID:  createdUnderAccount,
	})
	if err != nil {
		t.Fatalf("marshal campaign result: %v", err)
	}
	return &model.Campaign{
		PlatformCampaignID: "23851234567890123",
		Platform:           model.ProviderMetaAds,
		Status:             campaignStatusCreated,
		Result:             blob,
	}
}

// TestForcedSystemStillPausesAPreCutoverCampaign is the regression test for the failure
// mode that has no fix-forward: a live, SPENDING campaign that nobody can stop.
//
// LFX_FORCE_SYSTEM_ADS_ACCOUNT is flipped on for CREATION, but credsSource.resolve is
// SHARED — it also backs ToggleStatus and ReadMetrics. Under a naive forced resolver every
// operation, on every campaign, resolves the LF system account. A campaign created BEFORE
// the cutover recorded the PROJECT's account in its CampaignResult, so the provenance guard
// (verifyMetaAccountMatch) compares that stored creation account against the system account
// the flag now returns, finds them different, and refuses with ErrCampaignAccountMismatch.
//
// The campaign keeps serving and keeps spending, and the pause path — the one operation that
// must never be unavailable — returns 409 for as long as the flag is on. Inability to stop
// spend cannot be repaired after the fact, which is why this is pinned as a test rather than
// documented as an operational precondition.
//
// The fix scopes forcing to CREATION only: an operation on an EXISTING campaign resolves
// the account it was created under. This test therefore asserts the pause SUCCEEDS and that
// it was authenticated with the PROJECT's credentials, not the system row's.
func TestForcedSystemStillPausesAPreCutoverCampaign(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")

	var gotToken string
	var paused []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// GET /{adSetID}/ads is the cascade's ad-discovery step. It must answer with a
		// present `data` field: the client treats an absent one as an unprovable enumeration
		// and fails the toggle closed, which would mask the account-provenance outcome this
		// test is actually about.
		// The client authenticates with an Authorization: Bearer HEADER and never appends
		// access_token to the query (see doRequest in internal/platform/meta/client.go), so
		// that header is where "which row's credentials were used" is actually observable.
		gotToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/ads") {
			if _, err := w.Write([]byte(`{"data":[{"id":"23851234567890777"}]}`)); err != nil {
				t.Errorf("write response: %v", err)
			}
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		paused = append(paused, r.URL.Path+"="+r.FormValue("status"))
		if _, err := w.Write([]byte(`{"success":true}`)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer srv.Close()

	// The project's own connection — the account the campaign was created under, and the
	// only one that can address its campaign id — alongside the LF system row the flag
	// redirects CREATION to.
	repo := &scopedConnReader{rows: map[string]*model.Connection{
		"cncf":                metaConnFor("act_111"),
		model.SystemProjectID: metaConnFor("act_999"),
	}}
	// Distinct credentials per scope, so the assertion below can tell WHICH row
	// authenticated the pause rather than only which account id was compared.
	repo.rows["cncf"].EncryptedCredentials = []byte(`{"accessToken":"project-token"}`)
	repo.rows[model.SystemProjectID].EncryptedCredentials = []byte(`{"accessToken":"system-token"}`)

	d := NewMetaDispatcher(repo, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	campaign := preCutoverMetaCampaign(t, "act_111")

	err := d.ToggleStatus(context.Background(), "cncf", model.ProviderMetaAds, campaign, model.CampaignRunPaused)
	if err != nil {
		if errors.Is(err, domain.ErrCampaignAccountMismatch) {
			t.Fatalf("a pre-cutover campaign cannot be PAUSED while the force-system flag is on: %v\n"+
				"the campaign keeps spending and the service offers no way to stop it", err)
		}
		t.Fatalf("ToggleStatus: %v", err)
	}
	if gotToken != "project-token" {
		t.Errorf("pause authenticated with %q, want the PROJECT's credentials: the campaign lives in the "+
			"project's ad account and the system token cannot address it", gotToken)
	}
	if len(paused) == 0 {
		t.Error("no status update reached the platform; the pause never happened")
	}
}

// TestForcedSystemStillReadsPreCutoverCampaignMetrics is the read-side half. It is a lesser
// failure than an unstoppable campaign, but the same shared resolver produces it: metrics for
// a pre-cutover campaign are refused (or, worse without the guard, read from the system
// account where the id names nothing and renders as a truthful-looking zero).
func TestForcedSystemStillReadsPreCutoverCampaignMetrics(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")

	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"data":[{"impressions":"120","clicks":"7","spend":"3.50"}]}`)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	defer srv.Close()

	repo := &scopedConnReader{rows: map[string]*model.Connection{
		"cncf":                metaConnFor("act_111"),
		model.SystemProjectID: metaConnFor("act_999"),
	}}
	repo.rows["cncf"].EncryptedCredentials = []byte(`{"accessToken":"project-token"}`)
	repo.rows[model.SystemProjectID].EncryptedCredentials = []byte(`{"accessToken":"system-token"}`)

	d := NewMetaDispatcher(repo, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	campaign := preCutoverMetaCampaign(t, "act_111")

	m, err := d.ReadMetrics(context.Background(), "cncf", model.ProviderMetaAds, campaign, model.MetricsWindowLast7Days)
	if err != nil {
		if errors.Is(err, domain.ErrCampaignAccountMismatch) {
			t.Fatalf("a pre-cutover campaign's metrics cannot be READ while the force-system flag is on: %v", err)
		}
		t.Fatalf("ReadMetrics: %v", err)
	}
	if gotToken != "project-token" {
		t.Errorf("metrics read authenticated with %q, want the PROJECT's credentials", gotToken)
	}
	if m == nil {
		t.Fatal("ReadMetrics returned no metrics")
	}
}

// TestForcedSystemStillForcesCreation pins the OTHER half of the scoping decision: narrowing
// the flag to creation must not weaken creation itself. A dispatch (no existing campaign to
// carry provenance) still authenticates as the LF system account with the flag on.
//
// Without this, a fix that scoped forcing too aggressively — for example by resolving the
// project row whenever a campaign argument is nil — would silently disable the feature while
// every test about pausing kept passing.
func TestForcedSystemStillForcesCreation(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")

	repo := &scopedConnReader{rows: map[string]*model.Connection{
		"cncf":                metaConnFor("act_111"),
		model.SystemProjectID: metaConnFor("act_999"),
	}}
	got, err := newCredsSource(repo, identityEncryptor{}).
		resolve(context.Background(), "cncf", model.ProviderMetaAds)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !got.fromSystem || got.accountID != "act_999" {
		t.Errorf("creation resolved %q (fromSystem=%v), want the SYSTEM account: forcing still governs CREATION",
			got.accountID, got.fromSystem)
	}
}

// TestForcedSystemMissingRowCarriesTheSystemMissingSentinel binds the PRODUCER of the
// system-missing classification.
//
// The service layer's ordering fix is only reachable if this error actually carries
// domain.ErrSystemConnectionMissing — and a test that constructs the sentinel by hand proves
// the arm ordering but not the wiring. Without this, deleting the wrap at the producer leaves
// every classifier's new arm dead and the caller back to "connect your project", with the
// ordering tests still green.
//
// It also pins that ErrNotFound survives ALONGSIDE it: callers that only ask "was anything
// found?" must be unaffected by the added classification.
func TestForcedSystemMissingRowCarriesTheSystemMissingSentinel(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	// No system row installed, and a perfectly good project row that forcing must ignore.
	repo := &scopedConnReader{rows: map[string]*model.Connection{"cncf": metaConnFor("act_111")}}

	_, err := newCredsSource(repo, identityEncryptor{}).
		resolve(context.Background(), "cncf", model.ProviderMetaAds)
	if err == nil {
		t.Fatal("resolve = nil error, want a fail-closed error: the system account is not installed")
	}
	if !errors.Is(err, domain.ErrSystemConnectionMissing) {
		t.Errorf("err = %v, want domain.ErrSystemConnectionMissing so classifiers can order it "+
			"ABOVE ErrNotFound; without it the caller is told to connect a project connection "+
			"that forced mode ignores", err)
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound preserved alongside: the absence is real and "+
			"callers that only ask 'was anything found?' must be unaffected", err)
	}
}
