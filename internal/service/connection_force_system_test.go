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
		err := rejectForcedSystemAccountWrite(p, "8666746580")
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
}
