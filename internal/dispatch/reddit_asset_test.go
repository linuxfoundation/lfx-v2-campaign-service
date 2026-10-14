// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/assetsign"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
)

const (
	raBrief = "11111111-2222-4333-8444-555555555555"
	raAsset = "66666666-7777-4888-9999-aaaaaaaaaaaa"
)

type fakeAssetSizer struct {
	sizes map[[3]string]int64
	calls atomic.Int32
}

func (f *fakeAssetSizer) GetAssetSize(_ context.Context, p, b, a string) (int64, error) {
	f.calls.Add(1)
	if n, ok := f.sizes[[3]string{p, b, a}]; ok {
		return n, nil
	}
	return 0, domain.ErrNotFound
}

func assetBrief() *model.CampaignBrief {
	b := testBrief()
	b.ID = raBrief
	return b
}

func testAssetSigner(t *testing.T) *assetsign.Signer {
	t.Helper()
	s, err := assetsign.New(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", 32))), "https://lfx-api.example.org", "")
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return s.WithClock(func() time.Time { return time.Unix(1_800_000_000, 0) })
}

// redditAssetHarness is a fake Reddit API that records every call and the media_url the
// promoted post was authored with.
type redditAssetHarness struct {
	calls    atomic.Int32
	mu       sync.Mutex
	mediaURL string
	// postsFail makes the post create answer 400 with a body echoing media_url, the shape that
	// would carry a signature into persisted steps.
	postsFail bool
	// onVerify runs inside the account-verification GET, the step before post authoring.
	onVerify func()
	// verified reports whether that GET has completed.
	verified atomic.Bool
}

func (h *redditAssetHarness) dispatcher(t *testing.T) *RedditDispatcher {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.calls.Add(1)
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/ad_accounts/t2_acct"):
			if h.onVerify != nil {
				h.onVerify()
			}
			h.verified.Store(true)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_acct", "name": "acct"}})
		case strings.HasSuffix(r.URL.Path, "/posts"):
			if !h.verified.Load() {
				t.Error("the post was authored before the account was verified")
			}
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Data struct {
					Content []struct {
						MediaURL string `json:"media_url"`
					} `json:"content"`
				} `json:"data"`
			}
			_ = json.Unmarshal(body, &req)
			if len(req.Data.Content) > 0 {
				h.mu.Lock()
				h.mediaURL = req.Data.Content[0].MediaURL
				h.mu.Unlock()
			}
			if h.postsFail {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "could not fetch media_url " + h.mediaURL})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t3_post"}})
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "cmp_123"}})
		case strings.Contains(r.URL.Path, "ad_groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ad_1"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		}
	}))
	t.Cleanup(api.Close)
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tok.Close)
	return NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
}

func redditAssetCfg(extra string) json.RawMessage {
	return json.RawMessage(`{"redditConfig":{"budgetUsd":50,"startDate":"2099-08-01","endDate":"2099-08-31","objective":"traffic","subreddits":["kubernetes"],"variants":[{"headline":"Join us"}]` + extra + `}}`)
}

func ownedSizer() *fakeAssetSizer {
	return &fakeAssetSizer{sizes: map[[3]string]int64{{"cncf", raBrief, raAsset}: 1024}}
}

func requirePreCreate(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal")
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Fatalf("refusal must be pre-create (claim released), got %T: %v", err, err)
	}
}

func TestRedditAsset_StoredAssetBecomesSignedMediaURL(t *testing.T) {
	h := &redditAssetHarness{}
	d := h.dispatcher(t)
	signer := testAssetSigner(t)
	d.SetCreativeAssetURLs(ownedSizer(), signer)

	camp, err := d.Dispatch(context.Background(), assetBrief(), model.ProviderRedditAds, redditAssetCfg(`,"imageAssetId":"`+strings.ToUpper(raAsset)+`"`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Fatalf("status = %q, want created", camp.Status)
	}
	u, perr := url.Parse(h.mediaURL)
	if perr != nil || !strings.HasPrefix(h.mediaURL, "https://lfx-api.example.org"+assetsign.RoutePrefix) {
		t.Fatalf("media_url %q is not a signed asset URL", h.mediaURL)
	}
	segs := strings.Split(strings.TrimPrefix(u.Path, assetsign.RoutePrefix), "/")
	exp, _ := strconv.ParseInt(u.Query().Get("exp"), 10, 64)
	if len(segs) != 3 || segs[1] != raBrief || segs[2] != raAsset ||
		!signer.Verify(segs[0], segs[1], segs[2], exp, u.Query().Get("sig")) {
		t.Fatalf("media_url %q does not verify for (cncf, brief, asset)", h.mediaURL)
	}
	// The persisted row must not carry the minted URL (a bearer credential) anywhere.
	sig := u.Query().Get("sig")
	if strings.Contains(string(camp.ConfigSnapshot), sig) || strings.Contains(string(camp.Result), sig) {
		t.Fatal("the signature leaked into the persisted campaign row")
	}
	if !strings.Contains(string(camp.ConfigSnapshot), "imageAssetId") {
		t.Fatalf("snapshot should record the asset reference, got %s", camp.ConfigSnapshot)
	}
}

func TestRedditAsset_ImageURLPathUnchanged(t *testing.T) {
	h := &redditAssetHarness{}
	d := h.dispatcher(t)
	d.SetCreativeAssetURLs(ownedSizer(), testAssetSigner(t))
	const img = "https://events.linuxfoundation.org/banner.jpg"
	if _, err := d.Dispatch(context.Background(), assetBrief(), model.ProviderRedditAds, redditAssetCfg(`,"imageUrl":"`+img+`"`)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if h.mediaURL != img {
		t.Fatalf("media_url = %q, want the caller's imageUrl", h.mediaURL)
	}
}

func TestRedditAsset_NeitherKeepsTheNoImagePath(t *testing.T) {
	h := &redditAssetHarness{}
	d := h.dispatcher(t)
	d.SetCreativeAssetURLs(ownedSizer(), testAssetSigner(t))
	if _, err := d.Dispatch(context.Background(), assetBrief(), model.ProviderRedditAds, redditAssetCfg("")); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if h.mediaURL != "" {
		t.Fatalf("no image was asked for, but a post was authored with %q", h.mediaURL)
	}
}

// TestRedditAsset_RefusedBeforeAnyUpstreamCall covers every refusal: each must be pre-create
// AND must have made no Reddit call at all.
func TestRedditAsset_RefusedBeforeAnyUpstreamCall(t *testing.T) {
	foreign := &fakeAssetSizer{sizes: map[[3]string]int64{{"cncf", "99999999-2222-4333-8444-555555555555", raAsset}: 1024}}
	empty := &fakeAssetSizer{sizes: map[[3]string]int64{{"cncf", raBrief, raAsset}: 0}}
	cases := []struct {
		name    string
		sizer   *fakeAssetSizer
		enabled bool
		extra   string
		want    string
	}{
		{"both imageUrl and imageAssetId", ownedSizer(), true, `,"imageUrl":"https://x.example/a.png","imageAssetId":"` + raAsset + `"`, "exactly one of imageUrl or imageAssetId"},
		{"imageAssetId with postUrl", ownedSizer(), true, `,"postUrl":"t3_abc123","imageAssetId":"` + raAsset + `"`, "cannot be combined with postUrl"},
		{"feature off", ownedSizer(), false, `,"imageAssetId":"` + raAsset + `"`, "stored creative assets are not enabled for Reddit; supply imageUrl"},
		{"malformed asset id", ownedSizer(), true, `,"imageAssetId":"../../etc/passwd"`, "not a valid asset id"},
		{"foreign asset", foreign, true, `,"imageAssetId":"` + raAsset + `"`, "does not exist for this brief"},
		{"missing asset", &fakeAssetSizer{}, true, `,"imageAssetId":"` + raAsset + `"`, "does not exist for this brief"},
		{"asset without bytes", empty, true, `,"imageAssetId":"` + raAsset + `"`, "no stored image bytes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &redditAssetHarness{}
			d := h.dispatcher(t)
			var signer *assetsign.Signer
			if tc.enabled {
				signer = testAssetSigner(t)
			}
			d.SetCreativeAssetURLs(tc.sizer, signer)
			camp, err := d.Dispatch(context.Background(), assetBrief(), model.ProviderRedditAds, redditAssetCfg(tc.extra))
			requirePreCreate(t, err)
			if camp != nil {
				t.Fatalf("a refusal must return no campaign, got %+v", camp)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not say %q", err, tc.want)
			}
			if n := h.calls.Load(); n != 0 {
				t.Fatalf("%d Reddit calls were made before the refusal", n)
			}
		})
	}
}

func TestRedditAsset_PreflightRefusesSynchronously(t *testing.T) {
	d := NewRedditDispatcher(fakeConnReader{}, identityEncryptor{})
	// No asset referenced: nothing to check.
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditAssetCfg("")); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("no asset: err = %v, want ErrPreflightNotApplicable", err)
	}
	// Feature off: the caller gets the 400 sentinel with the actionable message.
	err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditAssetCfg(`,"imageAssetId":"`+raAsset+`"`))
	if !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), "supply imageUrl") {
		t.Fatalf("feature off: err = %v, want ErrCreateConfigInvalid naming imageUrl", err)
	}
	d.SetCreativeAssetURLs(ownedSizer(), testAssetSigner(t))
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditAssetCfg(`,"imageAssetId":"`+raAsset+`"`)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("enabled, valid: err = %v, want ErrPreflightNotApplicable", err)
	}
	err = d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditAssetCfg(`,"imageUrl":"https://x.example/a.png","imageAssetId":"`+raAsset+`"`))
	if !errors.Is(err, domain.ErrCreateConfigInvalid) {
		t.Fatalf("both: err = %v, want ErrCreateConfigInvalid", err)
	}
}

// TestRedditAsset_UpstreamEchoOfMediaURLNeverPersisted: when Reddit's error body echoes
// media_url, the signature must not reach the persisted row, and the classification (degraded
// success) must be unchanged. Today the Reddit client already reports only the status of a
// failed request, so this holds even without the dispatcher's scrub; the scrub
// (scrubErr/scrubSignedURL, unit-tested below) is defense in depth should the client ever start
// quoting response bodies.
func TestRedditAsset_UpstreamEchoOfMediaURLNeverPersisted(t *testing.T) {
	h := &redditAssetHarness{postsFail: true}
	d := h.dispatcher(t)
	d.SetCreativeAssetURLs(ownedSizer(), testAssetSigner(t))
	camp, err := d.Dispatch(context.Background(), assetBrief(), model.ProviderRedditAds, redditAssetCfg(`,"imageAssetId":"`+raAsset+`"`))
	if err != nil {
		t.Fatalf("the paid campaign WAS created, so the job must not fail: %v", err)
	}
	if camp.Status != campaignStatusCreatedDegraded {
		t.Fatalf("status = %q, want %q", camp.Status, campaignStatusCreatedDegraded)
	}
	u, _ := url.Parse(h.mediaURL)
	sig := u.Query().Get("sig")
	if sig == "" {
		t.Fatal("harness did not capture a signed media_url")
	}
	if strings.Contains(string(camp.Result), sig) {
		t.Fatalf("the signature survived into the persisted result: %s", camp.Result)
	}
}

func TestScrubErrPreservesIdentity(t *testing.T) {
	base := errors.New("boom https://h/p?exp=1&sig=SECRET")
	wrapped := scrubErr(base, "https://h/p?exp=1&sig=SECRET")
	if strings.Contains(wrapped.Error(), "SECRET") {
		t.Fatalf("scrubbed error still carries the signature: %v", wrapped)
	}
	if !errors.Is(wrapped, base) {
		t.Fatal("scrubbing must keep errors.Is working for classification")
	}
}

func TestScrubSignedURLRedactsTheQuery(t *testing.T) {
	const signed = "https://h/_campaigns/creative-assets/p/b/a?exp=1&sig=SECRETSECRET"
	got := scrubSignedURL("promoted-post failed: could not fetch "+signed+" (400)", signed)
	if strings.Contains(got, "SECRETSECRET") || !strings.Contains(got, "https://h/_campaigns/creative-assets/p/b/a?[signature redacted]") {
		t.Fatalf("scrubbed = %q", got)
	}
	if scrubSignedURL("unrelated", signed) != "unrelated" {
		t.Fatal("text without the URL must be unchanged")
	}
}

// TestRedditAsset_MintedAfterAccountCheckSurvivesASlowPrePostStep is the #310 review fix: the
// URL used to be minted in Dispatch, before CreateCampaign's account verification, which can
// sit out a Retry-After wait. With a short TTL the URL then expired before Reddit fetched it.
// Here the verification step advances the clock by an HOUR — far past any TTL — and the URL
// Reddit receives must still verify at the time of the post, with an expiry measured from
// AFTER the account check.
func TestRedditAsset_MintedAfterAccountCheckSurvivesASlowPrePostStep(t *testing.T) {
	var mu sync.Mutex
	clock := time.Unix(1_800_000_000, 0)
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	base := testAssetSigner(t)
	signer := base.WithClock(now)

	var verifiedAt time.Time
	h := &redditAssetHarness{onVerify: func() {
		mu.Lock()
		clock = clock.Add(time.Hour) // a slow pre-post step: throttling, token refresh, ...
		verifiedAt = clock
		mu.Unlock()
	}}
	d := h.dispatcher(t)
	d.SetCreativeAssetURLs(ownedSizer(), signer)

	if _, err := d.Dispatch(context.Background(), assetBrief(), model.ProviderRedditAds, redditAssetCfg(`,"imageAssetId":"`+raAsset+`"`)); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	u, err := url.Parse(h.mediaURL)
	if err != nil || h.mediaURL == "" {
		t.Fatalf("no signed media_url reached Reddit: %q", h.mediaURL)
	}
	exp, _ := strconv.ParseInt(u.Query().Get("exp"), 10, 64)
	if want := verifiedAt.Add(signer.TTL()).Unix(); exp != want {
		t.Fatalf("exp = %d, want %d: the URL must be minted after the account check, not before it", exp, want)
	}
	segs := strings.Split(strings.TrimPrefix(u.Path, assetsign.RoutePrefix), "/")
	if !signer.Verify(segs[0], segs[1], segs[2], exp, u.Query().Get("sig")) {
		t.Fatal("the URL Reddit received had already expired when the post was authored")
	}
}

// The URL is minted late, inside CreateCampaign, so anything that would make minting fail must
// be caught BEFORE any Reddit call: a project id a signed URL cannot carry is refused pre-create.
func TestRedditAsset_UncarriableProjectRefusedBeforeAnyUpstreamCall(t *testing.T) {
	h := &redditAssetHarness{}
	d := h.dispatcher(t)
	brief := assetBrief()
	brief.ProjectID = "cncf_legacy" // '_' is outside the signed route's project pattern
	d.SetCreativeAssetURLs(&fakeAssetSizer{sizes: map[[3]string]int64{{brief.ProjectID, raBrief, raAsset}: 1024}}, testAssetSigner(t))
	_, err := d.Dispatch(context.Background(), brief, model.ProviderRedditAds, redditAssetCfg(`,"imageAssetId":"`+raAsset+`"`))
	requirePreCreate(t, err)
	if n := h.calls.Load(); n != 0 {
		t.Fatalf("%d Reddit calls were made before the refusal", n)
	}
}
