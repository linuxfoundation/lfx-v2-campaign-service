// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// TestForceSystem_UpdateRefusesToPersistAnAccountID is the reversibility guard.
//
// While LFX_FORCE_SYSTEM_ADS_ACCOUNT is on, account discovery resolves the LF SYSTEM
// credential, so every id the picker can show names an LF-owned ad account. Persisting one
// of those onto the PROJECT's connection row outlives the flag: turning the flag off restores
// project credentials while the row still points at the LF account id, and nothing upstream
// reconciles the two. The spec's claim that the rollout is "reversible without a code change"
// is only true if that write cannot happen.
func TestForceSystem_UpdateRefusesToPersistAnAccountID(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	repo := newFakeRepo()
	repo.store[repoKey("cncf", model.ProviderGoogleAds)] = &model.Connection{Version: 1}
	s := newTestService(t, repo)
	ifMatch := "1"

	_, err := s.UpdateGoogleAds(context.Background(), &conn.UpdateGoogleAdsPayload{
		ProjectID: "cncf",
		// An id the picker could only have shown because discovery ran on the LF credential.
		Config:  &conn.GoogleAdsConnectionConfig{AccountID: strPtr("8666746580")},
		IfMatch: &ifMatch,
	})
	if err == nil {
		t.Fatal("update stored an ad account id while force-system mode is on; after the flag is " +
			"turned off the project's row points at an LF account it has no credentials for")
	}
	if _, ok := err.(*conn.BadRequestError); !ok {
		// 400 specifically: the update endpoints declare BadRequest but NOT Conflict
		// (design/connection.go), and Goa renders an undeclared error type as a 500 — which
		// would report an operator policy as a server fault.
		t.Fatalf("expected *conn.BadRequestError, got %T (%v)", err, err)
	}
}

// TestForceSystem_CreateRefusesToPersistAnAccountID: a connection can be created WITH an
// account id in a single call, so guarding only the PUT would leave the same LF id
// persistable by a different verb.
func TestForceSystem_CreateRefusesToPersistAnAccountID(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	s := newTestService(t, newFakeRepo())

	_, err := s.CreateGoogleAds(context.Background(), &conn.CreateGoogleAdsPayload{
		ProjectID: "cncf",
		Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("8666746580")},
		Credentials: &conn.GoogleAdsCredentials{
			RefreshToken: "rt", ClientID: "ci", ClientSecret: "cs", DeveloperToken: "dt",
		},
	})
	if err == nil {
		t.Fatal("create stored an ad account id while force-system mode is on")
	}
	if _, ok := err.(*conn.BadRequestError); !ok {
		t.Fatalf("expected *conn.BadRequestError, got %T (%v)", err, err)
	}
}

// TestForceSystem_ClearingAnAccountIDIsStillAllowed: PUT is a full replace, so un-selecting an
// account is expressed as an ABSENT account_id. Refusing that would trap a connection in
// whatever state the flag found it in — the opposite of reversible — so the guard must fire on
// a value being SET, not on the field being present.
func TestForceSystem_ClearingAnAccountIDIsStillAllowed(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	repo := newFakeRepo()
	repo.store[repoKey("cncf", model.ProviderGoogleAds)] = &model.Connection{Version: 1}
	s := newTestService(t, repo)
	ifMatch := "1"

	if _, err := s.UpdateGoogleAds(context.Background(), &conn.UpdateGoogleAdsPayload{
		ProjectID: "cncf",
		Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("")},
		IfMatch:   &ifMatch,
	}); err != nil {
		t.Fatalf("clearing the account id must stay allowed while the flag is on: %v", err)
	}
}

// TestForceSystem_AccountIDPersistsNormallyWhenTheFlagIsOff pins that the guard is scoped to
// forced mode. With the flag off, discovery shows the project's OWN accounts and saving one is
// the ordinary bootstrap — a guard that fired here would break every connection setup.
func TestForceSystem_AccountIDPersistsNormallyWhenTheFlagIsOff(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "")
	repo := newFakeRepo()
	repo.store[repoKey("cncf", model.ProviderGoogleAds)] = &model.Connection{Version: 1}
	s := newTestService(t, repo)
	ifMatch := "1"

	res, err := s.UpdateGoogleAds(context.Background(), &conn.UpdateGoogleAdsPayload{
		ProjectID: "cncf",
		Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("8666746580")},
		IfMatch:   &ifMatch,
	})
	if err != nil {
		t.Fatalf("with the flag off, saving a discovered account id is the ordinary bootstrap: %v", err)
	}
	if res == nil {
		t.Fatal("expected the updated connection back")
	}
}

// TestForceSystem_MissingSystemRowIsNotReportedAsAProjectProblem covers finding 3 at the
// discovery handler.
//
// A missing forced-system row surfaces as a plain domain.ErrNotFound, and every classifier
// checks ErrNotFound FIRST, so the caller was told "no connection configured for this
// project — connect it". That is unfollowable: forced mode ignores the project's own
// connection by construction, so connecting one changes nothing, while the operator who must
// install the LF row is never paged. The classification must therefore be made BEFORE the
// ErrNotFound arm, which is what domain.ErrSystemConnectionMissing exists to carry.
func TestForceSystem_MissingSystemRowIsNotReportedAsAProjectProblem(t *testing.T) {
	s := newTestService(t, newFakeRepo())
	d := accountDiscovery{provider: model.ProviderGoogleAds, displayName: "google ads", operation: "account discovery"}

	// Exactly the shape internal/dispatch/creds.go's resolveForcedSystem produces: the new
	// sentinel wrapped ALONGSIDE the absence it is derived from.
	aerr := errors.Join(domain.ErrSystemConnectionMissing, domain.ErrNotFound)

	err := s.classifyDiscoveryError(context.Background(), "cncf", d, aerr)
	if _, ok := err.(*conn.NotFoundError); ok {
		t.Fatalf("a missing LF SYSTEM connection was reported as 404 'connect your project': %v\n"+
			"the project's own connection is what forced mode ignores, so that advice cannot work", err)
	}
	if _, ok := err.(*conn.InternalServerError); !ok {
		t.Fatalf("expected *conn.InternalServerError (an operator must install the LF row), got %T (%v)", err, err)
	}
}

// TestForceSystem_AGenuineProjectAbsenceIsStill404 is the other half of the ordering, and the
// reason the new arm must key on the sentinel rather than on the flag: an ordinary project
// with no connection is still a 404 telling them to connect one. Without this, a fix for the
// arm above could answer 500 for every unconnected project and no test would notice.
func TestForceSystem_AGenuineProjectAbsenceIsStill404(t *testing.T) {
	s := newTestService(t, newFakeRepo())
	d := accountDiscovery{provider: model.ProviderGoogleAds, displayName: "google ads", operation: "account discovery"}

	err := s.classifyDiscoveryError(context.Background(), "cncf", d, domain.ErrNotFound)
	if _, ok := err.(*conn.NotFoundError); !ok {
		t.Fatalf("a project with no connection of its own must still get 404 'connect it', got %T (%v)", err, err)
	}
}

// TestForceSystem_HubspotCreateAndUpdateStayAllowed is the provider-scoping regression.
//
// model.Connection.AccountID is SHARED across every provider, and HubSpot's account_id is
// Required (design/connection.go) — CreateHubspot and UpdateHubspot copy it into that same
// field. A forced-system guard reading the field without asking which provider owns it
// therefore rejected EVERY HubSpot create and update while the flag is on, blocking CRM
// connection setup outright.
//
// It is not merely over-broad, it is unfollowable: the id being refused is a HubSpot
// list/audience id, not an ad account id. No ad-account discovery ever produced it, turning
// the flag off would not strand it, and the forced dispatch path gates on IsPaidAds()
// specifically so HubSpot/email is never redirected (FR-003). There is nothing for the
// operator to do differently.
//
// Both verbs are asserted because both call sites pass through the same shared helper; a fix
// applied to one would leave the other rejecting.
func TestForceSystem_HubspotCreateAndUpdateStayAllowed(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	repo := newFakeRepo()
	s := newTestService(t, repo)

	if _, err := s.CreateHubspot(context.Background(), &conn.CreateHubspotPayload{
		ProjectID: "cncf",
		// HubSpot's account_id is required and names a LIST, not an ad account.
		Config:      &conn.HubspotConnectionConfig{AccountID: "12345678"},
		Credentials: &conn.HubspotCredentials{PrivateAppToken: "pat-na1-token"},
	}); err != nil {
		t.Fatalf("HubSpot create must stay allowed while force-system mode is on: %v\n"+
			"forcing is scoped to paid ads (FR-003); rejecting here blocks CRM connection setup", err)
	}

	repo.store[repoKey("cncf", model.ProviderHubSpot)] = &model.Connection{Version: 1}
	ifMatch := "1"
	if _, err := s.UpdateHubspot(context.Background(), &conn.UpdateHubspotPayload{
		ProjectID: "cncf",
		Config:    &conn.HubspotConnectionConfig{AccountID: "12345678"},
		IfMatch:   &ifMatch,
	}); err != nil {
		t.Fatalf("HubSpot update must stay allowed while force-system mode is on: %v", err)
	}
}

// TestForceSystem_EveryPaidAdsProviderIsStillGuarded is the other half of the scoping, and
// the reason the guard asks IsPaidAds() rather than naming HubSpot.
//
// Without it, "exempt HubSpot" and "exempt everything" are indistinguishable to the suite:
// the original guard tests all use Google Ads, so a fix that widened the exemption past the
// email channel would leave five paid providers unprotected and no test would notice. This
// walks model.AllProviders() so a provider added later must classify itself into one arm or
// the other.
func TestForceSystem_EveryPaidAdsProviderIsStillGuarded(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	for _, p := range model.AllProviders() {
		// Current value "" — the newly-set direction, which stays refused for every paid-ads
		// provider. The guard now takes the stored selection so it can tell a CHANGED id from
		// a resent one; passing "" here keeps this walking the case it has always covered.
		err := rejectForcedSystemAccountWrite(p, "8666746580", "")
		if p.IsPaidAds() {
			if err == nil {
				t.Errorf("%s is a paid-ads provider: persisting an account id while forced mode "+
					"is on must be refused, or the flag stops being reversible", p)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s is not a paid-ads provider: forcing never redirects it, so its "+
				"account id must persist normally, got %v", p, err)
		}
	}

	// The other half, and the reason the loop above is not sufficient on its own: with the id
	// UNCHANGED, every provider — paid-ads included — must be let through. Without this, a fix
	// that widened the exemption and a fix that narrowed it are indistinguishable to the suite.
	for _, p := range model.AllProviders() {
		if err := rejectForcedSystemAccountWrite(p, "8666746580", "8666746580"); err != nil {
			t.Errorf("%s: resending the id already stored on the row persists nothing, so it "+
				"must be allowed while the flag is on, got %v", p, err)
		}
	}
}

// TestForceSystem_LabelOnlyUpdateSucceedsForEveryRequiredAccountProvider is the regression for
// the fifth variant of this PR's recurring defect: a guard that fixes one path and breaks an
// adjacent one.
//
// account_id is Required on LinkedIn, Reddit, X and Microsoft (design/connection.go's
// Required("account_id") on each config type, generated as a NON-POINTER string). PUT is a full
// replace on every provider in this API, so a caller renaming a connection has no way to omit
// the id — the schema will not decode a body without it. A guard that fired on the id being
// PRESENT therefore returned 400 for every update those four providers can express, which is
// the whole update endpoint, not an edge of it.
//
// Each provider is asserted separately rather than through the shared helper because the bug
// lived in what the ADAPTERS are obliged to send: reading Required("account_id") off the design
// is the step that was skipped, and only a payload-level test re-reads it.
func TestForceSystem_LabelOnlyUpdateSucceedsForEveryRequiredAccountProvider(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	ifMatch := "1"

	// seed installs a row that ALREADY stores the account id, which is the state a project that
	// connected before the cutover is in — and the selection the flag must leave intact for a
	// rollback to have anything to roll back to.
	seed := func(p model.Provider, accountID string) (*ConnectionService, *fakeRepo) {
		repo := newFakeRepo()
		repo.store[repoKey("cncf", p)] = &model.Connection{Version: 1, AccountID: accountID}
		return newTestService(t, repo), repo
	}

	t.Run("linkedin", func(t *testing.T) {
		s, _ := seed(model.ProviderLinkedInAds, "538170226")
		if _, err := s.UpdateLinkedinAds(context.Background(), &conn.UpdateLinkedinAdsPayload{
			ProjectID: "cncf",
			Config: &conn.LinkedinAdsConnectionConfig{
				Label: strPtr("CNCF paid social"), AccountID: "538170226", OrgID: "208777",
			},
			IfMatch: &ifMatch,
		}); err != nil {
			t.Fatalf("renaming a LinkedIn connection must stay possible while the flag is on: %v\n"+
				"account_id is Required on this provider, so the caller cannot omit it", err)
		}
	})

	t.Run("reddit", func(t *testing.T) {
		s, _ := seed(model.ProviderRedditAds, "t2_gv9wtbfa")
		if _, err := s.UpdateRedditAds(context.Background(), &conn.UpdateRedditAdsPayload{
			ProjectID: "cncf",
			Config: &conn.RedditAdsConnectionConfig{
				Label: strPtr("CNCF reddit"), AccountID: "t2_gv9wtbfa",
			},
			IfMatch: &ifMatch,
		}); err != nil {
			t.Fatalf("renaming a Reddit connection must stay possible while the flag is on: %v", err)
		}
	})

	t.Run("twitter", func(t *testing.T) {
		s, _ := seed(model.ProviderTwitterAds, "8r7gb")
		if _, err := s.UpdateTwitterAds(context.Background(), &conn.UpdateTwitterAdsPayload{
			ProjectID: "cncf",
			Config: &conn.TwitterAdsConnectionConfig{
				Label: strPtr("CNCF X"), AccountID: "8r7gb", FundingInstrumentID: "lygyi",
			},
			IfMatch: &ifMatch,
		}); err != nil {
			t.Fatalf("renaming an X connection must stay possible while the flag is on: %v", err)
		}
	})

	t.Run("microsoft", func(t *testing.T) {
		s, _ := seed(model.ProviderMicrosoftAds, "1234567")
		if _, err := s.UpdateMicrosoftAds(context.Background(), &conn.UpdateMicrosoftAdsPayload{
			ProjectID: "cncf",
			Config: &conn.MicrosoftAdsConnectionConfig{
				Label: strPtr("CNCF bing"), AccountID: "1234567", CustomerID: strPtr("7654321"),
			},
			IfMatch: &ifMatch,
		}); err != nil {
			t.Fatalf("renaming a Microsoft connection must stay possible while the flag is on: %v", err)
		}
	})
}

// TestForceSystem_OptionalAccountProvidersNeedNotClearToUpdate covers the other half of the same
// defect, on the two providers whose account_id is OPTIONAL (Google Ads, Meta —
// design/connection.go leaves both out of Required, generated as *string).
//
// These two could technically satisfy a presence check, but only by sending account_id absent —
// and because PUT is a full replace, absent CLEARS the column. So the presence check did not
// merely inconvenience them: the single way to rename a Google or Meta connection while the flag
// was on was to DESTROY its account selection. That is the exact loss the guard exists to
// prevent, reached by obeying the guard.
func TestForceSystem_OptionalAccountProvidersNeedNotClearToUpdate(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	ifMatch := "1"

	t.Run("google-ads", func(t *testing.T) {
		repo := newFakeRepo()
		repo.store[repoKey("cncf", model.ProviderGoogleAds)] = &model.Connection{Version: 1, AccountID: "8666746580"}
		s := newTestService(t, repo)

		res, err := s.UpdateGoogleAds(context.Background(), &conn.UpdateGoogleAdsPayload{
			ProjectID: "cncf",
			Config: &conn.GoogleAdsConnectionConfig{
				Label: strPtr("CNCF search"), AccountID: strPtr("8666746580"),
			},
			IfMatch: &ifMatch,
		})
		if err != nil {
			t.Fatalf("a Google Ads label edit must not require clearing the account id: %v", err)
		}
		// Assert the VALUE that reached the row, not just that no error came back: the point of
		// allowing this write is that the selection SURVIVES it. A fix that let the call through
		// while dropping the id would satisfy an error-only assertion and still lose the thing
		// the rollback needs.
		if got := repo.store[repoKey("cncf", model.ProviderGoogleAds)].AccountID; got != "8666746580" {
			t.Fatalf("account id on the row after a label-only update = %q, want %q — the "+
				"pre-flag selection a rollback depends on was not preserved", got, "8666746580")
		}
		if res == nil {
			t.Fatal("expected the updated connection back")
		}
	})

	t.Run("meta-ads", func(t *testing.T) {
		repo := newFakeRepo()
		repo.store[repoKey("cncf", model.ProviderMetaAds)] = &model.Connection{Version: 1, AccountID: "act_8666746580"}
		s := newTestService(t, repo)

		if _, err := s.UpdateMetaAds(context.Background(), &conn.UpdateMetaAdsPayload{
			ProjectID: "cncf",
			Config: &conn.MetaAdsConnectionConfig{
				Label: strPtr("CNCF meta"), AccountID: strPtr("act_8666746580"), PageID: "123456",
			},
			IfMatch: &ifMatch,
		}); err != nil {
			t.Fatalf("a Meta label edit must not require clearing the account id: %v", err)
		}
		if got := repo.store[repoKey("cncf", model.ProviderMetaAds)].AccountID; got != "act_8666746580" {
			t.Fatalf("account id on the row after a label-only update = %q, want %q", got, "act_8666746580")
		}
	})
}

// TestForceSystem_AChangedAccountIDIsStillRefused is the half that must NOT relax, and it is
// asserted on both routes to a change so that "allow unchanged" cannot quietly become "allow
// anything":
//
//   - NEWLY SET: the row has no selection and the body supplies one. This is the discovery
//     picker's output landing on a project row for the first time.
//   - CHANGED: the row already stores one and the body supplies a DIFFERENT one. This is the
//     case a naive "is it already set?" check would wave through, and it is the worse of the
//     two — it overwrites the project's own pre-flag selection with an LF-owned id.
func TestForceSystem_AChangedAccountIDIsStillRefused(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	ifMatch := "1"

	cases := []struct {
		name    string
		stored  string
		sending string
	}{
		{"newly set on a row with no selection", "", "8666746580"},
		{"changed away from the project's own id", "1111111111", "8666746580"},
		// Case-only and whitespace-only differences are NOT a change: the trim makes the second
		// a no-op, and an exact match is required beyond it. These pin that the comparison is on
		// the stored bytes, so a differently-cased id counts as changed and is refused.
		{"changed by case alone", "abc123", "ABC123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			repo.store[repoKey("cncf", model.ProviderGoogleAds)] = &model.Connection{Version: 1, AccountID: tc.stored}
			s := newTestService(t, repo)

			_, err := s.UpdateGoogleAds(context.Background(), &conn.UpdateGoogleAdsPayload{
				ProjectID: "cncf",
				Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr(tc.sending)},
				IfMatch:   &ifMatch,
			})
			if err == nil {
				t.Fatalf("stored %q, sent %q: this write MOVES the row's account selection while "+
					"forced mode is on, so it must be refused — after the flag is turned off the "+
					"project's row points at an LF account it has no credentials for", tc.stored, tc.sending)
			}
			if _, ok := err.(*conn.BadRequestError); !ok {
				t.Fatalf("expected *conn.BadRequestError, got %T (%v)", err, err)
			}
			// The row must be untouched: a guard that rejects AFTER writing protects nothing.
			if got := repo.store[repoKey("cncf", model.ProviderGoogleAds)].AccountID; got != tc.stored {
				t.Fatalf("the rejected write still reached the row: account id = %q, want %q", got, tc.stored)
			}
		})
	}
}

// TestForceSystem_CreatePathIsUnchanged pins that relaxing UPDATE did not relax CREATE.
//
// The two are genuinely different and the difference is the whole basis of the fix: update
// compares against a stored selection, create has none to compare against, so every non-empty id
// on a create is NEWLY set by definition. If the fix had been implemented by loading the current
// row in a shared helper, create would have found no row, and a sloppy "not found → treat as
// unchanged" would have opened create wide while every update test still passed.
func TestForceSystem_CreatePathIsUnchanged(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")

	t.Run("create with an account id is still refused", func(t *testing.T) {
		s := newTestService(t, newFakeRepo())
		_, err := s.CreateGoogleAds(context.Background(), &conn.CreateGoogleAdsPayload{
			ProjectID: "cncf",
			Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("8666746580")},
			Credentials: &conn.GoogleAdsCredentials{
				RefreshToken: "rt", ClientID: "ci", ClientSecret: "cs", DeveloperToken: "dt",
			},
		})
		if err == nil {
			t.Fatal("create has no prior selection, so a non-empty account id is newly set by " +
				"definition and must stay refused while the flag is on")
		}
		if _, ok := err.(*conn.BadRequestError); !ok {
			t.Fatalf("expected *conn.BadRequestError, got %T (%v)", err, err)
		}
	})

	t.Run("create without an account id still succeeds", func(t *testing.T) {
		s := newTestService(t, newFakeRepo())
		if _, err := s.CreateGoogleAds(context.Background(), &conn.CreateGoogleAdsPayload{
			ProjectID: "cncf",
			Config:    &conn.GoogleAdsConnectionConfig{Label: strPtr("CNCF search")},
			Credentials: &conn.GoogleAdsCredentials{
				RefreshToken: "rt", ClientID: "ci", ClientSecret: "cs", DeveloperToken: "dt",
			},
		}); err != nil {
			t.Fatalf("creating a connection with credentials only (account selection deferred) is "+
				"the documented bootstrap and must stay allowed: %v", err)
		}
	})
}

// TestForceSystem_UpdateReadFailureIsNotReportedAsABadRequest pins the error arm of the new read.
//
// updateConn now loads the current row to answer "did this change?". A read that FAILS must not
// be collapsed into "there is no current selection" — that would make every incoming id look
// newly set and turn a transient database fault into a 400 blaming the caller's body, which is
// the inverse of fail-closed and unfollowable besides (there is nothing wrong with the request).
func TestForceSystem_UpdateReadFailureIsNotReportedAsABadRequest(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	repo := newFakeRepo()
	repo.store[repoKey("cncf", model.ProviderGoogleAds)] = &model.Connection{Version: 1, AccountID: "8666746580"}
	repo.getErr = errors.New("connection reset by peer")
	s := newTestService(t, repo)
	ifMatch := "1"

	_, err := s.UpdateGoogleAds(context.Background(), &conn.UpdateGoogleAdsPayload{
		ProjectID: "cncf",
		Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("8666746580")},
		IfMatch:   &ifMatch,
	})
	if err == nil {
		t.Fatal("an unreadable current row cannot be treated as a successful comparison")
	}
	if _, ok := err.(*conn.BadRequestError); ok {
		t.Fatalf("a database read failure was reported as a 400 about the caller's account id: %v\n"+
			"nothing in the request is wrong; defaulting the current value to \"\" on a read error "+
			"would make every resubmission look newly set", err)
	}
	if _, ok := err.(*conn.InternalServerError); !ok {
		t.Fatalf("expected *conn.InternalServerError, got %T (%v)", err, err)
	}
}

// TestForceSystem_UpdateStillRequiresIfMatchBeforeReadingTheRow pins the ORDER of the new read
// against the precondition check.
//
// The current-row read was placed after parseIfMatch so a caller who omitted If-Match still gets
// 428 rather than a 404/500 produced by a read they should never have paid for. Order is
// invisible to an outcome-only test on the happy path, so it is asserted here directly.
func TestForceSystem_UpdateStillRequiresIfMatchBeforeReadingTheRow(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	repo := newFakeRepo()
	// No row at all, and a read error on top: if the read ran first, this surfaces as something
	// other than 428.
	repo.getErr = errors.New("connection reset by peer")
	s := newTestService(t, repo)

	_, err := s.UpdateGoogleAds(context.Background(), &conn.UpdateGoogleAdsPayload{
		ProjectID: "cncf",
		Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("8666746580")},
		IfMatch:   nil,
	})
	if _, ok := err.(*conn.PreconditionRequiredError); !ok {
		t.Fatalf("expected *conn.PreconditionRequiredError (428) for a missing If-Match, got %T (%v)\n"+
			"the current-row read must not run before the precondition is checked", err, err)
	}
}

// TestForceSystem_FlagOffLeavesEveryUpdatePathAlone is the scoping half for the NEW code, not
// just the old guard. The current-row read is CONDITIONAL — updateConn performs it only when
// forcedSystemGuardApplies, so with the flag off (or for HubSpot) no extra round-trip happens
// at all. This pins that adding the guard did not change what an ordinary deployment does:
// with the flag off, changing an account id — the ordinary bootstrap — still succeeds and
// still lands on the row.
//
// The read count itself is asserted by TestForceSystem_CurrentRowReadIsConditional below;
// this test owns the OUTCOME half.
func TestForceSystem_FlagOffLeavesEveryUpdatePathAlone(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "")
	repo := newFakeRepo()
	repo.store[repoKey("cncf", model.ProviderGoogleAds)] = &model.Connection{Version: 1, AccountID: "1111111111"}
	s := newTestService(t, repo)
	ifMatch := "1"

	if _, err := s.UpdateGoogleAds(context.Background(), &conn.UpdateGoogleAdsPayload{
		ProjectID: "cncf",
		Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("8666746580")},
		IfMatch:   &ifMatch,
	}); err != nil {
		t.Fatalf("with the flag off, switching the selected account is the ordinary bootstrap: %v", err)
	}
	if got := repo.store[repoKey("cncf", model.ProviderGoogleAds)].AccountID; got != "8666746580" {
		t.Fatalf("account id on the row = %q, want the newly chosen %q", got, "8666746580")
	}
}

// TestForceSystem_CurrentRowReadIsConditional pins the guard's SCOPE as a call count, which is
// the only place it is observable.
//
// updateConn reads the current row solely to answer "does this write CHANGE the account id",
// and forcedSystemGuardApplies gates both halves: the inspection and the read that feeds it.
// Making the read unconditional is behaviour-preserving in every RESULT — the same connection
// comes back either way — so no assertion on the returned value can tell the two apart. What
// it costs is a database round-trip on every paid-ads connection update in every deployment
// where the flag is off, which is all of them today.
//
// A stale comment on TestForceSystem_FlagOffLeavesEveryUpdatePathAlone asserted the read was
// unconditional, contradicting the code and sitting exactly where this test was missing.
//
// The three cases below are the guard's two inputs crossed against each other, so a mutation
// that drops EITHER conjunct is killed: dropping `forcedSystemAdsAccount()` is caught by the
// flag-off case, dropping `IsPaidAds()` by the HubSpot case, and the flag-on paid case proves
// the read still happens when it is genuinely needed (otherwise "never read" would pass two
// of three).
func TestForceSystem_CurrentRowReadIsConditional(t *testing.T) {
	t.Run("flag off: a paid-ads update reads no current row", func(t *testing.T) {
		t.Setenv(constants.EnvForceSystemAdsAccount, "")
		repo := newFakeRepo()
		repo.store[repoKey("cncf", model.ProviderGoogleAds)] = &model.Connection{Version: 1, AccountID: "1111111111"}
		s := newTestService(t, repo)
		ifMatch := "1"
		repo.gets = 0

		if _, err := s.UpdateGoogleAds(context.Background(), &conn.UpdateGoogleAdsPayload{
			ProjectID: "cncf",
			Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("8666746580")},
			IfMatch:   &ifMatch,
		}); err != nil {
			t.Fatalf("update with the flag off: %v", err)
		}
		if repo.gets != 0 {
			t.Errorf("repo.Get called %d time(s) with the flag OFF; the current-row read exists only "+
				"to feed the force-system guard, so gating it is what keeps an ordinary deployment "+
				"from paying an extra round-trip on every connection update", repo.gets)
		}
	})

	t.Run("flag on, HubSpot: no current row is read", func(t *testing.T) {
		t.Setenv(constants.EnvForceSystemAdsAccount, "true")
		repo := newFakeRepo()
		repo.store[repoKey("cncf", model.ProviderHubSpot)] = &model.Connection{Version: 1, AccountID: "12345678"}
		s := newTestService(t, repo)
		ifMatch := "1"
		repo.gets = 0

		if _, err := s.UpdateHubspot(context.Background(), &conn.UpdateHubspotPayload{
			ProjectID: "cncf",
			Config:    &conn.HubspotConnectionConfig{AccountID: "87654321"},
			IfMatch:   &ifMatch,
		}); err != nil {
			t.Fatalf("HubSpot update must stay allowed while force-system mode is on: %v", err)
		}
		if repo.gets != 0 {
			t.Errorf("repo.Get called %d time(s) for HubSpot; the guard scopes on IsPaidAds(), so the "+
				"email channel must not pay for a read whose result is discarded", repo.gets)
		}
	})

	t.Run("flag on, paid ads: the current row IS read", func(t *testing.T) {
		t.Setenv(constants.EnvForceSystemAdsAccount, "true")
		repo := newFakeRepo()
		repo.store[repoKey("cncf", model.ProviderGoogleAds)] = &model.Connection{Version: 1, AccountID: "1111111111"}
		s := newTestService(t, repo)
		ifMatch := "1"
		repo.gets = 0

		// Re-sending the STORED id is a no-op write, so it is allowed — and it still has to
		// read the row to know that. This case would be indistinguishable from "never read"
		// if it used a changed id, since the guard could then refuse on the incoming value
		// alone.
		if _, err := s.UpdateGoogleAds(context.Background(), &conn.UpdateGoogleAdsPayload{
			ProjectID: "cncf",
			Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("1111111111")},
			IfMatch:   &ifMatch,
		}); err != nil {
			t.Fatalf("re-sending the stored account id must be allowed (the write moves nothing): %v", err)
		}
		if repo.gets == 0 {
			t.Error("repo.Get was never called with the flag ON for a paid-ads provider; the guard " +
				"cannot tell an unchanged id from a changed one without reading the stored value, " +
				"so it would have to refuse every write or allow every write")
		}
	})
}

// TestForceSystem_NilCurrentRowDoesNotPanic covers the (nil, nil) return that
// domain.ConnectionReader permits and does not forbid.
//
// updateConn dereferences the current row to read its account id, so a reader reporting
// absence that way would panic and take down connection updates for EVERY paid-ads provider
// while the flag is on. This is not hypothetical shape-lawyering: the same branch already
// defends against exactly it in internal/dispatch/creds.go's systemCreated
// (`err != nil || conn == nil`), so the contract is treated as reachable in one new call site
// and, without this, unreachable in the other.
//
// The assertion is that an ERROR comes back rather than a panic, and specifically the 404 the
// absence warrants — not a 400 blaming the caller's body, which is what defaulting a nil row
// to an empty current selection would produce (every incoming id would look newly set).
func TestForceSystem_NilCurrentRowDoesNotPanic(t *testing.T) {
	t.Setenv(constants.EnvForceSystemAdsAccount, "true")
	repo := newFakeRepo()
	repo.store[repoKey("cncf", model.ProviderGoogleAds)] = &model.Connection{Version: 1, AccountID: "1111111111"}
	repo.nilNilGet = true
	s := newTestService(t, repo)
	ifMatch := "1"

	// Deliberately a CHANGED id: it is the input that reaches the guard's comparison, so a
	// nil row is dereferenced rather than short-circuited by an earlier arm.
	_, err := s.UpdateGoogleAds(context.Background(), &conn.UpdateGoogleAdsPayload{
		ProjectID: "cncf",
		Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("8666746580")},
		IfMatch:   &ifMatch,
	})
	if err == nil {
		t.Fatal("update SUCCEEDED against a nil current row; the guard cannot have compared " +
			"against a stored value that was never read")
	}
	if _, isBadRequest := err.(*conn.BadRequestError); isBadRequest {
		t.Errorf("err = %T (%v): a nil row is an ABSENT connection, not a bad request. Treating it "+
			"as an empty current selection makes every incoming id look newly set and blames the "+
			"caller's body for a row that is not there", err, err)
	}
	if _, isNotFound := err.(*conn.NotFoundError); !isNotFound {
		t.Errorf("err = %T (%v), want *conn.NotFoundError: a (nil, nil) read is the same absence "+
			"ErrNotFound expresses, and the update would have 404'd on it anyway", err, err)
	}
}
