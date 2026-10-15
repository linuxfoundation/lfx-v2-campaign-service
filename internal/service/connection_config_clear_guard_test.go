// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"strings"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// clearGuardProvider describes one provider's update for the omission-guard tests (issue #300).
type clearGuardProvider struct {
	provider model.Provider
	// required is the provider's Required config keys with the values the update resends.
	required map[string]string
	// accountID is resent on every update; it is not a provider-config key.
	accountID string
	// guarded is the provider's optional config keys and a valid stored value for each.
	guarded map[string]string
	// update sends a PUT whose optional keys are exactly sent (a missing key is omitted).
	update func(s *ConnectionService, sent map[string]*string, ifMatch string) error
}

func clearGuardProviders() []clearGuardProvider {
	ctx := context.Background()
	return []clearGuardProvider{
		{
			provider: model.ProviderMetaAds,
			required: map[string]string{"page_id": "123456789012345"},
			guarded:  map[string]string{"app_id": "998877"},
			update: func(s *ConnectionService, sent map[string]*string, ifMatch string) error {
				_, err := s.UpdateMetaAds(ctx, &conn.UpdateMetaAdsPayload{
					ProjectID: "cncf", IfMatch: &ifMatch,
					Config: &conn.MetaAdsConnectionConfig{PageID: "123456789012345", AppID: sent["app_id"]},
				})
				return err
			},
		},
		{
			provider:  model.ProviderRedditAds,
			accountID: "t2_gv9wtbfa",
			guarded:   map[string]string{"conversion_pixel_id": "a2_pixel"},
			update: func(s *ConnectionService, sent map[string]*string, ifMatch string) error {
				_, err := s.UpdateRedditAds(ctx, &conn.UpdateRedditAdsPayload{
					ProjectID: "cncf", IfMatch: &ifMatch,
					Config: &conn.RedditAdsConnectionConfig{
						AccountID: "t2_gv9wtbfa", ConversionPixelID: sent["conversion_pixel_id"],
					},
				})
				return err
			},
		},
		{
			provider: model.ProviderTwitterAds,
			required: map[string]string{"funding_instrument_id": "lygyi"},
			guarded:  map[string]string{"as_user_id": "1234567890123456789"},
			update: func(s *ConnectionService, sent map[string]*string, ifMatch string) error {
				_, err := s.UpdateTwitterAds(ctx, &conn.UpdateTwitterAdsPayload{
					ProjectID: "cncf", IfMatch: &ifMatch,
					Config: &conn.TwitterAdsConnectionConfig{
						FundingInstrumentID: "lygyi", AsUserID: sent["as_user_id"],
					},
				})
				return err
			},
		},
		{
			provider:  model.ProviderMicrosoftAds,
			accountID: "1234567",
			guarded:   map[string]string{"customer_id": "9999999"},
			update: func(s *ConnectionService, sent map[string]*string, ifMatch string) error {
				_, err := s.UpdateMicrosoftAds(ctx, &conn.UpdateMicrosoftAdsPayload{
					ProjectID: "cncf", IfMatch: &ifMatch,
					Config: &conn.MicrosoftAdsConnectionConfig{AccountID: "1234567", CustomerID: sent["customer_id"]},
				})
				return err
			},
		},
		{
			provider:  model.ProviderHubSpot,
			accountID: "12345678",
			guarded: map[string]string{
				"portal_id": "4455", "sender_email": "events@example.org",
				"sender_name": "LF Events", "brand_kit": "lf",
			},
			update: func(s *ConnectionService, sent map[string]*string, ifMatch string) error {
				_, err := s.UpdateHubspot(ctx, &conn.UpdateHubspotPayload{
					ProjectID: "cncf", IfMatch: &ifMatch,
					Config: &conn.HubspotConnectionConfig{
						AccountID: "12345678", PortalID: sent["portal_id"], SenderEmail: sent["sender_email"],
						SenderName: sent["sender_name"], BrandKit: sent["brand_kit"],
					},
				})
				return err
			},
		},
	}
}

// seedClearGuardRow stores a row at version 3 whose config holds every required key and, for
// each guarded key, the value in stored (a key missing from stored is stored as "").
func seedClearGuardRow(repo *fakeRepo, p clearGuardProvider, stored map[string]string) {
	cfg := map[string]string{}
	for k, v := range p.required {
		cfg[k] = v
	}
	for k := range p.guarded {
		cfg[k] = stored[k]
	}
	repo.store[repoKey("cncf", p.provider)] = &model.Connection{
		ProjectID: "cncf", Provider: p.provider, Status: model.StatusActive, AccountID: p.accountID,
		Version: 3, EncryptedCredentials: []byte("ciphertext"), ProviderConfig: cfg,
	}
}

// sendAllBut returns a payload map sending every guarded key with its stored value except
// omit, which is left out.
func sendAllBut(p clearGuardProvider, omit string) map[string]*string {
	sent := map[string]*string{}
	for k, v := range p.guarded {
		if k != omit {
			sent[k] = strPtr(v)
		}
	}
	return sent
}

// TestUpdateConnection_OmittedConfigKeyGuard covers issue #300 for every opted-in provider and
// every optional config key, one key at a time, with the other keys sent unchanged.
func TestUpdateConnection_OmittedConfigKeyGuard(t *testing.T) {
	for _, p := range clearGuardProviders() {
		for key := range p.guarded {
			t.Run(string(p.provider)+"/"+key, func(t *testing.T) {
				t.Run("omitted with a stored value is 400 naming the field, no write", func(t *testing.T) {
					repo := newFakeRepo()
					seedClearGuardRow(repo, p, p.guarded)
					s := newTestService(t, repo)

					err := p.update(s, sendAllBut(p, key), "3")
					bad, ok := err.(*conn.BadRequestError)
					if !ok {
						t.Fatalf("expected *conn.BadRequestError, got %T (%v)", err, err)
					}
					if !strings.Contains(bad.Message, "config."+key) || !strings.Contains(bad.Message, `send ""`) {
						t.Errorf("message %q must name config.%s and say how to clear it deliberately", bad.Message, key)
					}
					if repo.updateCalls != 0 {
						t.Errorf("repo.Update called %d time(s); a refused update must not write", repo.updateCalls)
					}
					if got := repo.store[repoKey("cncf", p.provider)].ProviderConfig[key]; got != p.guarded[key] {
						t.Errorf("stored %s = %q, want %q kept", key, got, p.guarded[key])
					}
				})

				t.Run(`explicit "" clears it`, func(t *testing.T) {
					repo := newFakeRepo()
					seedClearGuardRow(repo, p, p.guarded)
					s := newTestService(t, repo)
					sent := sendAllBut(p, key)
					sent[key] = strPtr("")

					if err := p.update(s, sent, "3"); err != nil {
						t.Fatalf("update: %v", err)
					}
					if got := repo.store[repoKey("cncf", p.provider)].ProviderConfig[key]; got != "" {
						t.Errorf("stored %s = %q, want it cleared by the explicit empty string", key, got)
					}
				})

				t.Run("a sent value replaces it", func(t *testing.T) {
					repo := newFakeRepo()
					seedClearGuardRow(repo, p, p.guarded)
					s := newTestService(t, repo)
					sent := sendAllBut(p, key)
					sent[key] = strPtr("777")

					if err := p.update(s, sent, "3"); err != nil {
						t.Fatalf("update: %v", err)
					}
					stored := repo.store[repoKey("cncf", p.provider)].ProviderConfig
					if stored[key] != "777" {
						t.Errorf("stored %s = %q, want the sent value", key, stored[key])
					}
					for k, v := range p.required {
						if stored[k] != v {
							t.Errorf("required %s = %q, want %q kept", k, stored[k], v)
						}
					}
				})

				t.Run("omitted with nothing stored is allowed", func(t *testing.T) {
					repo := newFakeRepo()
					stored := map[string]string{}
					for k, v := range p.guarded {
						if k != key {
							stored[k] = v
						}
					}
					seedClearGuardRow(repo, p, stored)
					s := newTestService(t, repo)

					if err := p.update(s, sendAllBut(p, key), "3"); err != nil {
						t.Fatalf("omitting a key the row has no value for changes nothing and must be allowed: %v", err)
					}
					if repo.updateCalls != 1 {
						t.Errorf("repo.Update called %d time(s), want 1", repo.updateCalls)
					}
				})

				t.Run("stale If-Match is 412 before judging the row, no write", func(t *testing.T) {
					repo := newFakeRepo()
					seedClearGuardRow(repo, p, p.guarded)
					s := newTestService(t, repo)

					err := p.update(s, sendAllBut(p, key), "2")
					if _, ok := err.(*conn.PreconditionFailedError); !ok {
						t.Fatalf("expected *conn.PreconditionFailedError for a stale If-Match, got %T (%v)", err, err)
					}
					if repo.updateCalls != 0 {
						t.Errorf("repo.Update called %d time(s); a stale update must not write", repo.updateCalls)
					}
				})
			})
		}

		t.Run(string(p.provider)+"/a full body reads no row", func(t *testing.T) {
			repo := newFakeRepo()
			seedClearGuardRow(repo, p, p.guarded)
			s := newTestService(t, repo)
			repo.gets = 0

			if err := p.update(s, sendAllBut(p, ""), "3"); err != nil {
				t.Fatalf("update: %v", err)
			}
			if repo.gets != 0 {
				t.Errorf("repo.Get called %d time(s); a body that omits nothing must not pay for the guard's read", repo.gets)
			}
		})
	}
}

// TestUpdateHubspot_OmittedConfigKeysAreAllNamed checks that a body omitting several stored
// keys names every one of them, in a stable order, so one round trip tells the caller all
// it must resend.
func TestUpdateHubspot_OmittedConfigKeysAreAllNamed(t *testing.T) {
	var p clearGuardProvider
	for _, c := range clearGuardProviders() {
		if c.provider == model.ProviderHubSpot {
			p = c
		}
	}
	repo := newFakeRepo()
	seedClearGuardRow(repo, p, p.guarded)
	s := newTestService(t, repo)

	err := p.update(s, map[string]*string{}, "3")
	bad, ok := err.(*conn.BadRequestError)
	if !ok {
		t.Fatalf("expected *conn.BadRequestError, got %T (%v)", err, err)
	}
	want := "config.brand_kit, config.portal_id, config.sender_email, config.sender_name were omitted"
	if !strings.HasPrefix(bad.Message, want) {
		t.Errorf("message = %q, want it to start with %q", bad.Message, want)
	}
}

// TestUpdateLinkedinAds_HasNoOmissionGuard records why LinkedIn is not opted in: org_id is its
// only config key and it is Required, so a full-body update must still read no row.
func TestUpdateLinkedinAds_HasNoOmissionGuard(t *testing.T) {
	repo := newFakeRepo()
	repo.store[repoKey("cncf", model.ProviderLinkedInAds)] = &model.Connection{
		ProjectID: "cncf", Provider: model.ProviderLinkedInAds, AccountID: "538170226", Version: 1,
		ProviderConfig: map[string]string{"org_id": "208777"},
	}
	s := newTestService(t, repo)
	ifMatch := "1"
	repo.gets = 0

	if _, err := s.UpdateLinkedinAds(context.Background(), &conn.UpdateLinkedinAdsPayload{
		ProjectID: "cncf", IfMatch: &ifMatch,
		Config: &conn.LinkedinAdsConnectionConfig{AccountID: "538170226", OrgID: "208778"},
	}); err != nil {
		t.Fatalf("UpdateLinkedinAds: %v", err)
	}
	if repo.gets != 0 {
		t.Errorf("repo.Get called %d time(s) for LinkedIn, which has no optional config key", repo.gets)
	}
}

// TestClearGuardProviders_CoverEveryConfigKey is the drift guard for the table above. For every
// opted-in provider, its Required keys plus its guarded keys must be exactly
// Provider.ConfigKeys(). A new optional key added to providerConfigKeys (and so to
// ProviderConfig) fails here until it is added to the table, and the table's per-key subtests
// then fail until the adapter passes it in optionalConfig. The only providers outside the table
// are LinkedIn (its one key is Required) and Google Ads (not opted in yet).
func TestClearGuardProviders_CoverEveryConfigKey(t *testing.T) {
	covered := map[model.Provider]bool{}
	for _, p := range clearGuardProviders() {
		covered[p.provider] = true
		want := map[string]bool{}
		for _, k := range p.provider.ConfigKeys() {
			want[k] = true
		}
		got := map[string]bool{}
		for k := range p.required {
			got[k] = true
		}
		for k := range p.guarded {
			if got[k] {
				t.Errorf("%s: %s is listed as both required and guarded", p.provider, k)
			}
			got[k] = true
		}
		for k := range want {
			if !got[k] {
				t.Errorf("%s: config key %s is neither required nor guarded in the table; if it is optional, "+
					"add it to the adapter's optionalConfig and to clearGuardProviders", p.provider, k)
			}
		}
		for k := range got {
			if !want[k] {
				t.Errorf("%s: the table lists %s, which is not one of the provider's config keys", p.provider, k)
			}
		}
	}
	excluded := map[model.Provider]bool{model.ProviderLinkedInAds: true, model.ProviderGoogleAds: true}
	for _, p := range model.AllProviders() {
		if covered[p] == excluded[p] {
			t.Errorf("%s: covered=%v, excluded=%v; every provider must be in exactly one of the guard table "+
				"or the named exclusions (LinkedIn, Google Ads)", p, covered[p], excluded[p])
		}
	}
}

// TestUpdateConnection_ConcurrentWriteAfterTheGuardReadIs412 pins the race argument in
// updateConnGuarded: the guard reads a row with nothing stored, so omitting the key passes;
// a concurrent writer then sets the key and bumps the version before the write. The write is
// conditioned on the If-Match version, so it must fail with 412 and leave the new key in place.
func TestUpdateConnection_ConcurrentWriteAfterTheGuardReadIs412(t *testing.T) {
	var p clearGuardProvider
	for _, c := range clearGuardProviders() {
		if c.provider == model.ProviderRedditAds {
			p = c
		}
	}
	repo := newFakeRepo()
	repo.checkVersion = true
	seedClearGuardRow(repo, p, map[string]string{})
	k := repoKey("cncf", p.provider)
	concurrent := &model.Connection{
		ProjectID: "cncf", Provider: p.provider, Status: model.StatusActive, AccountID: p.accountID,
		Version: 4, EncryptedCredentials: []byte("ciphertext"),
		ProviderConfig: map[string]string{"conversion_pixel_id": "a2_set_concurrently"},
	}
	repo.afterGet = func(r *fakeRepo) { r.store[k] = concurrent }
	s := newTestService(t, repo)

	err := p.update(s, map[string]*string{}, "3")
	if _, ok := err.(*conn.PreconditionFailedError); !ok {
		t.Fatalf("expected *conn.PreconditionFailedError once the row moved after the guard's read, got %T (%v)", err, err)
	}
	if repo.gets != 1 {
		t.Errorf("repo.Get called %d time(s), want 1: the guard must have read the row", repo.gets)
	}
	if repo.store[k] != concurrent {
		t.Error("the stored row was replaced; the stale write must not land")
	}
	if got := repo.store[k].ProviderConfig["conversion_pixel_id"]; got != "a2_set_concurrently" {
		t.Errorf("stored pixel = %q, want the concurrently set value kept", got)
	}
}
