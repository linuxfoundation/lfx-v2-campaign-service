// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	conn "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_connections"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// connectionProberStub implements ConnectionProber (and the bare PlatformDispatcher the
// orchestrator's map requires) so the service-layer classification can be driven directly.
type connectionProberStub struct {
	err          error
	gotProjectID string
	gotPlatform  model.Provider
	calls        int
}

func (s *connectionProberStub) Dispatch(_ context.Context, _ *model.CampaignBrief, _ model.Provider, _ json.RawMessage) (*model.Campaign, error) {
	return nil, nil
}

func (s *connectionProberStub) ProbeConnection(_ context.Context, projectID string, platform model.Provider) error {
	s.calls++
	s.gotProjectID = projectID
	s.gotPlatform = platform
	return s.err
}

// newProbeService builds a service holding one credentialed Google Ads connection, wired to an
// orchestrator whose only dispatcher is the supplied stub.
func newProbeService(t *testing.T, stub *connectionProberStub) *ConnectionService {
	t.Helper()
	s := newTestService(t, newFakeRepo())
	if _, err := s.CreateGoogleAds(context.Background(), &conn.CreateGoogleAdsPayload{
		ProjectID: "tlf",
		Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("8666746580")},
		Credentials: &conn.GoogleAdsCredentials{
			RefreshToken: "rt", ClientID: "ci", ClientSecret: "cs", DeveloperToken: "dt",
		},
	}); err != nil {
		t.Fatalf("CreateGoogleAds: %v", err)
	}
	s.SetOrchestrator(&Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: stub},
	})
	return s
}

func testGoogle(t *testing.T, s *ConnectionService) (*conn.ConnectionTestResult, error) {
	t.Helper()
	return s.TestGoogleAds(context.Background(), &conn.TestGoogleAdsPayload{ProjectID: "tlf"})
}

// TestTestConnUpstream_ProbeRuns is the headline assertion of LFXV2-2665: the connection test
// no longer answers from the presence of a credential blob in the row. It reaches the platform.
//
// Before this, TestGoogleAds returned OK: true for any row with credentials stored — never
// decrypting them, never authenticating, never touching the configured account. A refresh token
// revoked months earlier tested clean and failed at campaign creation.
func TestTestConnUpstream_ProbeRuns(t *testing.T) {
	stub := &connectionProberStub{}
	s := newProbeService(t, stub)

	res, err := testGoogle(t, s)
	if err != nil {
		t.Fatalf("TestGoogleAds: %v", err)
	}
	if stub.calls != 1 {
		t.Fatalf("the prober was called %d times; the connection test is still answering without contacting the platform", stub.calls)
	}
	if stub.gotProjectID != "tlf" || stub.gotPlatform != model.ProviderGoogleAds {
		t.Errorf("prober saw (%q, %q), want (\"tlf\", %q)", stub.gotProjectID, stub.gotPlatform, model.ProviderGoogleAds)
	}
	if !res.OK {
		t.Fatalf("OK = false for a successful probe; message = %v", res.Message)
	}
	if res.Message == nil || strings.Contains(*res.Message, "not been run") {
		t.Errorf("message = %v; it still carries testConn's baseline text, which claims no verification ran", res.Message)
	}
}

// TestTestConnUpstream_NoCredentialsSkipsProbe pins the short-circuit. There is nothing to
// verify without a credential, and the two failures have different remedies — authorize the
// connection versus re-authorize it — so they must not collapse into one message.
func TestTestConnUpstream_NoCredentialsSkipsProbe(t *testing.T) {
	repo := newFakeRepo()
	repo.store[repoKey("tlf", model.ProviderGoogleAds)] = &model.Connection{
		ProjectID: "tlf", Provider: model.ProviderGoogleAds,
		AccountID: "8666746580", Status: model.StatusActive, Version: 1,
		// EncryptedCredentials deliberately empty.
	}
	s := newTestService(t, repo)
	stub := &connectionProberStub{}
	s.SetOrchestrator(&Orchestrator{
		dispatchers: map[model.Provider]PlatformDispatcher{model.ProviderGoogleAds: stub},
	})

	res, err := testGoogle(t, s)
	if err != nil {
		t.Fatalf("TestGoogleAds: %v", err)
	}
	if stub.calls != 0 {
		t.Error("the prober ran for a connection with no stored credential; there is nothing to verify")
	}
	if res.OK {
		t.Error("OK = true for a connection with no stored credentials")
	}
	if res.Message == nil || !strings.Contains(*res.Message, "no credentials are stored") {
		t.Errorf("message = %v, does not name the absent credential as the reason", res.Message)
	}
	if res.Message != nil && !strings.Contains(*res.Message, "google ads") {
		t.Errorf("message = %v, does not name the provider; the same helper serves six of them", res.Message)
	}
}

// TestTestConnUpstream_Classification walks every arm of the shared switch. Each one exists
// because the arm below it would otherwise have answered wrongly, and three of them exist
// specifically to keep text this service did not author out of the HTTP body.
func TestTestConnUpstream_Classification(t *testing.T) {
	const canary = "DO-NOT-LEAK-https://googleads.googleapis.com/v18/customers?token=SECRET"

	t.Run("confirmed failure reports OK: false and echoes the authored verdict", func(t *testing.T) {
		probeErr := fmt.Errorf("%w: google-ads rejected the stored credential for account 8666746580",
			domain.ErrConnectionProbeFailed)
		s := newProbeService(t, &connectionProberStub{err: probeErr})
		res, err := testGoogle(t, s)
		if err != nil {
			t.Fatalf("TestGoogleAds: %v, want a normal (result, nil) failed test rather than a transport error", err)
		}
		if res.OK {
			t.Fatal("OK = true despite the platform rejecting the stored credential")
		}
		// The echo is the whole value of this arm: "verification failed" alone leaves an
		// operator nothing to repair.
		if res.Message == nil || !strings.Contains(*res.Message, "rejected the stored credential") {
			t.Errorf("message = %v, does not say what the platform actually did", res.Message)
		}
		if res.Message != nil && !strings.Contains(*res.Message, "8666746580") {
			t.Errorf("message = %v, does not name the account the verdict is about", res.Message)
		}
	})

	t.Run("inconclusive reports OK: true, not a failed test", func(t *testing.T) {
		// Nothing was learned about the connection, and the credential baseline already
		// passed. Reporting OK: false here would tell an operator their working connection is
		// broken every time the platform throttles or times out.
		probeErr := fmt.Errorf("%w: the google-ads check could not be completed", domain.ErrConnectionProbeInconclusive)
		s := newProbeService(t, &connectionProberStub{err: probeErr})
		res, err := testGoogle(t, s)
		if err != nil {
			t.Fatalf("TestGoogleAds: %v", err)
		}
		if !res.OK {
			t.Fatalf("OK = false for an inconclusive probe; message = %v", res.Message)
		}
		if res.Message == nil || !strings.Contains(*res.Message, "inconclusive") {
			t.Errorf("message = %v, does not tell the caller the check did not complete", res.Message)
		}
	})

	t.Run("a service defect is a typed 500, not a failed test", func(t *testing.T) {
		// Nothing the operator owns is at fault and no field they can edit repairs it, so
		// OK: false would send them to audit a connection that is fine.
		probeErr := fmt.Errorf("%w: %w: google-ads refused the connection-probe request this service built",
			domain.ErrServiceDefect, domain.ErrConnectionProbeRequestRejected)
		s := newProbeService(t, &connectionProberStub{err: probeErr})
		res, err := testGoogle(t, s)
		if res != nil {
			t.Errorf("result = %+v, want nil alongside the typed error", res)
		}
		if _, ok := err.(*conn.InternalServerError); !ok {
			t.Fatalf("err = %T (%v), want *conn.InternalServerError", err, err)
		}
	})

	t.Run("an unwired dispatcher is a typed 500", func(t *testing.T) {
		// No dispatcher registered at all: the orchestrator's unconditional unwired arm. This
		// must NOT be silent — returning nil would answer OK: true having verified nothing,
		// which is the exact bug this work removes.
		s := newTestService(t, newFakeRepo())
		if _, err := s.CreateGoogleAds(context.Background(), &conn.CreateGoogleAdsPayload{
			ProjectID: "tlf",
			Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("8666746580")},
			Credentials: &conn.GoogleAdsCredentials{
				RefreshToken: "rt", ClientID: "ci", ClientSecret: "cs", DeveloperToken: "dt",
			},
		}); err != nil {
			t.Fatalf("CreateGoogleAds: %v", err)
		}
		s.SetOrchestrator(&Orchestrator{dispatchers: map[model.Provider]PlatformDispatcher{}})
		res, err := testGoogle(t, s)
		if res != nil && res.OK {
			t.Fatal("OK = true with no dispatcher registered; the test verified nothing and said so to nobody")
		}
		if _, ok := err.(*conn.InternalServerError); !ok {
			t.Fatalf("err = %T (%v), want *conn.InternalServerError", err, err)
		}
	})

	t.Run("a decryption failure is a typed 500 carrying no error text", func(t *testing.T) {
		// The chain is built by domain.Encryptor from ciphertext and key material, which an
		// implementation is free to quote.
		probeErr := fmt.Errorf("%w: %s", domain.ErrCredentialDecryptionFailed, canary)
		s := newProbeService(t, &connectionProberStub{err: probeErr})
		_, err := testGoogle(t, s)
		ise, ok := err.(*conn.InternalServerError)
		if !ok {
			t.Fatalf("err = %T (%v), want *conn.InternalServerError", err, err)
		}
		if strings.Contains(ise.Message, "DO-NOT-LEAK") {
			t.Errorf("message = %q carries the decryption error chain, which can quote ciphertext and key material", ise.Message)
		}
	})

	t.Run("a datastore read failure is a 503, the one retryable arm", func(t *testing.T) {
		// Nothing was learned about the connection. Without this arm it fell to the default
		// and reported the connection broken because this service could not look at it.
		probeErr := fmt.Errorf("%w: %s", domain.ErrConnectionLoadFailed, canary)
		s := newProbeService(t, &connectionProberStub{err: probeErr})
		res, err := testGoogle(t, s)
		if res != nil && res.OK {
			t.Error("OK = true for a datastore failure")
		}
		sue, ok := err.(*conn.ConnServiceUnavailableError)
		if !ok {
			t.Fatalf("err = %T (%v), want *conn.ConnServiceUnavailableError", err, err)
		}
		if strings.Contains(sue.Message, "DO-NOT-LEAK") {
			t.Errorf("message = %q carries the repo error, which can quote the query or the row", sue.Message)
		}
	})

	t.Run("an unusable connection reports OK: false with the remedy, not the error", func(t *testing.T) {
		// One of the conditions behind this sentinel is detected by decoding the DECRYPTED
		// blob, and encoding/json quotes its input — so the echo would put credential-derived
		// bytes into an HTTP body for exactly the connection whose credentials are malformed.
		probeErr := fmt.Errorf("%w: %w: %s", domain.ErrConnectionNotUsable, domain.ErrCredentialsUndecodable, canary)
		s := newProbeService(t, &connectionProberStub{err: probeErr})
		res, err := testGoogle(t, s)
		if err != nil {
			t.Fatalf("TestGoogleAds: %v, want a normal failed test", err)
		}
		if res.OK {
			t.Fatal("OK = true for a connection that cannot be used as configured")
		}
		if res.Message == nil || strings.Contains(*res.Message, "DO-NOT-LEAK") {
			t.Fatalf("message = %v, want the remedy text with no part of the underlying error", res.Message)
		}
		if !strings.Contains(*res.Message, googleAdsAccountDiscovery.notUsableRemedy) {
			t.Errorf("message = %q does not carry the provider's remedy; it is the only actionable thing the caller is told", *res.Message)
		}
	})

	// The reason the echo is an allowlist rather than a default. Every class that reaches this
	// switch without an arm of its own used to inherit the echo simply by not matching one.
	t.Run("an unclassified probe error is not echoed", func(t *testing.T) {
		s := newProbeService(t, &connectionProberStub{err: errors.New(canary)})
		res, err := testGoogle(t, s)
		if err != nil {
			t.Fatalf("TestGoogleAds: %v, want a normal failed test", err)
		}
		if res.OK {
			t.Fatal("OK = true for an error that proves no probe succeeded")
		}
		if res.Message == nil || strings.Contains(*res.Message, "DO-NOT-LEAK") {
			t.Errorf("message = %v, want fixed text with no part of the unclassified error", res.Message)
		}
	})
}

// TestTestConnUpstream_AllSixEndpointsProbe pins that every endpoint LFXV2-2665 names is
// actually routed through the probe, and that each names its OWN provider when doing so.
//
// Routing five of six and leaving one on the old baseline is the most likely way this
// regresses, and it is invisible from any single-provider test: the missed endpoint keeps
// returning OK: true and nothing fails.
func TestTestConnUpstream_AllSixEndpointsProbe(t *testing.T) {
	cases := []struct {
		name        string
		platform    model.Provider
		displayName string
		create      func(*ConnectionService) error
		test        func(*ConnectionService) (*conn.ConnectionTestResult, error)
	}{
		{
			name: "google ads", platform: model.ProviderGoogleAds, displayName: "google ads",
			create: func(s *ConnectionService) error {
				_, err := s.CreateGoogleAds(context.Background(), &conn.CreateGoogleAdsPayload{
					ProjectID: "tlf",
					Config:    &conn.GoogleAdsConnectionConfig{AccountID: strPtr("8666746580")},
					Credentials: &conn.GoogleAdsCredentials{
						RefreshToken: "rt", ClientID: "ci", ClientSecret: "cs", DeveloperToken: "dt",
					},
				})
				return err
			},
			test: func(s *ConnectionService) (*conn.ConnectionTestResult, error) {
				return s.TestGoogleAds(context.Background(), &conn.TestGoogleAdsPayload{ProjectID: "tlf"})
			},
		},
		{
			name: "meta ads", platform: model.ProviderMetaAds, displayName: "meta ads",
			create: func(s *ConnectionService) error {
				_, err := s.CreateMetaAds(context.Background(), &conn.CreateMetaAdsPayload{
					ProjectID:   "tlf",
					Config:      &conn.MetaAdsConnectionConfig{AccountID: strPtr("act_193556282970417"), PageID: "1234567890"},
					Credentials: &conn.MetaAdsCredentials{AccessToken: "tok"},
				})
				return err
			},
			test: func(s *ConnectionService) (*conn.ConnectionTestResult, error) {
				return s.TestMetaAds(context.Background(), &conn.TestMetaAdsPayload{ProjectID: "tlf"})
			},
		},
		{
			name: "reddit ads", platform: model.ProviderRedditAds, displayName: "reddit ads",
			create: func(s *ConnectionService) error {
				_, err := s.CreateRedditAds(context.Background(), &conn.CreateRedditAdsPayload{
					ProjectID: "tlf",
					Config:    &conn.RedditAdsConnectionConfig{AccountID: "t2_gv9wtbfa"},
					Credentials: &conn.RedditAdsCredentials{
						ClientID: "ci", ClientSecret: "cs", RefreshToken: "rt",
					},
				})
				return err
			},
			test: func(s *ConnectionService) (*conn.ConnectionTestResult, error) {
				return s.TestRedditAds(context.Background(), &conn.TestRedditAdsPayload{ProjectID: "tlf"})
			},
		},
		{
			name: "x/twitter ads", platform: model.ProviderTwitterAds, displayName: "x/twitter ads",
			create: func(s *ConnectionService) error {
				_, err := s.CreateTwitterAds(context.Background(), &conn.CreateTwitterAdsPayload{
					ProjectID: "tlf",
					Config:    &conn.TwitterAdsConnectionConfig{AccountID: strPtr("18ce54d4x5t"), FundingInstrumentID: "lygyi"},
					Credentials: &conn.TwitterAdsCredentials{
						ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats",
					},
				})
				return err
			},
			test: func(s *ConnectionService) (*conn.ConnectionTestResult, error) {
				return s.TestTwitterAds(context.Background(), &conn.TestTwitterAdsPayload{ProjectID: "tlf"})
			},
		},
		{
			name: "microsoft ads", platform: model.ProviderMicrosoftAds, displayName: "microsoft ads",
			create: func(s *ConnectionService) error {
				_, err := s.CreateMicrosoftAds(context.Background(), &conn.CreateMicrosoftAdsPayload{
					ProjectID: "tlf",
					Config:    &conn.MicrosoftAdsConnectionConfig{AccountID: "1234"},
					Credentials: &conn.MicrosoftAdsCredentials{
						ClientID: "ci", ClientSecret: "cs", DeveloperToken: "dt", RefreshToken: "rt",
					},
				})
				return err
			},
			test: func(s *ConnectionService) (*conn.ConnectionTestResult, error) {
				return s.TestMicrosoftAds(context.Background(), &conn.TestMicrosoftAdsPayload{ProjectID: "tlf"})
			},
		},
		{
			name: "hubspot", platform: model.ProviderHubSpot, displayName: "hubspot",
			create: func(s *ConnectionService) error {
				_, err := s.CreateHubspot(context.Background(), &conn.CreateHubspotPayload{
					ProjectID:   "tlf",
					Config:      &conn.HubspotConnectionConfig{AccountID: "1", PortalID: strPtr("8112310")},
					Credentials: &conn.HubspotCredentials{PrivateAppToken: "pat"},
				})
				return err
			},
			test: func(s *ConnectionService) (*conn.ConnectionTestResult, error) {
				return s.TestHubspot(context.Background(), &conn.TestHubspotPayload{ProjectID: "tlf"})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestService(t, newFakeRepo())
			if err := tc.create(s); err != nil {
				t.Fatalf("create: %v", err)
			}
			stub := &connectionProberStub{}
			s.SetOrchestrator(&Orchestrator{
				dispatchers: map[model.Provider]PlatformDispatcher{tc.platform: stub},
			})

			if _, err := tc.test(s); err != nil {
				t.Fatalf("test endpoint: %v", err)
			}
			if stub.calls != 1 {
				t.Fatalf("the prober was called %d times; this endpoint still answers from the row alone", stub.calls)
			}
			if stub.gotPlatform != tc.platform {
				t.Errorf("prober saw platform %q, want %q — this endpoint probes a DIFFERENT platform's connection", stub.gotPlatform, tc.platform)
			}

			// And the authored messages must name this provider, not a copied one. Every
			// status assertion above passes with the wrong display name, which is exactly how
			// a copied remedy reaches production.
			stub.err = fmt.Errorf("%w: the check could not be completed", domain.ErrConnectionProbeInconclusive)
			res, err := tc.test(s)
			if err != nil {
				t.Fatalf("test endpoint (inconclusive): %v", err)
			}
			if res.Message == nil || !strings.Contains(*res.Message, tc.displayName) {
				t.Errorf("message = %v, does not name %q", res.Message, tc.displayName)
			}
		})
	}
}

// TestUnusableConnectionReason_ProbeSentinels closes the gap the probe path opened in the
// reason vocabulary.
//
// The connection-test response for a service defect deliberately carries no detail — it is a
// typed 500 with fixed text — so the log's reason token is the ONLY diagnostic anyone gets.
// Both probe sentinels travel alongside domain.ErrServiceDefect, and without an arm apiece
// every service-defect probe logged reason=unclassified, which is precisely the reading this
// vocabulary exists to prevent: an unclassified token says "no sentinel was attached", and
// there were two.
func TestUnusableConnectionReason_ProbeSentinels(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "the platform refused the request this service built",
			err: fmt.Errorf("%w: %w: google ads refused the connection-probe request this service built",
				domain.ErrServiceDefect, domain.ErrConnectionProbeRequestRejected),
			want: "probe_request_rejected",
		},
		{
			name: "no dispatcher implements ConnectionProber",
			err: fmt.Errorf("%w: %w: google ads",
				domain.ErrServiceDefect, domain.ErrConnectionProbeUnwired),
			want: "probe_unwired",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unusableConnectionReason(tc.err); got != tc.want {
				t.Errorf("unusableConnectionReason = %q, want %q; the response carries no detail, so this token is the whole diagnostic", got, tc.want)
			}
		})
	}
}
