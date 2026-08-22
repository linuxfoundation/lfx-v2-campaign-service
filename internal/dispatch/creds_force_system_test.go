// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// TestNewCredsSourceParsesForceFlag pins the exact-match parse of
// LFX_FORCE_SYSTEM_ADS_ACCOUNT: only the literal "true" turns forcing on, mirroring
// the REDDIT_METRICS_ENABLED flag it copies. Any other value — including "TRUE",
// "1", or absence — leaves the LF-owned account as a fallback, not the primary.
func TestNewCredsSourceParsesForceFlag(t *testing.T) {
	cases := map[string]struct {
		set  bool
		val  string
		want bool
	}{
		"unset":        {set: false, want: false},
		"true":         {set: true, val: "true", want: true},
		"upper TRUE":   {set: true, val: "TRUE", want: false},
		"one":          {set: true, val: "1", want: false},
		"empty string": {set: true, val: "", want: false},
		"spaced":       {set: true, val: " true ", want: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if tc.set {
				t.Setenv(constants.EnvForceSystemAdsAccount, tc.val)
			} else {
				// t.Setenv restores whatever the outer environment had; clear it so the
				// "unset" case is genuinely unset regardless of the caller's shell.
				t.Setenv(constants.EnvForceSystemAdsAccount, "")
			}
			s := newCredsSource(&scopedConnReader{}, identityEncryptor{})
			if s.forceSystemPaidAds != tc.want {
				t.Errorf("forceSystemPaidAds = %v, want %v (value %q)", s.forceSystemPaidAds, tc.want, tc.val)
			}
		})
	}
}

// TestForcedSystemResolvesSystemRowAndNeverReadsProject: with the flag on, a paid-ads
// dispatch authenticates as the LF-owned system account even when the project has a
// perfectly usable connection of its own. The project row must not even be READ — the
// whole point of forcing is that the project's own account is not the account of record.
func TestForcedSystemResolvesSystemRowAndNeverReadsProject(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	repo := &scopedConnReader{rows: map[string]*model.Connection{
		// A usable project row that, if consulted, would win under the normal path. The
		// test is only meaningful because the wrong behaviour is the silent, working one.
		"cncf":                usableConn(`{"project":true}`, "project-account"),
		model.SystemProjectID: usableConn(`{"sys":true}`, "sys-account"),
	}}
	got, err := newCredsSource(repo, identityEncryptor{}).
		resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.accountID != "sys-account" || string(got.plaintext) != `{"sys":true}` {
		t.Errorf("resolved %q/%q, want the SYSTEM account's credentials", got.accountID, got.plaintext)
	}
	if !got.fromSystem {
		t.Error("resolved value must be marked fromSystem so a later validator defect is attributed to the LF row")
	}
	if len(repo.gets) != 1 || repo.gets[0] != model.SystemProjectID {
		t.Errorf("scopes asked = %v, want the system scope ONLY (the project row must never be read)", repo.gets)
	}
}

// TestForcedSystemOverridesADisconnectedProject: forcing is UNCONDITIONAL. A project
// that explicitly disconnected its own account is still dispatched on the system
// account — the forced path must not run the fallback's Disconnected probe (FR-004).
func TestForcedSystemOverridesADisconnectedProject(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	repo := &scopedConnReader{
		rows:       map[string]*model.Connection{model.SystemProjectID: usableConn(`{"sys":true}`, "sys-account")},
		tombstoned: map[string]bool{"cncf": true},
		// A probe that would fail the request if it were ever consulted; forcing must not
		// consult it at all.
		disconnectErr: errors.New("Disconnected must not be called on the forced path"),
	}
	got, err := newCredsSource(repo, identityEncryptor{}).
		resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("resolve: %v — forcing overrides a project's own disconnect by design", err)
	}
	if !got.fromSystem || got.accountID != "sys-account" {
		t.Errorf("resolved = %+v, want the system account regardless of the disconnect", got)
	}
}

// TestForcedSystemFailsClosedWhenNoSystemRowInstalled: the flag being on is a promise
// that dispatch runs on the LF account. If that account is not installed for the
// provider, the request MUST fail closed — never fall through to the project connection
// the flag means to ignore. The failure is not-created (the orchestrator releases the
// claim) and system-origin (the LF row is what needs installing, not a project row).
func TestForcedSystemFailsClosedWhenNoSystemRowInstalled(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	repo := &scopedConnReader{rows: map[string]*model.Connection{
		// A usable PROJECT row that the flag-off path would happily use. Its presence is
		// the trap: a fall-through would resolve it and the forcing promise would be broken
		// silently.
		"cncf": usableConn(`{"project":true}`, "project-account"),
	}}
	_, err := newCredsSource(repo, identityEncryptor{}).
		resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if err == nil {
		t.Fatal("resolve = nil error, want a fail-closed error: the system account is not installed")
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want the underlying absence preserved", err)
	}
	if !errors.Is(err, domain.ErrSystemConnectionOrigin) {
		t.Errorf("err = %v, want it attributed to the SYSTEM row (that is what must be installed)", err)
	}
	var nc interface{ NoUpstreamCreate() bool }
	if !errors.As(err, &nc) || !nc.NoUpstreamCreate() {
		t.Errorf("err = %v, want a not-created error so the orchestrator releases the dispatch claim", err)
	}
	for _, scope := range repo.gets {
		if scope == "cncf" {
			t.Errorf("scopes asked = %v, want the project scope NEVER consulted; forcing must not fall through to it", repo.gets)
		}
	}
}

// TestForcedSystemMarksUnusableRowOrigin: a system row that is present but unusable
// (no credential blob) must arrive carrying ErrSystemConnectionNotUsable under
// ErrSystemConnectionOrigin, exactly like the fallback — so the operator who installed
// the LF credential is paged rather than a project told to fix a row it does not own.
func TestForcedSystemMarksUnusableRowOrigin(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	broken := usableConn(`{"sys":true}`, "sys-account")
	broken.EncryptedCredentials = nil // permanently unusable as it stands
	repo := &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: broken}}

	_, err := newCredsSource(repo, identityEncryptor{}).
		resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if !errors.Is(err, domain.ErrConnectionNotUsable) {
		t.Fatalf("err = %v, want ErrConnectionNotUsable", err)
	}
	if !errors.Is(err, domain.ErrSystemConnectionNotUsable) {
		t.Errorf("err = %v, want it attributed to the SYSTEM connection", err)
	}
	if !errors.Is(err, domain.ErrSystemConnectionOrigin) {
		t.Errorf("err = %v, want the system-origin marker", err)
	}
}

// TestForcedSystemNeverForcesHubSpot: FR-003 — the forced path gates on IsPaidAds(), so
// even with the flag on, ProviderHubSpot (email) is NEVER redirected to the system
// account. Forcing it would write a project's contacts into the LF portal. With the flag
// on and no project HubSpot connection, resolution takes the ordinary path and refuses,
// and the forced path must not have consulted the system scope on HubSpot's behalf.
func TestForcedSystemNeverForcesHubSpot(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	sysRow := usableConn(`{"sys":true}`, "lf-portal")
	sysRow.Provider = model.ProviderHubSpot
	repo := &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: sysRow}}

	got, err := newCredsSource(repo, identityEncryptor{}).
		resolve(context.Background(), "cncf", model.ProviderHubSpot)
	if err == nil {
		t.Fatalf("resolve = %+v, want an error: a project with no HubSpot connection must not be forced onto the LF portal", got)
	}
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want a plain absence — HubSpot is neither forced nor falls back", err)
	}
	if errors.Is(err, domain.ErrSystemConnectionOrigin) {
		t.Errorf("err = %v, must not be system-attributed: HubSpot was never resolved against the LF row", err)
	}
	for _, scope := range repo.gets {
		if scope == model.SystemProjectID {
			t.Errorf("scopes asked = %v, want the system scope never consulted for HubSpot even with the flag on", repo.gets)
		}
	}
}

// TestForcedSystemAppliesToEveryPaidAdsProvider: the single seam flips all six paid-ads
// platforms at once. Every paid-ads provider resolves the system row; no non-paid-ads
// provider does. Mirrors TestSystemFallbackIsGatedByClassificationNotByName but for the
// forced path, so a provider added later is denied forcing by default until classified.
func TestForcedSystemAppliesToEveryPaidAdsProvider(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	for _, p := range model.AllProviders() {
		t.Run(string(p), func(t *testing.T) {
			row := usableConn(`{"sys":true}`, "sys-account")
			row.Provider = p
			repo := &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: row}}

			got, err := newCredsSource(repo, identityEncryptor{}).
				resolve(context.Background(), "cncf", p)

			if p.IsPaidAds() {
				if err != nil {
					t.Fatalf("resolve(%s): %v; every paid-ads provider is forced onto the LF account", p, err)
				}
				if !got.fromSystem {
					t.Errorf("resolve(%s) = %+v, want the system account", p, got)
				}
				if len(repo.gets) != 1 || repo.gets[0] != model.SystemProjectID {
					t.Errorf("resolve(%s) scopes asked = %v, want the system scope only", p, repo.gets)
				}
				return
			}
			// A non-paid-ads provider is not forced: the system scope must not be consulted
			// on its behalf, and it takes the ordinary (refusing) path.
			if err == nil {
				t.Fatalf("resolve(%s) succeeded; only paid-ads providers may be forced onto the LF account", p)
			}
			for _, scope := range repo.gets {
				if scope == model.SystemProjectID {
					t.Errorf("resolve(%s) consulted the system scope; forcing is paid-ads only", p)
				}
			}
		})
	}
}

// TestForcedSystemAtSystemScopeShortCircuits: a request already scoped to the reserved
// system project must NOT re-enter the forced path (that would re-issue the identical
// lookup). It drops to the ordinary path, which asks exactly once. FR-004.
//
// The lookup COUNT alone cannot bind this: both paths issue exactly one
// Get(SystemProjectID, provider), so `len(repo.gets) == 1` holds whether the short-circuit
// is present or removed. `fromSystem` is the only observable difference, and it is
// load-bearing rather than bookkeeping — `resolved.systemScoped` keys on it, so a
// system-scope request that took the forced path would gain a spurious
// ErrSystemConnectionNotUsable and be classified 500/page-an-operator.
func TestForcedSystemAtSystemScopeShortCircuits(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	repo := &scopedConnReader{rows: map[string]*model.Connection{
		model.SystemProjectID: usableConn(`{"sys":true}`, "sys-account"),
	}}
	got, err := newCredsSource(repo, identityEncryptor{}).
		resolve(context.Background(), model.SystemProjectID, model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.accountID != "sys-account" {
		t.Errorf("resolved %q, want the system account", got.accountID)
	}
	if len(repo.gets) != 1 {
		t.Errorf("scopes asked = %v, want exactly one lookup (no forced-path re-entry at the system scope)", repo.gets)
	}
	// The assertion that actually binds the short-circuit — see the note above.
	if got.fromSystem {
		t.Error("fromSystem = true: the system-scope request took the FORCED path instead of " +
			"short-circuiting to the ordinary one. The lookup count is identical either way, so " +
			"this is the only signal that distinguishes them — and systemScoped keys on it, so a " +
			"failure here would be misreported as ErrSystemConnectionNotUsable (500).")
	}
}

// TestForceFlagOffLeavesProjectResolutionIntact: the flag off (the default) must not
// change resolution at all — a project with its own connection dispatches on it, and the
// system scope is never consulted. This is the guard rail that keeps the added branch
// dormant until an operator opts in.
func TestForceFlagOffLeavesProjectResolutionIntact(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "") // explicitly off
	repo := &scopedConnReader{rows: map[string]*model.Connection{
		"cncf":                usableConn(`{"project":true}`, "project-account"),
		model.SystemProjectID: usableConn(`{"sys":true}`, "sys-account"),
	}}
	got, err := newCredsSource(repo, identityEncryptor{}).
		resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.accountID != "project-account" || got.fromSystem {
		t.Errorf("resolved %+v, want the PROJECT's own account with the flag off", got)
	}
	for _, scope := range repo.gets {
		if scope == model.SystemProjectID {
			t.Errorf("scopes asked = %v, want the system scope never consulted when a project owns its connection", repo.gets)
		}
	}
}

// TestForcedSystemTransientLoadFailureKeepsSystemOrigin covers resolveForcedSystem's
// NON-ErrNotFound arm: the system row could not be LOADED at all (the store was
// unreachable), as distinct from the row being absent.
//
// The two arms are one line apart and both fail closed, which is exactly why the
// distinction needs binding rather than reasoning about. They differ in what an operator
// must DO: the absence arm says "install the LF connection for this provider" and is a
// settled, permanent state; this arm says "the connection store failed" and is transient —
// the row may well be installed and readable on the next attempt. Reporting either as the
// other sends whoever is paged to the wrong remedy.
//
// scopedConnReader already exposes the seam this needs (its `errs` map, keyed by project
// id — the same one the FALLBACK path's "system lookup fails" case in creds_test.go uses);
// no new fake is required. Injecting at model.SystemProjectID with the flag ON is what
// makes it reach the forced path rather than the fallback.
//
// The assertions are chosen so a mutation is observable:
//   - dropping the systemOrigin wrapper on this line still yields a non-nil, not-created
//     error, so only the ErrSystemConnectionOrigin check catches it;
//   - re-tagging this arm as the absence arm (returning the notCreated(...ErrNotFound)
//     shape) still carries system origin AND NoUpstreamCreate, so only the NEGATIVE
//     ErrNotFound assertion catches it. That negative is the point of the test: it is the
//     one property that separates this arm from the one directly above it.
func TestForcedSystemTransientLoadFailureKeepsSystemOrigin(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	repo := &scopedConnReader{
		// A usable PROJECT row that must never be reached: a transient system failure must
		// fail closed, not silently degrade to the account the flag exists to bypass.
		rows: map[string]*model.Connection{
			"cncf": usableConn(`{"project":true}`, "project-account"),
		},
		// The store itself failing — NOT domain.ErrNotFound, which is the arm above.
		errs: map[string]error{
			model.SystemProjectID: errors.New("connection refused"),
		},
	}

	_, err := newCredsSource(repo, identityEncryptor{}).
		resolve(context.Background(), "cncf", model.ProviderGoogleAds)
	if err == nil {
		t.Fatal("resolve = nil error, want the transient system-row load failure surfaced")
	}
	if !errors.Is(err, domain.ErrSystemConnectionOrigin) {
		t.Errorf("err = %v, want it attributed to the SYSTEM scope: the load that failed was the "+
			"LF row's, and a caller that reads this as a project-connection problem sends the "+
			"wrong owner to fix a row they do not have", err)
	}
	// The load-bearing negative: this is a TRANSIENT failure, not an absence. If it carried
	// ErrNotFound it would be indistinguishable from "no system connection is installed" —
	// a permanent state with a different remedy — and the consumers that branch on
	// ErrNotFound would report a misconfiguration that may not exist.
	if errors.Is(err, domain.ErrNotFound) {
		t.Errorf("err = %v, want NOT the not-created/absence shape: a store that failed to "+
			"answer is not the same as a system row that is missing, and the two have "+
			"different operator remedies", err)
	}
	var nc interface{ NoUpstreamCreate() bool }
	if !errors.As(err, &nc) || !nc.NoUpstreamCreate() {
		t.Errorf("err = %v, want a not-created error so the orchestrator releases the dispatch "+
			"claim; nothing was created upstream because credentials never resolved", err)
	}
	for _, scope := range repo.gets {
		if scope == "cncf" {
			t.Errorf("scopes asked = %v, want the project scope NEVER consulted: a transient "+
				"system failure must fail closed, not fall through to the project account", repo.gets)
		}
	}
}
