// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
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
			config: `{"redditConfig":{"budgetUsd":500,"geoTargets":["us"],"startDate":"2099-01-04","endDate":"2099-01-18"}}`,
			want:   "reddit requires a conversion pixel id and none is configured: add it to this project's Reddit connection (Reddit Ads → Events Manager lists the account's pixel)",
		},
		{
			// A composed name Reddit would reject. It used to be checked only AFTER the
			// account-verify GET, so the dispatch sent a token exchange and a GET to learn
			// nothing, and the refusal was unmarked (the job reported the generic string).
			// Sixty valid codes make the "+"-joined geo segment push the ad-group name past
			// Reddit's 200-rune limit while every per-field check passes.
			name:     "reddit composed ad group name too long",
			provider: model.ProviderRedditAds,
			build: func(hc *http.Client) refusalDispatcher {
				return NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{}, reddit.WithHTTPClient(hc))
			},
			config: `{"redditConfig":{"budgetUsd":500,"geoTargets":["us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us","us"],"startDate":"2099-01-04","endDate":"2099-01-18"}}`,
			want:   "composed ad group name is too long: 240 characters (max 200)",
		},
		{
			// A window Reddit would reject: the past start is nudged to now + the workflow
			// buffer (22 minutes) and the end, midnight of the same day, is not after it. It used
			// to be decided only AFTER the account-verify GET, as a bare fmt.Errorf. The clock is
			// pinned so the nudged instant in the sentence is stable.
			name:     "reddit end not after the nudged start",
			provider: model.ProviderRedditAds,
			build: func(hc *http.Client) refusalDispatcher {
				return NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{}, reddit.WithHTTPClient(hc),
					reddit.WithNowFunc(func() time.Time { return time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC) }))
			},
			config: `{"redditConfig":{"budgetUsd":500,"geoTargets":["us"],"startDate":"2026-06-01","endDate":"2026-07-01"}}`,
			want:   "campaign end 2026-07-01T00:00:00+00:00 is not after the effective start 2026-07-01T00:22:00+00:00 (a past start date was nudged forward)",
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
			// Every supplied geo is ineligible. Decided from the config and static tables alone,
			// so refused before the account preflight GET (it used to run after it, unmarked).
			name:     "meta no usable geo targets",
			provider: model.ProviderMetaAds,
			build: func(hc *http.Client) refusalDispatcher {
				return NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithHTTPClient(hc))
			},
			config: `{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","objective":"traffic","geoTargets":["IR","KP"],"currencyOffset":100,
				"variants":[{"headline":"KubeCon 2099","primaryText":"Join us","description":"Cloud native event"}]}}`,
			want: `no usable geo targets: all supplied geos are invalid or ineligible for Meta ads targeting (dropped: "IR", "KP") — refusing to silently fall back to US`,
		},
		{
			name:     "meta all geos regulated",
			provider: model.ProviderMetaAds,
			build: func(hc *http.Client) refusalDispatcher {
				return NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithHTTPClient(hc))
			},
			config: `{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","objective":"traffic","geoTargets":["SG","TW"],"currencyOffset":100,
				"variants":[{"headline":"KubeCon 2099","primaryText":"Join us","description":"Cloud native event"}]}}`,
			want: "meta campaign skipped: all selected geo targets (SG, TW) are regulated and excluded from API targeting; supply at least one eligible (non-regulated) geo target",
		},
		{
			name:     "x end date before start date",
			provider: model.ProviderTwitterAds,
			build: func(hc *http.Client) refusalDispatcher {
				return NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, twitter.WithHTTPClient(hc))
			},
			config: `{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-10","endDate":"2099-03-01","tweetId":"1234567890"}}`,
			want:   `end date "2099-03-01" must be after start date "2099-03-10"`,
		},
		{
			// validateDate is a shared helper; its refusal is marked at the CreateCampaign call.
			name:     "x start date not on the calendar",
			provider: model.ProviderTwitterAds,
			build: func(hc *http.Client) refusalDispatcher {
				return NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, twitter.WithHTTPClient(hc))
			},
			config: `{"twitterConfig":{"budgetAmount":500,"startDate":"2099-02-30","endDate":"2099-03-10","tweetId":"1234567890"}}`,
			want:   `invalid start date: "2099-02-30" is not a real calendar date`,
		},
		{
			// LFXV2-2665 create-time bidding: Dispatch re-runs the bid plan before any upstream
			// call (for a path that skipped PreflightCreate), so its refusal is marked like the
			// budget checks beside it. The X client's own copy of this check is marked too.
			name:     "x manual bid over the daily budget",
			provider: model.ProviderTwitterAds,
			build: func(hc *http.Client) refusalDispatcher {
				return NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, twitter.WithHTTPClient(hc))
			},
			config: `{"twitterConfig":{"budgetAmount":5,"startDate":"2099-03-01","endDate":"2099-03-10","tweetId":"1234567890","bidding":{"strategy":"manual","bidAmount":6}}}`,
			want:   "invalid campaign config: twitterConfig.bidding: bidAmount 6 exceeds the daily budget 5; X does not accept a line item bid above its campaign's budget",
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
		{"definite 4xx refusal", http.StatusBadRequest, true, "meta-ads refused the request (HTTP 400) before anything was created; nothing was created"},
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

// toServerTransport sends every request to srv, keeping the path and query. LinkedIn's token
// endpoint override is unexported, so this is how a dispatch-depth test points a REAL LinkedIn
// client's refresh exchange at an httptest server.
type toServerTransport struct{ srv *httptest.Server }

func (t toServerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	u, err := url.Parse(t.srv.URL)
	if err != nil {
		return nil, err
	}
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host, r.Host = u.Scheme, u.Host, u.Host
	return t.srv.Client().Transport.RoundTrip(r)
}

// refreshableLinkedInCreds is refreshable in shape with an EXPIRED access token, so the token
// exchange is the first request Dispatch makes. The values are placeholders, not credentials.
var refreshableLinkedInCreds = func() string {
	past := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	future := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	return `{"AccessToken":"at","RefreshToken":"rt","ClientID":"ci","ClientSecret":"cs",` +
		`"AccessTokenExpiresAt":"` + past + `","RefreshTokenExpiresAt":"` + future + `"}`
}()

const linkedInRefreshTestConfig = `{"linkedInConfig":{
	"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
	"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
	"targetingProfile":"cloud-native",
	"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
	"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]
}}`

// TestDispatch_ProviderTokenRefreshFailureIsClassified is the LinkedIn and Microsoft
// counterpart of TestDispatch_TokenRefreshFailureIsClassified: a REAL dispatcher whose token
// endpoint (an httptest server) answers 503 with a body carrying upstreamSecret. A 503 is not
// a verdict on the stored credential, so neither client tags it as a connection fault, and the
// job result must be the token-refresh sentence — reached through each client's own
// domain.MarkTokenRefreshFailure — without the body. Nothing but the token request is sent.
func TestDispatch_ProviderTokenRefreshFailureIsClassified(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider model.Provider
		build    func(srv *httptest.Server) refusalDispatcher
		config   string
	}{
		{
			name:     "linkedin",
			provider: model.ProviderLinkedInAds,
			build: func(srv *httptest.Server) refusalDispatcher {
				return NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(refreshableLinkedInCreds)}, identityEncryptor{},
					linkedin.WithHTTPClient(&http.Client{Transport: toServerTransport{srv: srv}}), linkedin.WithBaseURL(srv.URL))
			},
			config: linkedInRefreshTestConfig,
		},
		{
			name:     "microsoft",
			provider: model.ProviderMicrosoftAds,
			build: func(srv *httptest.Server) refusalDispatcher {
				return NewMicrosoftDispatcher(fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
					microsoft.WithBaseURL(srv.URL), microsoft.WithCustomerBaseURL(srv.URL), microsoft.WithTokenURL(srv.URL+"/token"))
			},
			// geoTargets makes the geo resolve the first networked step; it runs before the
			// find-existing lookup, so a refresh failure there is a clean pre-create abort. (A
			// lookup failure is deliberately UNCONFIRMED: it cannot prove a prior attempt's
			// campaign is absent.)
			config: `{"microsoftConfig":{"budget":50,"geoTargets":["GB"]}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, `{"error":"temporarily_unavailable","error_description":"retry https://login.example/token?`+upstreamSecret+`"}`)
			}))
			defer srv.Close()
			camp, err := tc.build(srv).Dispatch(context.Background(), testBrief(), tc.provider, json.RawMessage(tc.config))
			if err == nil {
				t.Fatalf("expected the create to fail, got %+v", camp)
			}
			if !domain.IsTokenRefreshFailure(err) {
				t.Fatalf("the refresh failure is not marked, so the job would read the generic string; err = %v", err)
			}
			if camp != nil || !isNoUpstreamCreate(err) {
				t.Fatalf("a failed token exchange sends no create, so it must stay pre-create; camp=%v err=%v", camp, err)
			}
			got := service.DispatchFailureMessage(tc.provider, err, true)
			if want := string(tc.provider) + " token refresh failed, so nothing was sent; the connection may need re-authorizing"; got != want {
				t.Errorf("job result error = %q\nwant               %q", got, want)
			}
			for _, leak := range []string{"SECRET", "temporarily_unavailable", "access_token", "login.example"} {
				if strings.Contains(got, leak) {
					t.Errorf("job result error %q leaks %q from the token endpoint's response", got, leak)
				}
			}
			if n := requests.Load(); n != 1 {
				t.Errorf("%d request(s) reached the fake platform; want exactly the one token exchange", n)
			}
		})
	}
}

// TestDispatch_CancelledTokenRefreshIsNotReportedAsARefreshFailure cancels the CALLER's
// context while a real LinkedIn token exchange is in flight (the fake token endpoint blocks
// until the test releases it). The cancellation says nothing about the credential, so the
// job result must be the generic string, not "the connection may need re-authorizing".
func TestDispatch_CancelledTokenRefreshIsNotReportedAsARefreshFailure(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	defer close(release)
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(refreshableLinkedInCreds)}, identityEncryptor{},
		linkedin.WithHTTPClient(&http.Client{Transport: toServerTransport{srv: srv}}), linkedin.WithBaseURL(srv.URL))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-entered:
			cancel()
		case <-time.After(10 * time.Second):
		}
	}()
	camp, err := d.Dispatch(ctx, testBrief(), model.ProviderLinkedInAds, json.RawMessage(linkedInRefreshTestConfig))
	if err == nil {
		t.Fatalf("expected the cancelled dispatch to fail, got %+v", camp)
	}
	select {
	case <-entered:
	default:
		t.Fatalf("the token exchange never reached the fake endpoint, so nothing was cancelled mid-refresh; err = %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v; want the caller's cancellation in the chain", err)
	}
	preCreate := camp == nil && isNoUpstreamCreate(err)
	if !preCreate {
		t.Fatalf("a refresh cancelled before any create was sent must stay pre-create; camp=%v err=%v", camp, err)
	}
	if got := service.DispatchFailureMessage(model.ProviderLinkedInAds, err, preCreate); got != "platform campaign creation failed" {
		t.Errorf("job result error = %q; want the generic fallback — a cancelled refresh says nothing about the credential", got)
	}
}

// TestDispatch_RefusalsNeverEchoHostileCallerInput: a marked refusal is copied into the job
// result verbatim, so any CALLER value it names must go through domain.CallerValue. A field
// that should hold an id can hold a pasted URL whose query carries a token, an arbitrarily
// long value, or a multi-line paste. Real X and LinkedIn dispatches, refused before anything
// is sent, with each shape in the field the refusal names.
func TestDispatch_RefusalsNeverEchoHostileCallerInput(t *testing.T) {
	const hostileURL = "https://x.com/a?token=SECRET-208"
	long := strings.Repeat("a", 200)
	xCfg := func(tweetID string) string {
		b, _ := json.Marshal(map[string]any{"twitterConfig": map[string]any{
			"budgetAmount": 500, "startDate": "2099-03-01", "endDate": "2099-03-10", "tweetId": tweetID,
		}})
		return string(b)
	}
	liCfg := func(urn string) string {
		b, _ := json.Marshal(map[string]any{"linkedInConfig": map[string]any{
			"budgetUsd": 100, "startDate": "2099-01-01", "endDate": "2099-02-01",
			"geoTargets":        []map[string]string{{"label": "United States", "urn": "urn:li:geo:103644278"}},
			"targetingProfile":  "cloud-native",
			"targetingProfiles": []map[string]any{{"id": "cloud-native", "label": "Cloud Native", "skills": []string{"urn:li:skill:1"}}},
			"variants":          []map[string]string{{"introText": "Join us — it's great", "headline": "KubeCon 2099", "ImageURN": urn}},
		}})
		return string(b)
	}
	x := func(hc *http.Client) refusalDispatcher {
		return NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, twitter.WithHTTPClient(hc))
	}
	li := func(hc *http.Client) refusalDispatcher {
		return NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{}, linkedin.WithHTTPClient(hc))
	}
	for _, tc := range []struct {
		name     string
		provider model.Provider
		build    func(*http.Client) refusalDispatcher
		config   string
		want     string // a fragment the refusal must still carry
	}{
		{"x tweet id is a secret-bearing URL", model.ProviderTwitterAds, x, xCfg(hostileURL), "invalid tweet id: (value withheld)"},
		{"x tweet id is over-long", model.ProviderTwitterAds, x, xCfg(long), `invalid tweet id: "` + strings.Repeat("a", 64) + `…"`},
		{"x tweet id spans lines", model.ProviderTwitterAds, x, xCfg("123\nSECRET-208"), "invalid tweet id: (value withheld)"},
		{"x tweet id is a bare query", model.ProviderTwitterAds, x, xCfg("access_token=SECRET-208&scope=ads"), "invalid tweet id: (value withheld)"},
		{"x tweet id is a long opaque token", model.ProviderTwitterAds, x, xCfg("ghp_SECRET208abcdefghijklmnopqrstuvwxyz0123"), "invalid tweet id: (value withheld)"}, // secretlint-disable-line -- intentional fake token, asserting it is withheld
		{"x tweet id is a protocol-relative URL", model.ProviderTwitterAds, x, xCfg("//events.example/reset/SECRET-208"), "invalid tweet id: (value withheld)"},
		{"x tweet id is a percent-encoded payload", model.ProviderTwitterAds, x, xCfg("SECRET%2F208"), "invalid tweet id: (value withheld)"},
		{"linkedin image urn is a protocol-relative URL", model.ProviderLinkedInAds, li, liCfg("//events.example/reset/SECRET-208"), "variant-1 image URN (value withheld) is malformed"},
		{"linkedin image urn carries a host and path", model.ProviderLinkedInAds, li, liCfg("urn:li:image:events.example/SECRET-208"), "variant-1 image URN (value withheld) is malformed"},
		{"x tweet id is an absolute path", model.ProviderTwitterAds, x, xCfg("/SECRET-208"), "invalid tweet id: (value withheld)"},
		{"x tweet id is a relative path", model.ProviderTwitterAds, x, xCfg("reset/SECRET-208"), "invalid tweet id: (value withheld)"},
		{"linkedin image urn is an absolute path", model.ProviderLinkedInAds, li, liCfg("/SECRET-208"), "variant-1 image URN (value withheld) is malformed"},
		{"linkedin image urn is a relative path", model.ProviderLinkedInAds, li, liCfg("reset/SECRET-208"), "variant-1 image URN (value withheld) is malformed"},
		{"linkedin image urn is a bare query", model.ProviderLinkedInAds, li, liCfg("urn:li:image:1?access_token=SECRET-208&scope=ads"), "variant-1 image URN (value withheld) is malformed"},
		{"linkedin image urn carries a long opaque token", model.ProviderLinkedInAds, li, liCfg("urn:li:digitalmediaAsset:SECRET208abcdefghijklmnopqrstuvwxyz0123"), "variant-1 image URN (value withheld) is malformed"},
		{"linkedin image urn is a secret-bearing URL", model.ProviderLinkedInAds, li, liCfg(hostileURL), "variant-1 image URN (value withheld) is malformed"},
		{"linkedin image urn spans lines", model.ProviderLinkedInAds, li, liCfg("urn:li:image:1\nSECRET-208"), "variant-1 image URN (value withheld) is malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &noSendTransport{}
			_, err := tc.build(&http.Client{Transport: tr}).Dispatch(context.Background(), testBrief(), tc.provider, json.RawMessage(tc.config))
			if err == nil {
				t.Fatal("expected a pre-flight refusal")
			}
			if _, ok := domain.AsPreflightRefusal(err); !ok {
				t.Fatalf("expected a marked refusal, got %v", err)
			}
			got := service.DispatchFailureMessage(tc.provider, err, true)
			if !strings.Contains(got, tc.want) {
				t.Errorf("job result error = %q, want it to contain %q", got, tc.want)
			}
			for _, leak := range []string{"SECRET", "://", strings.Repeat("a", 65), "\n"} {
				if strings.Contains(got, leak) {
					t.Errorf("job result error %q contains %q from the caller's input", got, leak)
				}
			}
			if n := tr.sent.Load(); n != 0 {
				t.Errorf("%d request(s) sent before a pre-flight refusal", n)
			}
		})
	}
}

// TestDispatch_XComposedNameOverflowIsARefusal: a 200-rune event name passes X's own event-name
// bound (maxEventNameLen) yet overflows the 255-rune COMPOSED campaign name. That check runs
// before verifyAccount, so it is a pre-flight refusal and must reach the job result verbatim
// rather than as the generic string.
func TestDispatch_XComposedNameOverflowIsARefusal(t *testing.T) {
	event := strings.Repeat("e", 200)
	brief := testBrief()
	brief.EventDetails = json.RawMessage(`{"eventName":"` + event + `","registrationUrl":"https://events.example/kc","project":"cncf"}`)
	tr := &noSendTransport{}
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, twitter.WithHTTPClient(&http.Client{Transport: tr}))
	_, err := d.Dispatch(context.Background(), brief, model.ProviderTwitterAds,
		json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetId":"1234567890"}}`))
	if err == nil {
		t.Fatal("expected the composed-name refusal")
	}
	name := "Events | " + event + " | Global | Awareness | Prospecting | Promoted Post | cncf | MoFU"
	want := fmt.Sprintf("invalid campaign name: composed name is %d characters, exceeds X's 255-character limit", len([]rune(name)))
	if got := service.DispatchFailureMessage(model.ProviderTwitterAds, err, true); got != want {
		t.Errorf("job result error = %q\nwant               %q", got, want)
	}
	if n := tr.sent.Load(); n != 0 {
		t.Errorf("%d request(s) sent before the composed-name refusal", n)
	}
}

// TestDispatch_LinkedInInvalidStoredImageReachesTheJobResult: Dispatch validates a stored image
// in resolveLinkedInImages, BEFORE the client's own CreateCampaign check runs, so that earlier
// refusal must be marked too, or an invalid image reads as the generic string. The reason is
// size/shape/format only and nothing is sent to LinkedIn.
func TestDispatch_LinkedInInvalidStoredImageReachesTheJobResult(t *testing.T) {
	api := newLIImageAPI(t)
	d := newLIImageDispatcher(api, liPNGAsset(t, 300, 200), NewAssetReserver(10<<20, time.Millisecond))
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, liImageConfig(liVariant(`"imageAssetId":"`+msAssetID+`"`)))
	if camp != nil || !isNoUpstreamCreate(err) {
		t.Fatalf("want a pre-create refusal, got camp=%v err=%v", camp, err)
	}
	msg, ok := domain.AsPreflightRefusal(err)
	if !ok {
		t.Fatalf("the stored-image refusal is not marked, so the job would read the generic string: %v", err)
	}
	got := service.DispatchFailureMessage(model.ProviderLinkedInAds, err, true)
	if got != msg {
		t.Errorf("job result error = %q, want the refusal verbatim %q", got, msg)
	}
	if !strings.Contains(got, msAssetID) || !strings.Contains(got, "300x200") {
		t.Errorf("job result error %q should name the asset and its size", got)
	}
	if n := api.count(""); n != 0 {
		t.Errorf("%d request(s) reached LinkedIn before the image refusal", n)
	}
}

// TestDispatch_MicrosoftInvalidStoredImageReachesTheJobResult: the same gap on Microsoft's
// pre-create asset resolution (resolveMicrosoftImage runs ValidateImage before the client).
func TestDispatch_MicrosoftInvalidStoredImageReachesTheJobResult(t *testing.T) {
	api := &msImageAPI{}
	d := newMSImageDispatcher(t, api, liPNGAsset(t, 30, 10))
	d.SetAssetReserver(NewAssetReserver(MaxConcurrentVariantAssetBytes, 0))
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds,
		msImageConfig(`,"imageAssetId":"`+msAssetID+`","imageAltText":"KubeCon NA"`))
	if camp != nil || !isNoUpstreamCreate(err) {
		t.Fatalf("want a pre-create refusal, got camp=%v err=%v", camp, err)
	}
	msg, ok := domain.AsPreflightRefusal(err)
	if !ok {
		t.Fatalf("the stored-image refusal is not marked: %v", err)
	}
	if got := service.DispatchFailureMessage(model.ProviderMicrosoftAds, err, true); got != msg || !strings.Contains(got, "30x10") {
		t.Errorf("job result error = %q, want the refusal %q naming the image size", got, msg)
	}
	if n := api.total(); n != 0 {
		t.Errorf("%d request(s) reached Microsoft before the image refusal", n)
	}
}

// TestDispatch_LinkedInUnselectedAccountNamesTheMissingChoice pins the create path's answer to a
// connection that has credentials but no ad account chosen. Dispatch resolves through
// resolveLinkedInCredentials, which tags that state ErrAccountNotSelected, so the job result is
// the fixed connection-class sentence with reason account_not_selected — the token that tells an
// operator to use account discovery. Before, Dispatch validated inline and answered with a
// refusal that never named the choice.
//
// The claim contract is unchanged (NoUpstreamCreate, nil campaign) and nothing is sent. Both
// scopes are covered because the deferred systemScoped in the helper is what decides between
// "this project's connection" and "the LF system connection".
func TestDispatch_LinkedInUnselectedAccountNamesTheMissingChoice(t *testing.T) {
	cfg := json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]
	}}`)
	for _, tc := range []struct {
		name string
		repo func(*model.Connection) connReader
		want string
	}{
		{
			name: "project-owned row",
			repo: func(c *model.Connection) connReader { return fakeConnReader{conn: c} },
			want: "this project's linkedin-ads connection cannot be used as configured (reason: account_not_selected); nothing was sent",
		},
		{
			name: "lf system fallback",
			repo: func(c *model.Connection) connReader {
				return &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: c}}
			},
			want: "the LF system linkedin-ads connection is not usable as configured (reason: account_not_selected); nothing was sent",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := activeLinkedInConn(goodLinkedInCreds)
			conn.AccountID = "  " // whitespace-only is unselected too: the helper trims before testing
			tr := &noSendTransport{}
			d := NewLinkedInDispatcher(tc.repo(conn), identityEncryptor{}, linkedin.WithHTTPClient(&http.Client{Transport: tr}))

			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
			if camp != nil {
				t.Errorf("want no campaign for an unselected account, got %+v", camp)
			}
			if !errors.Is(err, domain.ErrAccountNotSelected) || !errors.Is(err, domain.ErrConnectionNotUsable) {
				t.Fatalf("error = %v, want ErrConnectionNotUsable + ErrAccountNotSelected", err)
			}
			if !isNoUpstreamCreate(err) {
				t.Errorf("error %v is not NoUpstreamCreate, so the dispatch claim would be retained", err)
			}
			if got := service.DispatchFailureMessage(model.ProviderLinkedInAds, err, true); got != tc.want {
				t.Errorf("job result error = %q\nwant               %q", got, tc.want)
			}
			if n := tr.sent.Load(); n != 0 {
				t.Errorf("%d request(s) sent for an account-less connection", n)
			}
		})
	}
}

// TestDispatch_LinkedInMissingOrgIDNamesTheRowItRead pins WHOSE connection the create path's
// missing-org-id refusal names. The org id is checked inline after resolveLinkedInCredentials,
// so it never passes through systemScoped; before this test it always said the PROJECT's
// connection lacked one, even when the credentials came from the LF system fallback row — an
// operator told to fix a connection the project does not own. The refusal now reads the same
// provenance and uses the #208 connection-class sentence heads.
//
// Three rows: the project's own row; the unforced fallback (the project has no row); and the
// forced flag substituting the LF row for a project row that HAS an org id — the case where
// naming the project would be most wrong, because the project's own row is fine.
//
// Every case is still a pre-send refusal: NoUpstreamCreate (claim released), nil campaign,
// verbatim job result, zero requests.
func TestDispatch_LinkedInMissingOrgIDNamesTheRowItRead(t *testing.T) {
	cfg := json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]
	}}`)
	const (
		projectWording = "this project's linkedin-ads connection is missing its org id; nothing was sent"
		systemWording  = "the LF system linkedin-ads connection is missing its org id; nothing was sent"
	)
	noOrg := func() *model.Connection {
		c := activeLinkedInConn(goodLinkedInCreds)
		c.ProviderConfig = map[string]string{"org_id": "  "} // whitespace-only is missing too
		return c
	}
	for _, tc := range []struct {
		name   string
		forced bool
		repo   func() connReader
		want   string
	}{
		{
			name: "project-owned row",
			repo: func() connReader { return fakeConnReader{conn: noOrg()} },
			want: projectWording,
		},
		{
			name: "lf system fallback",
			repo: func() connReader {
				return &scopedConnReader{rows: map[string]*model.Connection{model.SystemProjectID: noOrg()}}
			},
			want: systemWording,
		},
		{
			name:   "forced lf system row over a complete project row",
			forced: true,
			repo: func() connReader {
				return &scopedConnReader{rows: map[string]*model.Connection{
					"cncf":                activeLinkedInConn(goodLinkedInCreds),
					model.SystemProjectID: noOrg(),
				}}
			},
			want: systemWording,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.forced {
				t.Setenv(constants.EnvForceSystemAdsAccount, "true")
			}
			tr := &noSendTransport{}
			d := NewLinkedInDispatcher(tc.repo(), identityEncryptor{}, linkedin.WithHTTPClient(&http.Client{Transport: tr}))

			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
			if camp != nil {
				t.Errorf("want no campaign for a connection with no org id, got %+v", camp)
			}
			if err == nil {
				t.Fatal("Dispatch: want a refusal for a connection with no org id, got nil")
			}
			if !isNoUpstreamCreate(err) {
				t.Errorf("error %v is not NoUpstreamCreate, so the dispatch claim would be retained", err)
			}
			if msg, ok := domain.AsPreflightRefusal(err); !ok || msg != tc.want {
				t.Errorf("refusal = %q (preflight=%v)\nwant      %q", msg, ok, tc.want)
			}
			if got := service.DispatchFailureMessage(model.ProviderLinkedInAds, err, true); got != tc.want {
				t.Errorf("job result error = %q\nwant               %q", got, tc.want)
			}
			if n := tr.sent.Load(); n != 0 {
				t.Errorf("%d request(s) sent for a connection with no org id", n)
			}
		})
	}
}
