// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// noSendTransport fails every request and counts it. A pre-flight refusal is by definition
// raised before anything is sent, so the count must stay zero: a refusal that reached this
// transport would be a refusal of something already on the wire, and the job result is not
// allowed to call that "nothing was sent".
type noSendTransport struct{ sent atomic.Int32 }

func (t *noSendTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.sent.Add(1)
	return nil, errors.New("noSendTransport: a pre-flight refusal must not send anything")
}

// TestDispatch_PreflightRefusalsAreOperatorSafe pins issue #208 per non-Google platform: one
// deterministic pre-flight refusal per dispatcher reaches the orchestrator as a
// domain.PreflightRefusal carrying EXACTLY the sentence the client or dispatcher wrote, which
// service.DispatchFailureMessage then copies into the job result verbatim. It also pins the two things
// that make the message honest: nothing was sent, and the claim contract is unchanged
// (NoUpstreamCreate, nil campaign).
func TestDispatch_PreflightRefusalsAreOperatorSafe(t *testing.T) {
	builtAuds := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	cases := []struct {
		name     string
		provider model.Provider
		build    func(hc *http.Client) refusalDispatcher
		config   string
		want     string
	}{
		{
			// The case the issue was filed for: a connection saved before conversion_pixel_id
			// existed. The message names the connection, which is where the fix is.
			name:     "reddit missing conversion pixel",
			provider: model.ProviderRedditAds,
			build: func(hc *http.Client) refusalDispatcher {
				conn := activeRedditConn(goodRedditCreds)
				conn.ProviderConfig = map[string]string{}
				return NewRedditDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, reddit.WithHTTPClient(hc))
			},
			config: `{"redditConfig":{"budgetUsd":500,"geoTargets":["us"],"startDate":"2027-01-04","endDate":"2027-01-18"}}`,
			want:   "reddit requires a conversion pixel id and none is configured: add it to this project's Reddit connection (Reddit Ads → Events Manager lists the account's pixel)",
		},
		{
			// A platform minimum, from the shared create/write validator.
			name:     "linkedin daily budget under the minimum",
			provider: model.ProviderLinkedInAds,
			build: func(hc *http.Client) refusalDispatcher {
				return NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithHTTPClient(hc))
			},
			config: `{"linkedInConfig":{
				"budgetUsd":5,"startDate":"2099-01-01","endDate":"2099-02-01",
				"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
				"targetingProfile":"cloud-native",
				"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
				"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]
			}}`,
			want: "daily budget 5 is below LinkedIn's minimum of $10 for a daily budget",
		},
		{
			name:     "meta no ad variants",
			provider: model.ProviderMetaAds,
			build: func(hc *http.Client) refusalDispatcher {
				return NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithHTTPClient(hc))
			},
			config: `{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","objective":"traffic","geoTargets":["US"]}}`,
			want:   "at least one ad variant is required for Meta campaign creation",
		},
		{
			name:     "x end date before start date",
			provider: model.ProviderTwitterAds,
			build: func(hc *http.Client) refusalDispatcher {
				return NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, twitter.WithHTTPClient(hc))
			},
			config: `{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-10","endDate":"2099-03-01","tweetId":"1234567890"}}`,
			want:   "end date 2099-03-01 must be after start date 2099-03-10",
		},
		{
			name:     "microsoft budget over the maximum",
			provider: model.ProviderMicrosoftAds,
			build: func(hc *http.Client) refusalDispatcher {
				return NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{}, microsoft.WithHTTPClient(hc))
			},
			config: `{"microsoftConfig":{"budget":2000000000}}`,
			want:   "microsoft-ads campaign budget 2000000000.00 exceeds the maximum 1000000000",
		},
		{
			name:     "hubspot missing source email",
			provider: model.ProviderHubSpot,
			build: func(hc *http.Client) refusalDispatcher {
				return NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, builtAuds, hubspot.WithHTTPClient(hc))
			},
			config: `{"hubspotConfig":{}}`,
			want:   "hubspot campaign requires a sourceEmailId (the template email to clone)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := &noSendTransport{}
			d := tc.build(&http.Client{Transport: tr})
			camp, err := d.Dispatch(context.Background(), testBrief(), tc.provider, json.RawMessage(tc.config))
			if err == nil {
				t.Fatalf("expected a pre-flight refusal, got campaign %+v", camp)
			}
			if camp != nil {
				t.Errorf("a pre-flight refusal must return no campaign, got %+v", camp)
			}
			msg, ok := domain.AsPreflightRefusal(err)
			if !ok {
				t.Fatalf("the refusal is not marked operator-safe, so the job would report the generic string; err = %v", err)
			}
			if msg != tc.want {
				t.Errorf("operator message = %q\nwant              %q", msg, tc.want)
			}
			var nuc interface{ NoUpstreamCreate() bool }
			if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
				t.Errorf("the claim contract changed: a pre-flight refusal must stay NoUpstreamCreate, got %T", err)
			}
			if n := tr.sent.Load(); n != 0 {
				t.Errorf("%d request(s) were sent before the refusal; a pre-flight refusal must send nothing", n)
			}
			// And the job result: the orchestrator's verdict on this error is pre-create
			// (NoUpstreamCreate, nil campaign), and the message is the refusal, verbatim.
			if got := service.DispatchFailureMessage(tc.provider, err, true); got != tc.want {
				t.Errorf("job result error = %q\nwant               %q", got, tc.want)
			}
		})
	}
}

// refusalDispatcher is the one dispatcher method this file exercises.
type refusalDispatcher interface {
	Dispatch(ctx context.Context, brief *model.CampaignBrief, platform model.Provider, config json.RawMessage) (*model.Campaign, error)
}

// upstreamSecret is a secret-looking query parameter planted in every upstream response body
// below. Response bodies are untrusted and can reflect request material (a destination URL's
// caller-permitted secret query, a token in a proxy diagnostic), so the job result must never
// contain it — only the class and the HTTP status.
const upstreamSecret = "access_token=SECRET-QUERY-VALUE-208"

// TestDispatch_UpstreamRefusalReportsStatusNotBody runs a REAL Meta dispatch against a platform
// that refuses the campaign create with a body carrying upstreamSecret. Meta is the sharpest
// case because its error text falls back to the raw response, so the secret provably reaches
// the dispatcher's error chain — the assertion that it does is what keeps this test from
// passing vacuously. The job result must carry the class and the status and nothing else, and
// the outcome must keep the claim contract it had before #208: a definite 4xx is pre-create, an
// ambiguous 5xx is UNCONFIRMED.
func TestDispatch_UpstreamRefusalReportsStatusNotBody(t *testing.T) {
	for _, tc := range []struct {
		name          string
		status        int
		wantPreCreate bool
		want          string
	}{
		{"definite 4xx refusal", http.StatusBadRequest, true, "meta-ads refused the create (HTTP 400); nothing was created"},
		{"ambiguous 5xx stays unconfirmed", http.StatusInternalServerError, false, "meta-ads campaign creation could not be confirmed (the platform answered HTTP 500)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/act_777") {
					_, _ = io.WriteString(w, `{"id":"act_777","name":"LF","account_status":1,"currency":"USD"}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":{"message":"Invalid link https://evil.example/landing?`+upstreamSecret+`","type":"OAuthException","code":100}}`)
			}))
			defer srv.Close()
			d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(srv.URL))
			cfg := json.RawMessage(`{"metaConfig":{
				"budget":2500,"lifetimeBudget":true,"startDate":"2099-01-01","endDate":"2099-02-01",
				"objective":"traffic","geoTargets":["US"],"currencyOffset":100,
				"variants":[{"headline":"KubeCon 2099","primaryText":"Join us","description":"Cloud native event"}]
			}}`)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
			if err == nil {
				t.Fatalf("expected the create to fail, got %+v", camp)
			}
			if !strings.Contains(err.Error(), upstreamSecret) {
				t.Fatalf("fixture no longer puts the body into the error chain, so this test proves nothing; err = %v", err)
			}
			var nuc interface{ NoUpstreamCreate() bool }
			preCreate := errors.As(err, &nuc) && nuc.NoUpstreamCreate() && camp == nil
			if preCreate != tc.wantPreCreate {
				t.Fatalf("claim verdict changed: preCreate = %v, want %v (camp=%v, err=%v)", preCreate, tc.wantPreCreate, camp != nil, err)
			}
			got := service.DispatchFailureMessage(model.ProviderMetaAds, err, preCreate)
			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("job result error = %q\nwant prefix          %q", got, tc.want)
			}
			for _, leak := range []string{"SECRET", "evil.example", "access_token", "OAuthException", "/campaigns", "act_777"} {
				if strings.Contains(got, leak) {
					t.Errorf("job result error %q leaks %q from the upstream response or request", got, leak)
				}
			}
		})
	}
}

// TestDispatch_TokenRefreshFailureIsClassified runs a REAL Reddit dispatch whose OAuth token
// exchange is refused with a body carrying upstreamSecret. The create was never sent, so the
// claim is released, and the job result says the token refresh failed — without the body.
func TestDispatch_TokenRefreshFailureIsClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","detail":"https://www.reddit.com/api/v1/access_token?`+upstreamSecret+`"}`)
	}))
	defer srv.Close()
	d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(srv.URL), reddit.WithTokenURL(srv.URL+"/api/v1/access_token"))
	cfg := json.RawMessage(`{"redditConfig":{"budgetUsd":500,"geoTargets":["us"],"startDate":"2099-01-04","endDate":"2099-01-18"}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, cfg)
	if err == nil {
		t.Fatalf("expected the create to fail, got %+v", camp)
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if camp != nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Fatalf("a refused token exchange sends no create, so it must stay pre-create; camp=%v err=%v", camp, err)
	}
	got := service.DispatchFailureMessage(model.ProviderRedditAds, err, true)
	if want := "reddit-ads token refresh failed, so nothing was sent; the connection may need re-authorizing"; got != want {
		t.Errorf("job result error = %q\nwant               %q", got, want)
	}
	if strings.Contains(got, "SECRET") || strings.Contains(got, "invalid_grant") || strings.Contains(got, "access_token") {
		t.Errorf("job result error %q leaks the token endpoint's response", got)
	}
}
