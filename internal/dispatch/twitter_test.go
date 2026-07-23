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
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

const goodTwitterCreds = `{"ConsumerKey":"ck","ConsumerSecret":"cs","AccessToken":"at","AccessTokenSecret":"ats"}`

func activeTwitterConn(creds string) *model.Connection {
	return &model.Connection{
		Provider:             model.ProviderTwitterAds,
		AccountID:            "acc1",
		EncryptedCredentials: []byte(creds),
		ProviderConfig:       map[string]string{"funding_instrument_id": "fi1"},
		Status:               model.StatusActive,
	}
}

// ---- pre-create paths -----------------------------------------------------

func TestTwitter_PreCreateErrorsReleaseClaim(t *testing.T) {
	cases := []struct {
		name string
		repo connReader
		enc  domain.Encryptor
	}{
		{"missing connection", fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{}},
		{"no stored credentials", fakeConnReader{conn: &model.Connection{Provider: model.ProviderTwitterAds, Status: model.StatusActive}}, identityEncryptor{}},
		{"decrypt fails", fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, errEncryptor{}},
		{"incomplete credentials", fakeConnReader{conn: activeTwitterConn(`{"ConsumerKey":"ck"}`)}, identityEncryptor{}},
		{"inactive connection", fakeConnReader{conn: &model.Connection{Provider: model.ProviderTwitterAds, AccountID: "acc1", EncryptedCredentials: []byte(goodTwitterCreds), ProviderConfig: map[string]string{"funding_instrument_id": "fi1"}, Status: model.StatusInactive}}, identityEncryptor{}},
		{"missing funding instrument", fakeConnReader{conn: &model.Connection{Provider: model.ProviderTwitterAds, AccountID: "acc1", EncryptedCredentials: []byte(goodTwitterCreds), Status: model.StatusActive}}, identityEncryptor{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewTwitterDispatcher(tc.repo, tc.enc)
			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, nil)
			var nuc interface{ NoUpstreamCreate() bool }
			if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
				t.Errorf("a pre-create failure must be NoUpstreamCreate, got %T: %v", err, err)
			}
		})
	}
}

func TestTwitter_BadConfigIsPreCreate(t *testing.T) {
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{})
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, json.RawMessage(`{bad`))
	var nuc interface{ NoUpstreamCreate() bool }
	if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a malformed config must be a pre-create error, got %T: %v", err, err)
	}
}

// ---- happy path through an httptest twitter ads API -----------------------

func TestTwitter_DispatchSuccessMapsResult(t *testing.T) {
	// Capture whether the promoted_tweets POST happened and what tweet id it carried,
	// so a regression that dropped the adapter's TweetID mapping (which would silently
	// create a campaign that promotes NO tweet) fails this test.
	var (
		mu               sync.Mutex
		promotedTweetHit bool
		promotedTweetReq string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "promoted_tweets"):
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			promotedTweetHit = true
			// The tweet id may arrive as a query param (tweet_ids=...) or in the body.
			promotedTweetReq = r.URL.RawQuery + " " + string(b)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	d := NewTwitterDispatcher(
		fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0),
	)
	cfg := json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetId":"1234567890"}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "cmp1" {
		t.Fatalf("adapter must map the upstream campaign id, got %+v", camp)
	}
	if camp.CampaignName == "" || len(camp.Result) == 0 {
		t.Error("campaign name + result blob should be populated")
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("clean success status = %q, want %q", camp.Status, campaignStatusCreated)
	}
	// Persistence-contract columns populated from the config (X has no lifetime flag,
	// so the budget is a daily cap → BudgetType daily; not left NULL).
	if camp.BudgetAmount == nil || *camp.BudgetAmount != 500 {
		t.Errorf("BudgetAmount = %v, want 500", camp.BudgetAmount)
	}
	if camp.BudgetType == nil || *camp.BudgetType != model.BudgetDaily {
		t.Errorf("BudgetType = %v, want daily", camp.BudgetType)
	}
	if camp.StartDate == nil || camp.StartDate.Format("2006-01-02") != "2099-03-01" {
		t.Errorf("StartDate = %v, want 2099-03-01", camp.StartDate)
	}
	if camp.EndDate == nil || camp.EndDate.Format("2006-01-02") != "2099-03-10" {
		t.Errorf("EndDate = %v, want 2099-03-10", camp.EndDate)
	}
	if len(camp.ConfigSnapshot) == 0 {
		t.Error("ConfigSnapshot should capture the validated twitter config")
	}
	// The promoted_tweets association is what actually attaches the ad creative — it
	// MUST have been called with the configured tweet id.
	mu.Lock()
	defer mu.Unlock()
	if !promotedTweetHit {
		t.Fatal("the adapter must POST promoted_tweets to attach the tweet to the line item")
	}
	if !strings.Contains(promotedTweetReq, "1234567890") {
		t.Errorf("promoted_tweets request must carry the configured tweet id 1234567890, got: %q", promotedTweetReq)
	}
}

func TestTwitter_PromotedTweetWarningSetsDegradedStatus(t *testing.T) {
	// A promoted_tweets POST failure makes the client return (result, nil) with a
	// non-empty PromotedTweetWarning — a DEGRADED success. The campaign IS created, so
	// the adapter must NOT fail the job (nil error); instead it records the campaign with
	// a `created_degraded` status so the degraded state stays visible without an
	// unrecoverable job failure.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case strings.HasSuffix(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case strings.HasSuffix(r.URL.Path, "promoted_tweets"):
			w.WriteHeader(http.StatusBadRequest) // promoted-tweet association fails
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	d := NewTwitterDispatcher(
		fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0),
	)
	cfg := json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetId":"1234567890"}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, cfg)
	// The campaign IS created, so this is not a job failure (that would mislead + be
	// unrecoverable by retry via idempotency). The degraded state is instead made
	// visible in the persisted row: a `created_degraded` status + the warning in Result.
	if err != nil {
		t.Fatalf("a degraded success must not fail the job (the campaign exists): %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "cmp1" {
		t.Fatalf("the campaign must be returned/recorded, got %+v", camp)
	}
	if camp.Status != campaignStatusCreatedDegraded {
		t.Errorf("a promoted-tweet warning must set the created_degraded status, got %q", camp.Status)
	}
}

func TestTwitter_NoTweetIDIsDegradedNotCleanCreated(t *testing.T) {
	// The manual-tweet workflow: tweetId omitted. The client skips the promoted_tweets
	// POST and returns (result, nil) with an EMPTY PromotedTweetID — the campaign exists
	// but no tweet is attached. This is SUPPORTED (must not error), but a campaign
	// promoting no tweet is not fully wired, so it must persist as created_degraded (a
	// human attaches the tweet later), not a silent clean `created`.
	var promotedTweetHit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "promoted_tweets"):
			promotedTweetHit = true // must NOT happen — no tweet id supplied
			_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	d := NewTwitterDispatcher(
		fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0),
	)
	// No tweetId in the config → manual-tweet workflow.
	cfg := json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10"}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, cfg)
	if err != nil {
		t.Fatalf("the manual-tweet workflow (no tweetId) is supported and must NOT error: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "cmp1" {
		t.Fatalf("the created campaign must still be mapped, got %+v", camp)
	}
	if promotedTweetHit {
		t.Error("no promoted_tweets POST should fire when tweetId is omitted")
	}
	if camp.Status != campaignStatusCreatedDegraded {
		t.Errorf("a campaign with no tweet attached must be created_degraded (not silently created), got %q", camp.Status)
	}
}

func TestTwitter_ReusedCampaignIsDegraded(t *testing.T) {
	// When the client REUSES an existing campaign/line item by name (the GET find-by-name
	// returns a match), it does NOT apply this request's budget/config/dates — so even
	// with a tweet that attaches, the result is a config-drift situation, not a clean
	// `created`. The client signals this via Result.Reused; the adapter must persist
	// created_degraded. The mock returns a find-by-name match by echoing the searched
	// `q` value back as the element's name (findByName matches on exact name).
	echoMatch := func(w http.ResponseWriter, r *http.Request, id string) {
		nameJSON, _ := json.Marshal(r.URL.Query().Get("q")) // exact-match name the client searched for
		_, _ = io.WriteString(w, `{"data":[{"id":"`+id+`","name":`+string(nameJSON)+`}]}`)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "line_items"):
			echoMatch(w, r, "li-existing") // existing line item → reuse
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "campaigns"):
			echoMatch(w, r, "cmp-existing") // existing campaign → reuse
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "promoted_tweets"):
			_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
		default:
			// No create POSTs should be needed (both are reused), but answer benignly.
			_, _ = w.Write([]byte(`{"data":{"id":"unexpected"}}`))
		}
	}))
	defer srv.Close()

	d := NewTwitterDispatcher(
		fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0),
	)
	cfg := json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetId":"1234567890"}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, cfg)
	if err != nil {
		t.Fatalf("a reuse (config-drift) success must NOT error: %v", err)
	}
	if camp == nil || camp.PlatformCampaignID != "cmp-existing" {
		t.Fatalf("the reused campaign id must be mapped, got %+v", camp)
	}
	if camp.Status != campaignStatusCreatedDegraded {
		t.Errorf("a reused campaign/line item (config not applied) must be created_degraded, got %q", camp.Status)
	}
}

func TestTwitter_AmbiguousCreateRetainsClaim(t *testing.T) {
	// A 5xx on the campaign POST is ambiguous → the twitter client returns a non-nil
	// name-only partial (empty CampaignID). The adapter must retain the claim (not
	// NoUpstreamCreate) and return the campaign for orphan recording.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "campaigns"):
			w.WriteHeader(http.StatusBadGateway) // ambiguous 5xx on the campaign POST
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	d := NewTwitterDispatcher(
		fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0),
	)
	cfg := json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetId":"1234567890"}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, cfg)
	if err == nil {
		t.Fatal("expected an error from an ambiguous create")
	}
	var nuc interface{ NoUpstreamCreate() bool }
	if errors.As(err, &nuc) && nuc.NoUpstreamCreate() {
		t.Error("an ambiguous create must NOT be NoUpstreamCreate — the claim must be retained")
	}
	if camp == nil {
		t.Error("an ambiguous create must return a non-nil campaign for orphan recording")
	}
}

func TestTwitter_DefiniteRejectionReleasesClaim(t *testing.T) {
	// A definite 400 on the campaign POST (nothing created) → the client returns
	// (nil, err); the adapter must wrap it NoUpstreamCreate so the claim is released.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "campaigns"):
			w.WriteHeader(http.StatusBadRequest) // definite 4xx — nothing created
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	d := NewTwitterDispatcher(
		fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0),
	)
	cfg := json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetId":"1234567890"}}`)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, cfg)
	var nuc interface{ NoUpstreamCreate() bool }
	if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Errorf("a definite campaign rejection must release the claim (NoUpstreamCreate), got %T: %v", err, err)
	}
}
