// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// ─── Meta, Reddit and X campaign resolution (LFXV2-2665) ───
//
// The three routes share resolvePlatformCampaignRef with the Google and Microsoft ones; what
// each adds is the platform the route fixes and that platform's id rule. Every behaviour the
// Microsoft tests pin is pinned here per platform, table-driven so a fourth route cannot be
// added to one platform and forgotten for another.

type campaignRefRoute struct {
	name     string
	platform model.Provider
	validID  string
	// malformed are ids the platform's rule refuses; each must be a 400 BEFORE any lookup.
	malformed []string
	resolve   func(*ConnectionService, string, string) (*conn.PlatformCampaignResolution, error)
}

var campaignRefRoutes = []campaignRefRoute{
	{
		name: "meta", platform: model.ProviderMetaAds, validID: "120210000000000001",
		malformed: []string{"", "abc", "0", "0120", "-1", "12a", " 120", "120 ", "act_120", strings.Repeat("9", 33)},
		resolve: func(s *ConnectionService, projectID, id string) (*conn.PlatformCampaignResolution, error) {
			return s.ResolveMetaAdsCampaign(context.Background(), &conn.ResolveMetaAdsCampaignPayload{ProjectID: projectID, PlatformCampaignID: id})
		},
	},
	{
		name: "reddit", platform: model.ProviderRedditAds, validID: "t2_camp_123",
		malformed: []string{"", "a-b", "a/b", "a.b", " t2_c", "t2_c ", "a?b", strings.Repeat("a", 65)},
		resolve: func(s *ConnectionService, projectID, id string) (*conn.PlatformCampaignResolution, error) {
			return s.ResolveRedditAdsCampaign(context.Background(), &conn.ResolveRedditAdsCampaignPayload{ProjectID: projectID, PlatformCampaignID: id})
		},
	},
	{
		name: "twitter", platform: model.ProviderTwitterAds, validID: "8wxyz",
		malformed: []string{"", "a_b", "a-b", "a/b", " 8wxyz", "8wxyz ", strings.Repeat("a", 65)},
		resolve: func(s *ConnectionService, projectID, id string) (*conn.PlatformCampaignResolution, error) {
			return s.ResolveTwitterAdsCampaign(context.Background(), &conn.ResolveTwitterAdsCampaignPayload{ProjectID: projectID, PlatformCampaignID: id})
		},
	},
}

func platformCampaign(platform model.Provider, id, briefID, projectID, platformCampaignID string) *model.Campaign {
	c := googleCampaign(id, briefID, projectID, platformCampaignID)
	c.Platform = platform
	return c
}

func TestResolveCampaignRef_FoundReturnsTheBriefAndCampaignPair(t *testing.T) {
	for _, r := range campaignRefRoutes {
		t.Run(r.name, func(t *testing.T) {
			svc := resolverService(t, platformCampaign(r.platform, "c-1", "b-1", "cncf", r.validID))
			res, err := r.resolve(svc, "cncf", r.validID)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if res.MatchCount != 1 || len(res.Matches) != 1 {
				t.Fatalf("matches = %d, match_count = %d, want 1", len(res.Matches), res.MatchCount)
			}
			if res.Matches[0].CampaignID != "c-1" || res.Matches[0].BriefID != "b-1" {
				t.Errorf("match = %+v, want campaign c-1 under brief b-1", res.Matches[0])
			}
			if res.PlatformCampaignID != r.validID {
				t.Errorf("PlatformCampaignID = %q, want the requested id", res.PlatformCampaignID)
			}
		})
	}
}

func TestResolveCampaignRef_UnownedIDIsAnEmptyAnswerNotAnError(t *testing.T) {
	for _, r := range campaignRefRoutes {
		t.Run(r.name, func(t *testing.T) {
			svc := resolverService(t)
			res, err := r.resolve(svc, "cncf", r.validID)
			if err != nil {
				t.Fatalf("an unowned id must be a 200 answer, got %T: %v", err, err)
			}
			if res.MatchCount != 0 || res.Matches == nil || len(res.Matches) != 0 {
				t.Fatalf("matches = %#v, want a non-nil empty slice", res.Matches)
			}
		})
	}
}

func TestResolveCampaignRef_DoesNotResolveAnotherProjectsCampaign(t *testing.T) {
	for _, r := range campaignRefRoutes {
		t.Run(r.name, func(t *testing.T) {
			svc := resolverService(t, platformCampaign(r.platform, "c-other", "b-other", "another-foundation", r.validID))
			res, err := r.resolve(svc, "cncf", r.validID)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if res.MatchCount != 0 {
				t.Fatalf("another project's campaign resolved: %+v", res.Matches)
			}
		})
	}
}

func TestResolveCampaignRef_SkipsSoftDeletedCampaigns(t *testing.T) {
	for _, r := range campaignRefRoutes {
		t.Run(r.name, func(t *testing.T) {
			deleted := platformCampaign(r.platform, "c-1", "b-1", "cncf", r.validID)
			deleted.Status = "deleted"
			res, err := r.resolve(resolverService(t, deleted), "cncf", r.validID)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if res.MatchCount != 0 {
				t.Errorf("a soft-deleted campaign was resolved: %+v", res.Matches)
			}
		})
	}
}

// More than one live row is reachable on every one of these platforms (000020's index is
// Google-only); both must come back so the caller refuses, never the first quietly.
func TestResolveCampaignRef_ReportsTwoLiveRowsRatherThanPickingOne(t *testing.T) {
	for _, r := range campaignRefRoutes {
		t.Run(r.name, func(t *testing.T) {
			svc := resolverService(t,
				platformCampaign(r.platform, "c-1", "b-1", "cncf", r.validID),
				platformCampaign(r.platform, "c-2", "b-2", "cncf", r.validID),
			)
			res, err := r.resolve(svc, "cncf", r.validID)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if res.MatchCount != 2 || len(res.Matches) != 2 {
				t.Fatalf("matches = %d, want both rows so the caller can refuse", len(res.Matches))
			}
		})
	}
}

// THE PLATFORM IS FIXED BY THE ROUTE: the same id stored under every platform resolves, on each
// route, to that route's row alone.
func TestResolveCampaignRef_NewRoutesResolveOnlyTheirOwnPlatform(t *testing.T) {
	const sharedID = "123456789"
	rows := []*model.Campaign{platformCampaign(model.ProviderGoogleAds, "c-google-ads", "b", "cncf", sharedID)}
	for _, r := range campaignRefRoutes {
		rows = append(rows, platformCampaign(r.platform, "c-"+string(r.platform), "b", "cncf", sharedID))
	}
	svc := resolverService(t, rows...)
	for _, r := range campaignRefRoutes {
		t.Run(r.name, func(t *testing.T) {
			res, err := r.resolve(svc, "cncf", sharedID)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if res.MatchCount != 1 || res.Matches[0].CampaignID != "c-"+string(r.platform) {
				t.Errorf("matched %+v, want only c-%s", res.Matches, r.platform)
			}
		})
	}
}

// A malformed id is refused BEFORE any lookup. The repo here fails every lookup with a storage
// fault, so an id that reached the query would come back as a 500; only a refusal made first
// can be the 400 asserted.
func TestResolveCampaignRef_MalformedIDIsRefusedBeforeTheLookup(t *testing.T) {
	for _, r := range campaignRefRoutes {
		t.Run(r.name, func(t *testing.T) {
			svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
			svc.SetOrchestrator(NewOrchestrator(&fakeCampaignRepo{resolveErr: errors.New("the lookup ran")}, newFakeJobRepo(), map[model.Provider]PlatformDispatcher{
				model.ProviderGoogleAds: &keywordActionDispatcher{},
			}))
			for _, id := range r.malformed {
				_, err := r.resolve(svc, "cncf", id)
				bad, ok := err.(*conn.BadRequestError)
				if !ok {
					t.Errorf("id %q: error = %T (%v), want *conn.BadRequestError", id, err, err)
					continue
				}
				if strings.Contains(bad.Message, id) && id != "" {
					t.Errorf("id %q: the 400 echoes the input: %q", id, bad.Message)
				}
			}
			// The valid id does reach the lookup — which is what makes the 400s above prove
			// ordering rather than a route that refuses everything.
			if _, err := r.resolve(svc, "cncf", r.validID); err == nil {
				t.Fatal("expected the forced storage fault for a well-formed id")
			} else if _, ok := err.(*conn.InternalServerError); !ok {
				t.Errorf("well-formed id: error = %T (%v), want *conn.InternalServerError", err, err)
			}
		})
	}
}

func TestResolveCampaignRef_RejectsSystemScope(t *testing.T) {
	for _, r := range campaignRefRoutes {
		t.Run(r.name, func(t *testing.T) {
			svc := resolverService(t, platformCampaign(r.platform, "c-1", "b-1", model.SystemProjectID, r.validID))
			_, err := r.resolve(svc, model.SystemProjectID, r.validID)
			if _, ok := err.(*conn.NotFoundError); !ok {
				t.Errorf("error = %T (%v), want *conn.NotFoundError", err, err)
			}
		})
	}
}

func TestResolveCampaignRef_ColdStartIs503(t *testing.T) {
	for _, r := range campaignRefRoutes {
		t.Run(r.name, func(t *testing.T) {
			_, err := r.resolve(NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{}), "cncf", r.validID)
			if _, ok := err.(*conn.ConnServiceUnavailableError); !ok {
				t.Errorf("error = %T (%v), want *conn.ConnServiceUnavailableError", err, err)
			}
		})
	}
}

// ─── list-reddit-ads-accounts (LFXV2-2665) ───

func redditListService(d *mockAccountListerDispatcher) *ConnectionService {
	svc := NewConnectionService(&mockConnectionRepo{}, &mockEncryptor{})
	svc.SetOrchestrator(&Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderRedditAds: d},
	})
	return svc
}

func TestListRedditAdsAccounts_QueriesTheRedditDispatcherAndPassesIDsThrough(t *testing.T) {
	d := &mockAccountListerDispatcher{accounts: []model.AccessibleAccount{
		{ID: "t2_gv9wtbfa", Label: "Linux Foundation [USD] (The Linux Foundation)"},
		{ID: "t2_8k2mzq1x", Label: "CNCF"},
	}}
	res, err := redditListService(d).ListRedditAdsAccounts(context.Background(), &conn.ListRedditAdsAccountsPayload{ProjectID: "p"})
	if err != nil {
		t.Fatalf("ListRedditAdsAccounts: %v", err)
	}
	if d.gotPlatform != model.ProviderRedditAds {
		t.Errorf("dispatcher asked for %q, want reddit-ads", d.gotPlatform)
	}
	if len(res.Accounts) != 2 || res.Accounts[0].ID != "t2_gv9wtbfa" || res.Accounts[1].ID != "t2_8k2mzq1x" {
		t.Fatalf("accounts = %+v", res.Accounts)
	}
	if res.Accounts[0].Label == nil || *res.Accounts[0].Label != "Linux Foundation [USD] (The Linux Foundation)" {
		t.Errorf("label = %v", res.Accounts[0].Label)
	}
}

func TestListRedditAdsAccounts_EmptyIsNotNil(t *testing.T) {
	res, err := redditListService(&mockAccountListerDispatcher{accounts: []model.AccessibleAccount{}}).
		ListRedditAdsAccounts(context.Background(), &conn.ListRedditAdsAccountsPayload{ProjectID: "p"})
	if err != nil {
		t.Fatalf("ListRedditAdsAccounts: %v", err)
	}
	if res.Accounts == nil || len(res.Accounts) != 0 {
		t.Fatalf("accounts = %#v, want a non-nil empty slice", res.Accounts)
	}
}

// The classification is the shared listAccounts one; what is Reddit's own is the TEXT, and an
// upstream failure must be a 503 (unknown), never an empty 200.
func TestListRedditAdsAccounts_ErrorClassification(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		check func(t *testing.T, err error)
	}{
		{"no connection is 404 naming reddit", domain.ErrNotFound, func(t *testing.T, err error) {
			nf, ok := err.(*conn.NotFoundError)
			if !ok {
				t.Fatalf("got %T: %v", err, err)
			}
			if !strings.Contains(nf.Message, "reddit ads") {
				t.Errorf("message = %q, want it to name reddit ads", nf.Message)
			}
		}},
		{"unusable connection is 400 naming reddit's fields", fmt.Errorf("%w: %w: reddit credentials are incomplete",
			domain.ErrConnectionNotUsable, domain.ErrCredentialsIncomplete), func(t *testing.T, err error) {
			br, ok := err.(*conn.BadRequestError)
			if !ok {
				t.Fatalf("got %T: %v", err, err)
			}
			for _, f := range []string{"client_id", "client_secret", "refresh_token"} {
				if !strings.Contains(br.Message, f) {
					t.Errorf("remedy = %q, want it to name %s", br.Message, f)
				}
			}
			if strings.Contains(br.Message, "are incomplete") {
				t.Errorf("message %q echoes the wrapped cause", br.Message)
			}
			if !strings.Contains(br.Message, "reddit ads") {
				t.Errorf("message = %q, want it to name reddit ads", br.Message)
			}
		}},
		{"upstream failure is 503 without upstream text", errors.New("reddit API GET /me/businesses -> 502 upstream-secret-text"), func(t *testing.T, err error) {
			su, ok := err.(*conn.ConnServiceUnavailableError)
			if !ok {
				t.Fatalf("got %T: %v", err, err)
			}
			if strings.Contains(su.Message, "upstream-secret-text") || strings.Contains(su.Message, "502") {
				t.Errorf("message %q leaks upstream text", su.Message)
			}
			if !strings.Contains(su.Message, "account discovery") {
				t.Errorf("message = %q, want it to name account discovery, not another operation", su.Message)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := redditListService(&mockAccountListerDispatcher{err: tc.err}).
				ListRedditAdsAccounts(context.Background(), &conn.ListRedditAdsAccountsPayload{ProjectID: "p"})
			tc.check(t, err)
		})
	}
}
