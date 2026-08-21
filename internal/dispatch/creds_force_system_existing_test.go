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
	return metaPauseHarnessWith(t, campaign, nil)
}

// metaPauseHarnessWith is metaPauseHarness with a hook to perturb the connection repo before
// the pause runs, so the project-scope FAILURE arms can be exercised against the identical
// stub platform and the identical dispatcher wiring. Passing nil is the ordinary two-row case.
func metaPauseHarnessWith(t *testing.T, campaign *model.Campaign, mutate func(*scopedConnReader)) (token string, paused []string, err error) {
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
	if mutate != nil {
		mutate(repo)
	}

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

// TestPostCutoverCampaignStaysPausableAfterTheProjectDisconnects covers the FAILURE arm of
// resolveExisting, and it is the same never-fix-forward failure as its two siblings above
// arriving by a different route.
//
// Scoping resolution to the recorded creation account fixed the SUCCESS path: when the project
// resolves to a different account, the system row is tried and matched. But project resolution
// can also FAIL outright, and a campaign created on the system account is no less addressable
// for that.
//
// A project that DISCONNECTS its own connection produces exactly this. systemConn refuses the
// ordinary fallback for a disconnected project by design — a disconnect is a statement, not an
// absence — so resolveWithFallback returns ErrNotFound and the valid system row that OWNS this
// campaign is never consulted. Pause and read-metrics then fail on a campaign that keeps
// serving and keeps spending, and the operator's remedy (reconnect the project) is both wrong
// and something they deliberately undid.
//
// The recorded creation account is the invariant on this arm too: the row already says the
// system account owns the live campaign, so the credentials that can address it exist.
func TestPostCutoverCampaignStaysPausableAfterTheProjectDisconnects(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")

	gotToken, paused, err := metaPauseHarnessWith(t, postCutoverMetaCampaign(t, "act_999"),
		func(r *scopedConnReader) {
			// Delete soft-deletes, and Get filters the row out: an explicit disconnect reaches
			// the resolver as the same ErrNotFound as never having connected, with Disconnected
			// reporting true so the ordinary system fallback is (correctly) refused.
			delete(r.rows, "cncf")
			r.tombstoned = map[string]bool{"cncf": true}
		})
	if err != nil {
		t.Fatalf("a system-created campaign became unpausable after the project disconnected its own "+
			"connection: %v\nthe row records the SYSTEM account as its creating account, so the "+
			"credentials that can stop it exist; the campaign keeps spending with no way to stop it", err)
	}
	if gotToken != "system-token" {
		t.Errorf("pause authenticated with %q, want the SYSTEM credentials: the campaign was created "+
			"in the LF system ad account and only that row's token can address its campaign id", gotToken)
	}
	if len(paused) == 0 {
		t.Error("no status update reached the platform; the pause never happened")
	}
}

// TestPostCutoverCampaignStaysPausableWhenTheProjectRowIsUnusable is the second failure arm.
//
// The project row is PRESENT but cannot be resolved — its stored credential blob does not
// decrypt or validate — so resolveWithFallback returns a resolve error rather than ErrNotFound,
// and the fallback never runs at all. As with the disconnect case, that error says nothing
// about the system account the campaign actually records.
//
// It is pinned separately from the disconnect because the two reach the early return through
// DIFFERENT branches of resolveWithFallback (the non-ErrNotFound arm at the top versus the
// ErrNotFound/systemConn arm), and a fix that handled only the absence would leave this one
// stranded with the disconnect test still green.
func TestPostCutoverCampaignStaysPausableWhenTheProjectRowIsUnusable(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")

	gotToken, paused, err := metaPauseHarnessWith(t, postCutoverMetaCampaign(t, "act_999"),
		func(r *scopedConnReader) {
			r.errs = map[string]error{"cncf": errors.New("connection row is corrupt")}
		})
	if err != nil {
		t.Fatalf("a system-created campaign became unpausable because the PROJECT's row is unusable: %v\n"+
			"the campaign lives in the system account and the project's row is irrelevant to stopping it", err)
	}
	if gotToken != "system-token" {
		t.Errorf("pause authenticated with %q, want the SYSTEM credentials", gotToken)
	}
	if len(paused) == 0 {
		t.Error("no status update reached the platform; the pause never happened")
	}
}

// TestProjectResolutionErrorSurvivesForALegacyRow is the guard against the fix above widening
// into "always try the system account on failure".
//
// A row with NO recorded provenance has no system claim to honour. Reaching for the system row
// on its behalf would silently run a legacy campaign's pause against an account whose namespace
// its campaign id does not belong to — a pause that reports success against a campaign it never
// touched. The project's error is the honest answer, and it names the connection an operator
// can actually act on.
//
// Without this, "consult the system row when provenance says so" and "consult it whenever the
// project fails" are indistinguishable to the suite.
func TestProjectResolutionErrorSurvivesForALegacyRow(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")

	repo := &scopedConnReader{rows: map[string]*model.Connection{
		model.SystemProjectID: metaConnFor("act_999"),
	}}
	repo.tombstoned = map[string]bool{"cncf": true}

	// creationAccountID "" is the legacy row: metaCreationAccountID returns "" for it.
	_, err := newCredsSource(repo, identityEncryptor{}).
		resolveExisting(context.Background(), "cncf", model.ProviderMetaAds, "")
	if err == nil {
		t.Fatal("a legacy row with no recorded provenance must surface the PROJECT's resolution error; " +
			"reaching for the system account on its behalf would address its campaign id in a namespace " +
			"it does not belong to")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want the project's own ErrNotFound preserved", err)
	}
}

// TestProjectResolutionErrorSurvivesWhenTheSystemRowDidNotCreateIt is the third arm: the system
// row EXISTS and resolves, but is not the account recorded on the campaign either. Nothing
// reachable can address the campaign, so the project's error must stand rather than be replaced
// by a system resolution that would fail the provenance guard downstream with a message aimed
// at the wrong operator.
func TestProjectResolutionErrorSurvivesWhenTheSystemRowDidNotCreateIt(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")

	repo := &scopedConnReader{rows: map[string]*model.Connection{
		model.SystemProjectID: metaConnFor("act_999"),
	}}
	repo.tombstoned = map[string]bool{"cncf": true}

	// Created under act_777 — neither the disconnected project nor the system row.
	_, err := newCredsSource(repo, identityEncryptor{}).
		resolveExisting(context.Background(), "cncf", model.ProviderMetaAds, "act_777")
	if err == nil {
		t.Fatal("no reachable account created this campaign; the PROJECT's resolution error must stand")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want the project's own ErrNotFound preserved", err)
	}
}

// TestSystemRowFaultIsNotReportedAsTheProjectsError pins WHICH error the failure arm returns
// when BOTH scopes fail, and it is an operator-routing question rather than a cosmetic one.
//
// The added system lookup on the failure arm can itself fail (no system row installed, or an
// unusable one). Returning THAT error would re-attribute a project's own connection problem to
// the LF row: internal/service/brief.go and internal/service/connection.go both branch on
// domain.ErrSystemConnectionOrigin to decide whether the remedy belongs to the project or to
// whoever installed the LF credential. A project that simply disconnected its connection would
// page the platform operator, and the project would never be told to reconnect.
//
// This test is here because a mutation that substituted the system error survived the whole
// suite: every other test on this arm asserts a SUCCESS, so nothing observed which of two
// failures came back.
//
// The rule it pins is NOT "always report the project's error", and reading it that way is what
// produced the defect TestSystemCreatedCampaignSurfacesTheMissingSentinelWhenBothFail covers.
// The choice keys on the recorded creation account (see systemCreated): a SYSTEM-created
// campaign is the system row's to answer for. This fixture installs NO system row at all, so
// provenance cannot be established and the project-owned default is what applies — which is
// the case the paragraph above describes, and the common one.
func TestSystemRowFaultIsNotReportedAsTheProjectsError(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")

	// The project disconnected (so project resolution fails), and NO system row is installed
	// (so the added system lookup fails too, with a system-origin error).
	repo := &scopedConnReader{rows: map[string]*model.Connection{}}
	repo.tombstoned = map[string]bool{"cncf": true}

	_, err := newCredsSource(repo, identityEncryptor{}).
		resolveExisting(context.Background(), "cncf", model.ProviderMetaAds, "act_999")
	if err == nil {
		t.Fatal("both scopes failed; resolveExisting must return an error")
	}
	if errors.Is(err, domain.ErrSystemConnectionOrigin) {
		t.Errorf("err = %v, want the PROJECT's error: tagging this system-origin routes the remedy "+
			"to whoever installs the LF credential, when the project is the one that disconnected", err)
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want the project's own ErrNotFound preserved", err)
	}
}

// TestExistingResolutionNeverReachesForTheSystemRowForHubSpot pins the paid-ads precondition
// on the failure arm's system lookup.
//
// resolveForcedSystem is the LF-system redirect, and resolve() gates it on IsPaidAds()
// precisely so HubSpot/email is never pointed at the LF portal — routing one tenant's contact
// data through another's is not the trade the paid-ads fallback makes (FR-003). The failure
// arm added to resolveExisting performs that same lookup, so it must carry the same gate.
//
// Every caller of resolveExisting today happens to be a paid-ads dispatcher, so a
// call-site-only convention would test green while leaving the function itself willing to
// redirect email. This asserts the property of the FUNCTION: with a HubSpot provider the
// system row is never consulted, even though one is installed and the recorded creation
// account would match it.
func TestExistingResolutionNeverReachesForTheSystemRowForHubSpot(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")

	sysRow := usableConn(`{"privateAppToken":"tok"}`, "act_999")
	sysRow.Provider = model.ProviderHubSpot
	repo := &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: sysRow}}
	// The project disconnected, so project resolution fails and the failure arm is entered.
	repo.tombstoned = map[string]bool{"cncf": true}

	// "act_999" is exactly the system row's account: if the gate were missing, this would
	// resolve successfully against the LF row.
	_, err := newCredsSource(repo, identityEncryptor{}).
		resolveExisting(context.Background(), "cncf", model.ProviderHubSpot, "act_999")
	if err == nil {
		t.Fatal("HubSpot resolution fell back to the LF SYSTEM row: forcing is scoped to paid ads " +
			"(FR-003), and redirecting email would route one tenant's contact data through another's")
	}
	for _, got := range repo.gets {
		if got == model.SystemProjectID {
			t.Errorf("the LF system %s row was consulted for HubSpot; gets = %v", model.ProviderHubSpot, repo.gets)
		}
	}
}

// unusableMetaConnFor is a system row that EXISTS and records its ad account, but whose
// credential blob is absent — so resolveForcedSystem loads and REFUSES it
// (ErrConnectionNotUsable, tagged ErrSystemConnectionNotUsable). It models the operator-owned
// fault this file's two provenance tests turn on: the account id is still recorded on the
// column, which is precisely why provenance survives the row being unusable.
func unusableMetaConnFor(accountID string) *model.Connection {
	c := metaConnFor(accountID)
	c.EncryptedCredentials = nil
	return c
}

// TestSystemCreatedCampaignSurfacesTheSystemFaultOnTheSuccessArm is the success-arm half of the
// defect this file's failure-arm sibling covers, and it is an operator-ROUTING test rather than
// a resolution one.
//
// A campaign created while forcing was on records the SYSTEM account. If that row later breaks
// while the project still has a working connection of its own, project resolution SUCCEEDS to a
// different account and the system lookup fails. Returning the project's resolution then hands
// the caller the provenance guard's account-mismatch 409 — "reconnect the original account" —
// for a row the project does not own, cannot see and cannot repair, while the operator who
// installed the LF credential is never paged and the campaign keeps spending.
//
// The discriminator is the RECORDED creation account, not which resolution succeeded. This
// asserts the system-origin fault is what comes back, so the service layer's
// ErrSystemConnectionNotUsable arm can route the remedy to its actual owner.
func TestSystemCreatedCampaignSurfacesTheSystemFaultOnTheSuccessArm(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")

	// Project resolves fine to act_111; the system row that CREATED this campaign (act_999)
	// exists but is unusable.
	repo := &scopedConnReader{rows: map[string]*model.Connection{
		"cncf":                metaConnFor("act_111"),
		model.SystemProjectID: unusableMetaConnFor("act_999"),
	}}

	res, err := newCredsSource(repo, identityEncryptor{}).
		resolveExisting(context.Background(), "cncf", model.ProviderMetaAds, "act_999")
	if err == nil {
		t.Fatalf("resolveExisting returned the PROJECT's account %q with no error: the campaign was "+
			"created on the SYSTEM account, so the caller's provenance guard now renders a 409 "+
			"telling the project to reconnect a row it does not own, and the operator who must "+
			"repair the LF credential is never paged while the campaign keeps spending", res.accountID)
	}
	if !errors.Is(err, domain.ErrSystemConnectionNotUsable) {
		t.Errorf("err = %v, want domain.ErrSystemConnectionNotUsable: internal/service/brief.go "+
			"routes the remedy on this sentinel, and without it the fault is reported as the "+
			"project's own", err)
	}
	if !errors.Is(err, domain.ErrSystemConnectionOrigin) {
		t.Errorf("err = %v, want domain.ErrSystemConnectionOrigin so the decrypt/log arms attribute "+
			"the failing row to the system scope rather than to the caller", err)
	}
}

// TestProjectCreatedCampaignKeepsTheProjectRemedyWhenItRepoints is the OTHER half of the same
// invariant, and it is what stops the fix above from becoming an unconditional flip.
//
// A campaign created on the PROJECT's own account, whose project has since re-pointed its
// connection to a different account, must still receive the project-owned account-mismatch
// remedy — even when the system row happens to be broken at the same moment. The project
// re-pointed its own connection; that is its repair to make, and paging the platform operator
// for it is the misdirection the existing comments correctly defend against.
//
// Both this and its sibling above must hold simultaneously, which is why the decision keys on
// the recorded creation account rather than on which resolution failed.
func TestProjectCreatedCampaignKeepsTheProjectRemedyWhenItRepoints(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")

	// Created under act_777 (an account the PROJECT used to point at), the project now points
	// at act_111, and the system row (act_999) is unusable — but it did NOT create this row.
	repo := &scopedConnReader{rows: map[string]*model.Connection{
		"cncf":                metaConnFor("act_111"),
		model.SystemProjectID: unusableMetaConnFor("act_999"),
	}}

	res, err := newCredsSource(repo, identityEncryptor{}).
		resolveExisting(context.Background(), "cncf", model.ProviderMetaAds, "act_777")
	if err != nil {
		t.Fatalf("resolveExisting = %v, want the PROJECT's resolution: this campaign was NOT created "+
			"on the system account, so a system-row fault here pages the platform operator for a "+
			"connection the project re-pointed itself", err)
	}
	if res.accountID != "act_111" {
		t.Errorf("resolved account = %q, want act_111: the provenance guard must render its mismatch "+
			"against the account the project CURRENTLY points at, which is the actionable one", res.accountID)
	}
	if res.fromSystem {
		t.Error("resolution reports fromSystem: a project-created campaign must not be attributed to " +
			"the LF row, which would re-route the remedy to an operator")
	}
}

// TestSystemCreatedCampaignSurfacesTheMissingSentinelWhenBothFail is the failure-arm sibling:
// project resolution fails AND the system row is missing, for a campaign the SYSTEM created.
//
// The existing comment on this arm defends returning the project's error so a project that
// merely disconnected does not page the platform operator. That reasoning is right for a
// PROJECT-created campaign and wrong for this one: the project's connection never created this
// campaign and could not address it if it were reconnected, so the honest answer is the one
// naming the row that can.
func TestSystemCreatedCampaignSurfacesTheMissingSentinelWhenBothFail(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")

	// The system row exists (so provenance is READABLE and names act_999 as the creator) but is
	// unusable, and the project disconnected, so project resolution fails too.
	repo := &scopedConnReader{rows: map[string]*model.Connection{
		model.SystemProjectID: unusableMetaConnFor("act_999"),
	}}
	repo.tombstoned = map[string]bool{"cncf": true}

	_, err := newCredsSource(repo, identityEncryptor{}).
		resolveExisting(context.Background(), "cncf", model.ProviderMetaAds, "act_999")
	if err == nil {
		t.Fatal("both scopes failed; resolveExisting must return an error")
	}
	if !errors.Is(err, domain.ErrSystemConnectionNotUsable) {
		t.Errorf("err = %v, want domain.ErrSystemConnectionNotUsable: the SYSTEM account created this "+
			"campaign, so the project's ErrNotFound sends the caller to reconnect a connection that "+
			"could never address it", err)
	}
}

// TestSystemCreatedCampaignPauseReportsTheOperatorFault drives the REAL Meta dispatcher end to
// end and asserts the fault an operator actually receives, not the value resolveExisting
// happens to return.
//
// This is the shape the defect took: the two unit tests above assert a sentinel, but the whole
// point of the sentinel is which remedy the service layer renders from it, and a fix that
// returned the right error to the wrong caller would satisfy them. Driving ToggleStatus proves
// the classification survives the adapter — the layer that turned the previous fix's correct
// error into a project-owned 409.
//
// A campaign created on the LF system account, whose system row has since become unusable,
// must fail with the SYSTEM sentinels (which internal/service/brief.go renders as a 500 and
// "an operator must repair it"), and must NOT fail with ErrCampaignAccountMismatch (which
// renders as a 409 telling the project to reconnect a row it does not own).
func TestSystemCreatedCampaignPauseReportsTheOperatorFault(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")

	_, paused, err := metaPauseHarnessWith(t, postCutoverMetaCampaign(t, "act_999"),
		func(r *scopedConnReader) {
			// The system row that CREATED this campaign is still present (so its account id is
			// readable) but has lost its credential blob — the operator-owned fault.
			r.rows[model.SystemProjectID] = unusableMetaConnFor("act_999")
		})
	if err == nil {
		t.Fatal("pause SUCCEEDED against the project's account: the campaign id is scoped to the " +
			"system ad account, so this either addressed nothing or hit an unrelated campaign")
	}
	if errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Errorf("pause failed with ErrCampaignAccountMismatch (%v), which renders as a 409 "+
			"'reconnect the original account'. The project does not own the LF row, cannot see it "+
			"and cannot repair it, so that remedy is unfollowable while the campaign keeps spending", err)
	}
	if !errors.Is(err, domain.ErrSystemConnectionNotUsable) {
		t.Errorf("err = %v, want domain.ErrSystemConnectionNotUsable so brief.go answers 500 and "+
			"pages whoever installed the LF credential", err)
	}
	if len(paused) != 0 {
		t.Errorf("status updates reached the platform (%v); resolution was refused before any "+
			"upstream call, so nothing should have been sent", paused)
	}
}

// TestProjectRepointedCampaignPauseKeepsTheProjectRemedy is the end-to-end sibling that keeps
// the fix from being a flip: a PROJECT-created campaign whose project re-pointed its connection
// must still receive the project-owned account-mismatch refusal, even while the system row is
// broken at the same moment.
//
// Without this, substituting the system fault unconditionally would pass every assertion in the
// test above — which is exactly the mutation an earlier round of this PR let through.
func TestProjectRepointedCampaignPauseKeepsTheProjectRemedy(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")

	// Created under act_777: the project's OLD account. It now points at act_111, and the
	// system row (act_999) is unusable but did not create this campaign.
	_, paused, err := metaPauseHarnessWith(t, preCutoverMetaCampaign(t, "act_777"),
		func(r *scopedConnReader) {
			r.rows[model.SystemProjectID] = unusableMetaConnFor("act_999")
		})
	if err == nil {
		t.Fatal("pause SUCCEEDED against a re-pointed connection: the provenance guard exists to " +
			"refuse a campaign id addressed against an account it was not created under")
	}
	if errors.Is(err, domain.ErrSystemConnectionNotUsable) || errors.Is(err, domain.ErrSystemConnectionMissing) {
		t.Errorf("pause failed with a SYSTEM sentinel (%v), which pages the platform operator. The "+
			"project re-pointed its own connection; that is the project's repair and the LF row is "+
			"not what broke this campaign", err)
	}
	if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Errorf("err = %v, want domain.ErrCampaignAccountMismatch so brief.go answers 409 naming "+
			"the account the project must reconnect", err)
	}
	if len(paused) != 0 {
		t.Errorf("status updates reached the platform (%v); the guard refuses before any upstream "+
			"call, so nothing should have been sent", paused)
	}
}

// TestUnprovableProvenanceKeepsTheProjectFault pins the "known=false" contract of
// systemCreated, and it exists because two mutations that weakened it survived the suite.
//
// The routing rule is asymmetric on purpose. Claiming a campaign was system-created sends the
// page to the platform operator; claiming otherwise leaves the project with its own actionable
// error. So provenance that CANNOT be established must fall to the project-owned default
// rather than be guessed, because a wrong guess in that direction pages an operator for a
// repair the project has to make — the exact misdirection the existing comments defend.
//
// Two states cannot establish it, and neither is exotic:
//
//   - the system row exists but records NO ad account of its own (an installed-but-unconfigured
//     LF connection). It names no account, so it cannot be shown to be this campaign's creator.
//   - the system-row lookup FAILED. An unanswered question is not a yes; treating a transient
//     repo error as proof of system creation would re-route a project's fault to an operator
//     every time the database hiccuped.
//
// Both must yield the PROJECT's error. Each subtest kills a mutation that survived without it.
func TestUnprovableProvenanceKeepsTheProjectFault(t *testing.T) {
	for name, mutate := range map[string]func(*scopedConnReader){
		"system row records no account of its own": func(r *scopedConnReader) {
			// Present and unusable, but nameless: provenance is unprovable, not proven.
			r.rows[model.SystemProjectID] = unusableMetaConnFor("")
		},
		"the system-row lookup fails": func(r *scopedConnReader) {
			// A row value AND an error: repo implementations are free to return a partially
			// populated value alongside a failure, and the rule is that an errored lookup
			// proves nothing regardless of what came back with it. A fixture that returned a
			// nil row would let a mutation dropping the error check pass on the nil alone.
			delete(r.rows, model.SystemProjectID)
			r.errs = map[string]error{model.SystemProjectID: errors.New("connection reset by peer")}
			r.errRows = map[string]*model.Connection{model.SystemProjectID: metaConnFor("act_999")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(constants.EnvForceSystemAdsAccount, "true")

			// The project resolves to act_111; the campaign records act_999, which nothing
			// reachable can be shown to own.
			repo := &scopedConnReader{rows: map[string]*model.Connection{"cncf": metaConnFor("act_111")}}
			mutate(repo)

			res, err := newCredsSource(repo, identityEncryptor{}).
				resolveExisting(context.Background(), "cncf", model.ProviderMetaAds, "act_999")
			if err != nil {
				t.Fatalf("resolveExisting = %v, want the PROJECT's resolution: provenance could not be "+
					"established, and treating that as proof of system creation pages the platform "+
					"operator for a fault the project may well own", err)
			}
			if res.accountID != "act_111" {
				t.Errorf("resolved account = %q, want act_111 so the provenance guard renders its "+
					"mismatch against the account the project currently points at", res.accountID)
			}
		})
	}
}

// TestAbsentSystemRowSurfacesTheOperatorFault covers the third arm of the same routing
// defect its two siblings above already fixed, arriving through the system row being
// ABSENT rather than present-and-unusable.
//
// systemCreated answers known=false for three distinct states, and the deliberate design
// is that an UNSETTLEABLE question must not fabricate an answer. Two of those states are
// genuinely unsettleable and TestUnprovableProvenanceKeepsTheProjectFault pins them: a
// system row that records no account of its own, and a lookup that FAILED. Neither can be
// shown to own the campaign, so the project keeps its own actionable error.
//
// A row proven ABSENT is not one of them, and collapsing it into the same answer is what
// this test refuses. ErrNotFound is a settled reading of storage, not an unanswered
// question: it says no LF row is installed for this provider. So when a campaign RECORDS
// a creation account that the project's own connection does not match, and the system row
// that could have owned it is proven absent, the fault is the operator's — the LF
// credential row is missing, which is a deployment-wide repair no project can make. The
// prior arms already route the present-but-unusable version of this to
// ErrSystemConnectionMissing/NotUsable; an absent row must not silently downgrade to the
// project's account-mismatch 409, because that pages nobody while the campaign spends.
//
// The assertion is on the sentinel an operator actually receives, not on prose: brief.go
// and orchestrator.go both branch on domain.ErrSystemConnectionMissing to answer 500.
func TestAbsentSystemRowSurfacesTheOperatorFault(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")

	// The project resolves fine to act_111. The campaign records act_999 — an account the
	// project does not own — and NO system row exists at all (no rows entry, no errs entry,
	// so Get answers a clean domain.ErrNotFound).
	repo := &scopedConnReader{rows: map[string]*model.Connection{"cncf": metaConnFor("act_111")}}

	res, err := newCredsSource(repo, identityEncryptor{}).
		resolveExisting(context.Background(), "cncf", model.ProviderMetaAds, "act_999")
	if err == nil {
		t.Fatalf("resolveExisting SUCCEEDED (account %q): the campaign was created under act_999, "+
			"which neither the project's connection nor any installed system row owns, so there are "+
			"no credentials that can address it", res.accountID)
	}
	// The discriminating assertion. ErrCampaignAccountMismatch is the PROJECT-owned remedy
	// ("reconnect the original account") and is what the unfixed code produces by way of
	// returning the project resolution; ErrSystemConnectionMissing is the operator-owned one.
	// Both are reachable from this call, so asserting the absence of one and the presence of
	// the other is what separates the arms — a test asserting only "an error came back" passes
	// on either.
	if errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Errorf("err = %v, which routes to the project's account-mismatch 409. No LF system row is "+
			"installed; that is a deployment-wide repair a project cannot make, so this pages nobody "+
			"while the campaign keeps spending", err)
	}
	if !errors.Is(err, domain.ErrSystemConnectionMissing) {
		t.Errorf("err = %v, want domain.ErrSystemConnectionMissing so internal/service answers the "+
			"operator-owned 500 (internal/service/brief.go's ErrSystemConnectionMissing arm)", err)
	}
}
