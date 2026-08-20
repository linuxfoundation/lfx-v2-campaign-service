// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// systemScopedConnReader answers the SYSTEM scope only: a project asking for its own
// connection gets ErrNotFound, which is the one condition credsSource.resolve treats as a
// genuine absence and therefore the only one that reaches the LF fallback.
type systemScopedConnReader struct{ sys *model.Connection }

func (r systemScopedConnReader) Get(_ context.Context, projectID string, _ model.Provider) (*model.Connection, error) {
	if projectID == model.SystemProjectID {
		return r.sys, nil
	}
	return nil, domain.ErrNotFound
}

// Disconnected reports false: these projects never connected an account, as opposed to having
// deliberately disconnected one — a tombstoned row must NOT reach the fallback.
func (r systemScopedConnReader) Disconnected(context.Context, string, model.Provider) (bool, error) {
	return false, nil
}

// TestReddit_DispatchStampsProvenanceEndToEnd drives a REAL Dispatch and asserts the campaign
// it hands back carries the provenance. It is the test that pins the `defer` itself.
//
// It exists because the sibling tests could not. TestStampProvenance_* calls stampProvenance
// directly against a resolved it constructed, and the orchestrator's test uses a stub
// dispatcher that hardcodes the field — so between them, NO test ran a real Dispatch and then
// looked at RanOnSystemAccount. Neutralising the
// `defer func() { res.stampProvenance(camp) }()` left the whole suite green, so the change's
// central safety claim — "a deferred call on the named return covers every exit, including
// ones not written yet" — was the one thing nothing verified. (Neutralise it rather than
// DELETE the line: deletion leaves `res` unused wherever no other statement reads it, and the
// compiler then rejects the mutation instead of the test catching it. See the note on
// TestAllDispatchers_StampProvenanceOnEveryCampaignReturn for the form to use — it applies to
// this dispatcher too.)
//
// Both exits that claim is really about are covered, on the adapter's own httptest harness:
//
//   - the clean success, and
//   - the AMBIGUOUS create, which returns a campaign ALONGSIDE an error. That is the arm
//     per-return stamping would most plausibly miss, and it is the row an operator
//     reconciling system-account spend most needs, because it may correspond to a real paid
//     campaign upstream.
//
// Reddit is the subject because it is also the awkward case: its credential is resolved behind
// resolveRedditClientWithCreds, so this exercises the split helper rather than the plain path.
func TestReddit_DispatchStampsProvenanceEndToEnd(t *testing.T) {
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()

	cfg := json.RawMessage(`{"redditConfig":{"budgetUsd":50,"startDate":"2099-08-01","endDate":"2099-08-31","objective":"traffic","subreddits":["kubernetes"]}}`)

	newDispatcher := func(apiURL string, fromSystem bool) *RedditDispatcher {
		var repo connReader = fakeConnReader{conn: activeRedditConn(goodRedditCreds)}
		if fromSystem {
			repo = systemScopedConnReader{sys: activeRedditConn(goodRedditCreds)}
		}
		return NewRedditDispatcher(repo, identityEncryptor{},
			reddit.WithBaseURL(apiURL+"/api/v3"), reddit.WithTokenURL(tok.URL),
			reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
		)
	}

	successAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "cmp_123"}})
		case strings.Contains(r.URL.Path, "ad_groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		}
	}))
	defer successAPI.Close()

	// A 5xx on the campaign POST makes the client return a name-only partial plus an error.
	ambiguousAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ambiguousAPI.Close()

	for _, tc := range []struct {
		name       string
		api        *httptest.Server
		fromSystem bool
		wantErr    bool
	}{
		{"success on the LF system account", successAPI, true, false},
		{"success on the project's own account", successAPI, false, false},
		{"ambiguous create on the LF system account", ambiguousAPI, true, true},
		{"ambiguous create on the project's own account", ambiguousAPI, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newDispatcher(tc.api.URL, tc.fromSystem)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, cfg)
			if tc.wantErr && err == nil {
				t.Fatal("expected an error from the ambiguous create")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if camp == nil {
				t.Fatal("expected a campaign on this path; without one the provenance has " +
					"nothing to ride on and this case proves nothing")
			}
			if camp.RanOnSystemAccount == nil {
				t.Fatalf("Dispatch returned a campaign with NO provenance — the deferred "+
					"stampProvenance did not run on this exit, so the row persists as "+
					"\"unknown\" and drops out of system-account spend reporting "+
					"(fromSystem=%v)", tc.fromSystem)
			}
			if *camp.RanOnSystemAccount != tc.fromSystem {
				t.Errorf("RanOnSystemAccount = %v, want %v — the campaign records the wrong "+
					"paying account", *camp.RanOnSystemAccount, tc.fromSystem)
			}
		})
	}
}

// campaignDispatcher is the one method this table needs. Declared locally rather than reaching
// for service.PlatformDispatcher because internal/service imports nothing from here and this
// package must not import it back — the two are siblings, which is the same boundary the
// provenance value itself crosses on *model.Campaign.
type campaignDispatcher interface {
	Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error)
}

// TestAllDispatchers_StampProvenanceOnEveryCampaignReturn is the coverage the `defer` argument
// requires: one case per dispatcher, so deleting the defer from ANY of the seven fails a test
// that names it.
//
// The reviewers found this gap by mutation: with the defer neutralised, the entire suite
// stayed green. That made the change's own safety claim the one unverified thing in it —
// seventeen per-return edits were rejected in favour of seven defers precisely so a future
// exit could not be missed, and nothing held the seven in place.
//
// Choose the mutation carefully when re-checking this. DELETING the defer line leaves `res`
// unused in any Dispatch that reads it nowhere else, and the compiler rejects that — a build
// break is not evidence that any test covers anything, so a sweep done that way silently
// proves nothing for those dispatchers. (At the time of writing that is reddit and hubspot,
// but do not rely on the list: it changes with any edit to a Dispatch body, which is exactly
// why the mutation should not depend on it.) The honest analogue of a regression keeps `res`
// used and the defer present, dropping only the effect:
//
//	defer func() { _ = res }()
//
// Under that form all seven compile and all seven fail HERE, each naming its dispatcher.
//
// (The closure is not decoration: a bare `defer res.stampProvenance(camp)` would evaluate
// `camp` at defer time — always nil — and stamp nothing.)
//
// Every case drives a real create against that adapter's own fake API, reusing the harnesses
// its existing tests use. Eleven of the thirteen reach a clean success; the two linkedin rows
// reach the UNCONFIRMED arm instead, and that is declared per row in `wantErr` and ASSERTED —
// so a fake API that drifts into returning partials everywhere fails loudly rather than
// quietly weakening what this table covers. An earlier revision of this table pointed all seven at one
// 5xx server on the theory that the error exit is the interesting one; that produced a table
// which SKIPPED nine of its fourteen cases, because most adapters return (nil, err) rather than
// a partial when the create fails outright. A skipping table reports PASS while asserting
// nothing, which is precisely the failure mode this test was added to close, so the skip is
// gone: a case that reaches no campaign is now a FAILURE naming the dispatcher.
//
// The error-carrying exits — where a campaign comes back ALONGSIDE an error, which is the arm
// per-return stamping would most plausibly miss — are covered for reddit by
// TestReddit_DispatchStampsProvenanceEndToEnd above, on both the system and project scopes.
func TestAllDispatchers_StampProvenanceOnEveryCampaignReturn(t *testing.T) {
	// A shared OAuth token endpoint for the adapters whose clients mint one.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tokenSrv.Close()

	// jsonServer answers every request from one handler, for the adapters whose success path
	// this test can satisfy with a single canned body.
	jsonServer := func(h http.HandlerFunc) *httptest.Server {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return srv
	}

	metaSrv := jsonServer(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "23851"})
	})
	linkedinSrv := jsonServer(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// The by-name search REQUIRES a metadata block: without it the client refuses to
			// treat the search as complete, rather than risk creating a duplicate.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"elements": []any{},
				"metadata": map[string]any{"total": 0},
				"paging":   map[string]any{"start": 0, "count": 0, "total": 0},
			})
			return
		}
		w.Header().Set("x-linkedin-id", "777")
		w.Header().Set("x-restli-id", "777")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "777"})
	})
	redditSrv := jsonServer(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "cmp_123"}})
		case strings.Contains(r.URL.Path, "ad_groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		}
	})
	twitterSrv := jsonServer(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "tw_1"}})
	})

	// google-ads and microsoft have their own multi-endpoint harnesses; hubspot needs an
	// audience reader alongside its server.
	gaOpts, _ := googleAdsServers(t,
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaignBudgets/111"}]}`)
		},
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"results":[{"resourceName":"customers/1234567890/campaigns/222"}]}`)
		},
	)
	msOpts, _ := microsoftServers(t)
	hsSrv, _ := hubspotServer(t)

	cases := []struct {
		name     string
		platform model.Provider
		conn     func() *model.Connection
		build    func(connReader) campaignDispatcher
		config   json.RawMessage
		// fallbackEligible reports whether this provider may reach the LF system account at
		// all. Only PAID ADS providers may: credsSource.systemConn refuses the fallback for
		// anything else, because HubSpot is a CRM portal rather than an ad account and
		// falling back would write one project's contacts into the LF's own portal. So the
		// hubspot row runs the project-owned scope ONLY — asserting a system-served hubspot
		// campaign would be asserting a behaviour the service deliberately does not have.
		fallbackEligible bool
		// wantErrContains records WHICH EXIT this row actually drives, and is asserted rather
		// than ignored. Empty means "expect a clean create"; non-empty is a substring the
		// error must contain.
		//
		// It is a substring rather than a bool on purpose. A bool would pin only that SOME
		// error occurred, and the linkedin rows would keep passing if the fake drifted into a
		// different failure — a 500 on the campaign-group POST aborts before the dark-post
		// step, still returns a partial, and would satisfy `wantErr: true` while the comment
		// explaining WHY this row errors quietly went stale.
		//
		// The linkedin rows reach the UNCONFIRMED arm because linkedinSrv answers the
		// dark-post step with a plain id where the client requires a urn:li:share/ugcPost URN.
		// That is kept deliberately: the error-carrying exit is the one per-return stamping
		// would most plausibly miss, so it is worth covering.
		wantErrContains string
	}{
		{
			"google-ads", model.ProviderGoogleAds,
			func() *model.Connection { return activeGoogleAdsConn(goodGoogleAdsCreds) },
			func(r connReader) campaignDispatcher {
				return NewGoogleAdsDispatcher(r, identityEncryptor{}, gaOpts...)
			},
			json.RawMessage(`{"googleAdsConfig":{"budget":50}}`),
			true,
			"",
		},
		{
			"microsoft-ads", model.ProviderMicrosoftAds,
			func() *model.Connection { return activeMicrosoftConn(goodMicrosoftCreds) },
			func(r connReader) campaignDispatcher {
				return NewMicrosoftDispatcher(r, identityEncryptor{}, msOpts...)
			},
			json.RawMessage(`{"microsoftConfig":{"budget":50}}`),
			true,
			"",
		},
		{
			"reddit-ads", model.ProviderRedditAds,
			func() *model.Connection { return activeRedditConn(goodRedditCreds) },
			func(r connReader) campaignDispatcher {
				return NewRedditDispatcher(r, identityEncryptor{},
					reddit.WithBaseURL(redditSrv.URL+"/api/v3"), reddit.WithTokenURL(tokenSrv.URL),
					reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }))
			},
			json.RawMessage(`{"redditConfig":{"budgetUsd":50,"startDate":"2099-08-01","endDate":"2099-08-31","objective":"traffic","subreddits":["kubernetes"]}}`),
			true,
			"",
		},
		{
			"meta-ads", model.ProviderMetaAds,
			func() *model.Connection { return activeMetaConn(goodMetaCreds) },
			func(r connReader) campaignDispatcher {
				return NewMetaDispatcher(r, identityEncryptor{}, meta.WithBaseURL(metaSrv.URL))
			},
			json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","objective":"traffic","geoTargets":["US"],"currencyOffset":100,"variants":[{"PrimaryText":"p","Headline":"h","Description":"d"}]}}`),
			true,
			"",
		},
		{
			"linkedin-ads", model.ProviderLinkedInAds,
			func() *model.Connection { return activeLinkedInConn(goodLinkedInCreds) },
			func(r connReader) campaignDispatcher {
				return NewLinkedInDispatcher(r, identityEncryptor{}, linkedin.WithBaseURL(linkedinSrv.URL))
			},
			json.RawMessage(`{"linkedInConfig":{"budgetUsd":50,"startDate":"2099-01-01","endDate":"2099-02-01","geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],"targetingProfile":"devs","targetingProfiles":[{"id":"devs","label":"Developers","skills":["urn:li:skill:1234"]}],"variants":[{"IntroText":"i","Headline":"h"}]}}`),
			true,
			"urn:li:share/ugcPost",
		},
		{
			"twitter-ads", model.ProviderTwitterAds,
			func() *model.Connection { return activeTwitterConn(goodTwitterCreds) },
			func(r connReader) campaignDispatcher {
				return NewTwitterDispatcher(r, identityEncryptor{}, twitter.WithBaseURL(twitterSrv.URL))
			},
			json.RawMessage(`{"twitterConfig":{"budgetAmount":50,"startDate":"2099-08-01","endDate":"2099-08-31","tweetText":"t"}}`),
			true,
			"",
		},
		{
			"hubspot", model.ProviderHubSpot,
			func() *model.Connection { return activeHubSpotConn(goodHubSpotCreds) },
			func(r connReader) campaignDispatcher {
				return NewHubSpotDispatcher(r, identityEncryptor{},
					fakeAudienceReader{auds: builtHubSpotAudience("26724", []string{"9001"})},
					hubspot.WithBaseURL(hsSrv.URL))
			},
			json.RawMessage(`{"hubspotConfig":{"sourceEmailId":"555"}}`),
			false,
			"",
		},
	}

	if len(cases) != 7 {
		t.Fatalf("this table covers %d dispatchers, but the package has 7 Dispatch methods that "+
			"stamp provenance; an uncovered one can lose its defer silently", len(cases))
	}

	for _, tc := range cases {
		scopes := []bool{false}
		if tc.fallbackEligible {
			scopes = []bool{true, false}
		}
		for _, fromSystem := range scopes {
			scope := "project's own connection"
			if fromSystem {
				scope = "LF system fallback"
			}
			t.Run(tc.name+" via the "+scope, func(t *testing.T) {
				var repo connReader = fakeConnReader{conn: tc.conn()}
				if fromSystem {
					repo = systemScopedConnReader{sys: tc.conn()}
				}
				camp, err := tc.build(repo).Dispatch(context.Background(), testBrief(), tc.platform, tc.config)
				// Assert WHICH ARM ran. Without this the table would keep passing if a fake API
				// drifted and every case silently became a partial — the campaigns would still
				// be stamped, so the provenance assertion below could not tell the difference,
				// and the doc comment's claim about what this exercises would quietly rot.
				switch {
				case tc.wantErrContains == "" && err != nil:
					t.Errorf("%s: expected a clean create, got %v — the fake API drifted, so "+
						"this row is exercising a different exit than it claims", tc.name, err)
				case tc.wantErrContains != "" && err == nil:
					t.Errorf("%s: expected the UNCONFIRMED/partial arm containing %q, got a "+
						"clean create — this row no longer covers the error-carrying exit it "+
						"was added for", tc.name, tc.wantErrContains)
				case tc.wantErrContains != "" && !strings.Contains(err.Error(), tc.wantErrContains):
					t.Errorf("%s: expected an error containing %q, got %v — still an error, but "+
						"a DIFFERENT one, so this row no longer exercises the arm it documents",
						tc.name, tc.wantErrContains, err)
				}
				if camp == nil {
					// NOT a skip. A case that reaches no campaign asserts nothing about
					// provenance, and letting it pass quietly is how the first version of this
					// table came to skip nine of fourteen cases while reporting success.
					t.Fatalf("%s: Dispatch returned no campaign (err=%v) — this case proves "+
						"nothing about the defer; fix the fake API so the create succeeds",
						tc.name, err)
				}
				if camp.RanOnSystemAccount == nil {
					t.Fatalf("%s: Dispatch returned a campaign with NO provenance — the deferred "+
						"stampProvenance did not run on this exit, so the row persists as "+
						"\"unknown\" and is indistinguishable from a legitimate pre-migration row",
						tc.name)
				}
				if *camp.RanOnSystemAccount != fromSystem {
					t.Errorf("%s: RanOnSystemAccount = %v, want %v — the campaign records the "+
						"wrong paying account", tc.name, *camp.RanOnSystemAccount, fromSystem)
				}
			})
		}
	}
}
