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

// postCutoverMetaCampaign models a campaign CREATED AFTER the forced-system cutover: because
// creation is forced, its persisted CampaignResult records the LF SYSTEM ad account. Like the
// pre-cutover case this is a historical fact, and it stays true after the flag is turned off.
func postCutoverMetaCampaign(t *testing.T, systemAccount string) *model.Campaign {
	t.Helper()
	return preCutoverMetaCampaign(t, systemAccount)
}

// metaPauseHarness runs a Meta pause against a stub platform and reports which row's
// credentials authenticated it. Shared by the post-cutover cases so the flag-on and flag-off
// halves differ ONLY in the flag, rather than in two hand-copied servers that could drift.
func metaPauseHarness(t *testing.T, campaign *model.Campaign) (token string, paused []string, err error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The cascade's ad-discovery step must answer with a present `data` field; the client
		// treats an absent one as an unprovable enumeration and fails the toggle closed, which
		// would mask the account-provenance outcome under test.
		token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/ads") {
			if _, werr := w.Write([]byte(`{"data":[{"id":"23851234567890777"}]}`)); werr != nil {
				t.Errorf("write response: %v", werr)
			}
			return
		}
		if perr := r.ParseForm(); perr != nil {
			t.Errorf("parse form: %v", perr)
		}
		paused = append(paused, r.URL.Path+"="+r.FormValue("status"))
		if _, werr := w.Write([]byte(`{"success":true}`)); werr != nil {
			t.Errorf("write response: %v", werr)
		}
	}))
	defer srv.Close()

	repo := &scopedConnReader{rows: map[string]*model.Connection{
		"cncf":                metaConnFor("act_111"),
		model.SystemProjectID: metaConnFor("act_999"),
	}}
	// Distinct credentials per scope, so the assertions can tell WHICH row authenticated the
	// pause rather than only which account id was compared.
	repo.rows["cncf"].EncryptedCredentials = []byte(`{"accessToken":"project-token"}`)
	repo.rows[model.SystemProjectID].EncryptedCredentials = []byte(`{"accessToken":"system-token"}`)

	d := NewMetaDispatcher(repo, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	err = d.ToggleStatus(context.Background(), "cncf", model.ProviderMetaAds, campaign, model.CampaignRunPaused)
	return token, paused, err
}

// TestForcedSystemPausesAPostCutoverCampaign is the MIRROR-IMAGE regression test to
// TestForcedSystemStillPausesAPreCutoverCampaign, and pins the same never-fix-forward failure
// pointed the other way.
//
// Scoping the flag to creation is only half the rule. If existing-campaign operations resolve
// project-then-system-fallback, then a campaign the flag itself just created on the SYSTEM
// account resolves to the PROJECT's account whenever the project has a connection of its own —
// which is the ordinary case. The provenance guard compares the system account recorded in the
// campaign's CampaignResult against the project account resolved now, finds them different, and
// refuses the pause with ErrCampaignAccountMismatch.
//
// That strands exactly the campaigns the cutover created: they keep serving and keep spending,
// and turning the flag back OFF does not help, because the project-then-fallback resolution
// that misroutes them is not flag-conditional. The fix is to resolve the account the campaign
// RECORDS having been created under, which is a historical fact on every row.
func TestForcedSystemPausesAPostCutoverCampaign(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")

	// Created on the SYSTEM account, because creation is forced while the flag is on.
	gotToken, paused, err := metaPauseHarness(t, postCutoverMetaCampaign(t, "act_999"))
	if err != nil {
		if errors.Is(err, domain.ErrCampaignAccountMismatch) {
			t.Fatalf("a POST-cutover campaign cannot be PAUSED while the force-system flag is on: %v\n"+
				"the cutover created this campaign on the system account and the service now offers "+
				"no way to stop it spending", err)
		}
		t.Fatalf("ToggleStatus: %v", err)
	}
	if gotToken != "system-token" {
		t.Errorf("pause authenticated with %q, want the SYSTEM credentials: the campaign was created "+
			"in the LF system ad account and only that row's token can address its campaign id", gotToken)
	}
	if len(paused) == 0 {
		t.Error("no status update reached the platform; the pause never happened")
	}
}

// TestPostCutoverCampaignStaysPausableAfterTheFlagIsOff is the half that outlives the cutover.
//
// A campaign created during the cutover must stay stoppable FOREVER, not only while the flag
// stays on. The flag is a temporary migration switch and will be retired; if resolution for an
// existing campaign were conditional on it, retiring it would strand every campaign the cutover
// created — the failure would appear at the moment an operator believed they were cleaning up.
//
// Resolution therefore keys on the account RECORDED on the row, which does not change when the
// flag does. This test is identical to the flag-on case except that the flag is off, and it
// must produce the identical outcome.
func TestPostCutoverCampaignStaysPausableAfterTheFlagIsOff(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "false")

	gotToken, paused, err := metaPauseHarness(t, postCutoverMetaCampaign(t, "act_999"))
	if err != nil {
		if errors.Is(err, domain.ErrCampaignAccountMismatch) {
			t.Fatalf("a campaign created during the cutover became unpausable once the flag was turned OFF: %v\n"+
				"the creating account is a historical fact on the row and cannot depend on current flag state", err)
		}
		t.Fatalf("ToggleStatus: %v", err)
	}
	if gotToken != "system-token" {
		t.Errorf("pause authenticated with %q, want the SYSTEM credentials: the row records the system "+
			"account as its creating account regardless of the flag's current value", gotToken)
	}
	if len(paused) == 0 {
		t.Error("no status update reached the platform; the pause never happened")
	}
}

// TestLegacyRowWithoutProvenanceResolvesTheProjectAccount pins the "unknown, proceed" arm for
// rows that record NO creation account.
//
// Every *CreationAccountID sibling returns "" for a row written before the explicit account
// field existed (and, on reddit and X, for any row whose blob predates it, since neither has a
// URL fallback to recover it from). Such a row cannot prove which account created it, so
// resolution must fall back to ordinary project-then-system behaviour — the behaviour those
// rows had before the flag existed, and the same permission-to-proceed the provenance guards
// already grant them.
//
// Without this test the empty case is unpinned: making absence mean "not the project's
// account" would silently re-route every legacy row to the system account, where its campaign
// id names nothing — a pause that reports success against a campaign it never touched, and
// metrics that render as a truthful-looking zero. A mutation doing exactly that survived the
// whole dispatch suite before this test existed.
func TestLegacyRowWithoutProvenanceResolvesTheProjectAccount(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")

	// A row with a result blob that records NO account id — metaCreationAccountID returns "".
	legacy := preCutoverMetaCampaign(t, "")
	if got := metaCreationAccountID(legacy); got != "" {
		t.Fatalf("fixture records provenance %q; this test needs a row with NONE", got)
	}

	gotToken, paused, err := metaPauseHarness(t, legacy)
	if err != nil {
		t.Fatalf("a legacy row without recorded provenance must still be pausable: %v", err)
	}
	if gotToken != "project-token" {
		t.Errorf("pause authenticated with %q, want the PROJECT's credentials: an unrecorded creating "+
			"account must fall back to ordinary project-then-system resolution, not be re-routed to "+
			"the system account where the campaign id names nothing", gotToken)
	}
	if len(paused) == 0 {
		t.Error("no status update reached the platform; the pause never happened")
	}
}
