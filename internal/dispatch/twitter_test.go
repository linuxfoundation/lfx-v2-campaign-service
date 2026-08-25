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

// TestTwitter_TweetTextIsMappedAndAuthorsCleanCreated verifies the dispatch-layer
// config plumbing for tweetText/asUserId: both must reach twitter.CampaignInput,
// and a fully successful author+promote run must persist as a clean `created`
// (not created_degraded) — the whole point of closing the manual-tweet gap.
func TestTwitter_TweetTextIsMappedAndAuthorsCleanCreated(t *testing.T) {
	var tweetHit bool
	var tweetParams string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "promotable_users"):
			_, _ = w.Write([]byte(`{"data":[{"user_id":"u1"}]}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "tweet"):
			tweetHit = true
			tweetParams = r.URL.RawQuery
			_, _ = w.Write([]byte(`{"data":{"id":123456789,"id_str":"123456789"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "promoted_tweets"):
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
	cfg := json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetText":"Join us at KubeCon","asUserId":"u1"}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !tweetHit {
		t.Fatal("the adapter must POST the tweet-authoring endpoint when tweetText is configured")
	}
	if !strings.Contains(tweetParams, "as_user_id=u1") {
		t.Errorf("tweet authoring request must carry the configured asUserId, got query: %q", tweetParams)
	}
	if !strings.Contains(tweetParams, "nullcast=true") {
		t.Errorf("tweet authoring request must send nullcast=true explicitly, got query: %q", tweetParams)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("a fully authored+promoted tweet must persist as a clean %q, got %q", campaignStatusCreated, camp.Status)
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

// ---- status toggle --------------------------------------------------------

// twitterToggleCampaign builds a persisted campaign row whose Result blob uses the SAME
// key casing campaignFromTwitter produces (json.Marshal of an untagged struct → Go field
// names, i.e. "LineItemID"). Using lowerCamel here would silently yield an empty id.
func twitterToggleCampaign(campaignID, lineItemID string) *model.Campaign {
	return &model.Campaign{
		Platform:           model.ProviderTwitterAds,
		PlatformCampaignID: campaignID,
		Result:             json.RawMessage(`{"CampaignID":"` + campaignID + `","LineItemID":"` + lineItemID + `","PromotedTweetID":"pt1"}`),
	}
}

// TestTwitter_ToggleStatus_PutsEntityStatus verifies the dispatcher resolves creds and PUTs
// entity_status through the client, cascading campaign + line item in the right order.
func TestTwitter_ToggleStatus_PutsEntityStatus(t *testing.T) {
	type put struct{ method, path, status string }
	// Guarded: the handler runs on the server's goroutine while the assertions run on the
	// test's. api.Close() only synchronizes at the deferred call — AFTER the reads below — so an
	// unguarded slice is a genuine race even when -race happens not to flag it.
	var (
		mu  sync.Mutex
		got []put
	)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, put{r.Method, r.URL.Path, r.URL.Query().Get("entity_status")})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"id":"x"}}`))
	}))
	defer api.Close()

	d := NewTwitterDispatcher(
		fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(api.URL), twitter.WithAPIVersion("12"), twitter.WithWriteDelay(0),
	)
	camp := twitterToggleCampaign("cmp1", "li1")
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderTwitterAds, camp, model.CampaignRunPaused); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	// PAUSE flips the campaign gate FIRST (delivery stops now), then the line item. The
	// promoted tweet is deliberately NOT touched — the line item is X's delivery gate.
	mu.Lock()
	got = append([]put(nil), got...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("issued %d PUTs, want 2 (campaign + line item): %+v", len(got), got)
	}
	if !strings.Contains(got[0].path, "/campaigns/cmp1") || got[0].status != twitter.StatusPaused {
		t.Errorf("PUT[0] = %+v, want the campaign gate paused first", got[0])
	}
	if !strings.Contains(got[1].path, "/line_items/li1") || got[1].status != twitter.StatusPaused {
		t.Errorf("PUT[1] = %+v, want the line item paused second", got[1])
	}
	for _, g := range got {
		if strings.Contains(g.path, "promoted_tweets") {
			t.Errorf("the promoted tweet must not be toggled: %+v", g)
		}
	}
}

// TestTwitter_ToggleStatus_ActivateWithoutLineItemIsNotProvisioned pins the fail-closed
// ACTIVATE guard: with no line-item id nothing could serve, so the dispatcher refuses with
// ErrCampaignNotProvisioned (a 409 state error) WITHOUT calling X.
func TestTwitter_ToggleStatus_ActivateWithoutLineItemIsNotProvisioned(t *testing.T) {
	// Mutex-guarded: the handler goroutine writes this and the test goroutine reads it, and
	// httptest.Server.Close only synchronizes at the deferred Close — which runs AFTER the
	// assertion below.
	var mu sync.Mutex
	reached := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		reached = true
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer api.Close()

	d := NewTwitterDispatcher(
		fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(api.URL), twitter.WithAPIVersion("12"), twitter.WithWriteDelay(0),
	)
	camp := twitterToggleCampaign("cmp1", "")
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderTwitterAds, camp, model.CampaignRunActive)
	if !errors.Is(err, domain.ErrCampaignNotProvisioned) {
		t.Fatalf("want ErrCampaignNotProvisioned, got %T: %v", err, err)
	}
	mu.Lock()
	sawRequest := reached
	mu.Unlock()
	if sawRequest {
		t.Error("no API call should be made — the refusal is a local state check")
	}
}

// TestTwitter_ToggleStatus_ChildIDsMatchPersistedShape pins twitterChildIDs against the
// blob campaignFromTwitter ACTUALLY writes — json.Marshal of an untagged
// twitter.CampaignResult, i.e. Go field names ("LineItemID"). The round-trip below is the
// real guard: if the persisted shape ever changes, this fails rather than silently yielding
// "" and turning every ACTIVATE into a spurious not-provisioned 409.
//
// (Key CASING alone is not the risk: encoding/json matches object keys case-insensitively,
// so both "LineItemID" and "lineItemId" resolve. A renamed or nested FIELD would not.)
func TestTwitter_ToggleStatus_ChildIDsMatchPersistedShape(t *testing.T) {
	marshaled, err := json.Marshal(&twitter.CampaignResult{CampaignID: "cmp1", LineItemID: "li9"})
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if got := twitterChildIDs(&model.Campaign{Result: marshaled}); got != "li9" {
		t.Fatalf("twitterChildIDs over the real persisted blob = %q, want %q (blob: %s)", got, "li9", marshaled)
	}
	// A blob with no line item yields "" (drives the not-provisioned ACTIVATE refusal).
	if got := twitterChildIDs(&model.Campaign{Result: json.RawMessage(`{"CampaignID":"cmp1"}`)}); got != "" {
		t.Errorf("a blob without a line item must yield \"\", got %q", got)
	}
	// Malformed / empty blobs must not panic.
	if got := twitterChildIDs(&model.Campaign{Result: json.RawMessage(`{`)}); got != "" {
		t.Errorf("a malformed blob must yield \"\", got %q", got)
	}
	if got := twitterChildIDs(nil); got != "" {
		t.Errorf("a nil campaign must yield \"\", got %q", got)
	}
}

// TestTwitter_ToggleStatus_RejectsUnsupportedStatus keeps the run-state vocabulary closed.
func TestTwitter_ToggleStatus_RejectsUnsupportedStatus(t *testing.T) {
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{})
	err := d.ToggleStatus(context.Background(), "proj", model.ProviderTwitterAds, twitterToggleCampaign("cmp1", "li1"), "archived")
	if err == nil {
		t.Fatal("an unsupported run status must be rejected")
	}
}

// TestTwitter_ToggleStatus_UnconfirmedCrossesTheDispatcherBoundary pins the classification
// branch in ToggleStatus, which is what actually feeds BriefService's verify-vs-retry
// decision.
//
// The two ambiguous shapes reach it differently, and only one depends on the branch to
// acquire the marker at all:
//   - a first-call 5xx surfaces as a plain apiError, which does NOT implement
//     Unconfirmed(). twitter.IsOutcomeUnconfirmed recognizes it structurally (via
//     createOutcomeAmbiguous), and the dispatcher's wrap is what turns that into an error
//     carrying the behavioral marker. Delete the branch and this outcome silently
//     degrades to "not modified".
//   - a partial cascade already implements Unconfirmed() itself (client.go
//     partialCascadeError), so it would survive the branch's removal.
//
// Both are covered because the branch must classify them alike, but the 5xx row is the one
// that would regress: without it, deleting the wrap leaves every client test green while
// operators are told a possibly-applied update was "not modified".
//
// Both halves matter. A definite 4xx on the FIRST call mutated nothing and must stay
// definite, otherwise the branch could be replaced by an unconditional wrap and still pass.
func TestTwitter_ToggleStatus_UnconfirmedCrossesTheDispatcherBoundary(t *testing.T) {
	cases := []struct {
		name string
		// status returned for the first PUT the dispatcher issues; on PAUSE that is the
		// campaign gate, and the line item follows only if the gate succeeded.
		firstStatus     int
		secondStatus    int
		wantUnconfirmed bool
	}{
		// Ambiguous on the first call: X may or may not have applied the gate flip.
		{"5xx on the first put", http.StatusBadGateway, http.StatusOK, true},
		// Definite rejection, nothing mutated — must NOT be softened to "verify".
		{"4xx on the first put", http.StatusBadRequest, http.StatusOK, false},
		// Post-first-write failure: the campaign gate ALREADY paused upstream, so even a
		// definite 4xx on the line item leaves the cascade half-applied and ambiguous.
		{"4xx after the gate applied", http.StatusOK, http.StatusBadRequest, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Guarded: written on the server goroutine, read on the test goroutine, and
			// api.Close() only synchronizes at the deferred call — after the reads below.
			var (
				mu sync.Mutex
				n  int
			)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				n++
				seq := n
				mu.Unlock()
				code := tc.firstStatus
				if seq > 1 {
					code = tc.secondStatus
				}
				w.WriteHeader(code)
				if code == http.StatusOK {
					_, _ = w.Write([]byte(`{"data":{"id":"x"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"errors":[{"code":"BAD","message":"nope"}]}`))
			}))
			defer api.Close()

			d := NewTwitterDispatcher(
				fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
				twitter.WithBaseURL(api.URL), twitter.WithAPIVersion("12"), twitter.WithWriteDelay(0),
			)
			err := d.ToggleStatus(context.Background(), "proj", model.ProviderTwitterAds,
				twitterToggleCampaign("cmp1", "li1"), model.CampaignRunPaused)
			if err == nil {
				t.Fatal("expected an error from the failing cascade")
			}
			var unconf interface{ Unconfirmed() bool }
			got := errors.As(err, &unconf) && unconf.Unconfirmed()
			if got != tc.wantUnconfirmed {
				t.Errorf("Unconfirmed() = %v, want %v (err %T: %v)", got, tc.wantUnconfirmed, err, err)
			}
		})
	}
}

// TestTwitter_ToggleStatus_SucceedsWithoutFundingInstrument defends the ONE intentional
// difference between the create and toggle credential rules: Dispatch requires
// funding_instrument_id (a create-time field), validateTwitterConnection deliberately does
// not, because UpdateCampaignAndChildrenStatus only PUTs entity_status on entities that
// already exist and never puts that field on the wire. Demanding it would refuse an
// otherwise-valid pause on a connection that has drifted.
//
// TestTwitter_PreCreateErrorsReleaseClaim pins the create half ("missing funding
// instrument" is a pre-create failure); this pins the toggle half. A refactor that folds
// the funding-instrument check into the shared validator now breaks here instead of
// silently restoring the rejection this asymmetry exists to avoid.
func TestTwitter_ToggleStatus_SucceedsWithoutFundingInstrument(t *testing.T) {
	var (
		mu    sync.Mutex
		paths []string
	)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"id":"x"}}`))
	}))
	defer api.Close()

	// Identical to activeTwitterConn EXCEPT that ProviderConfig carries no
	// funding_instrument_id — the exact connection shape Dispatch rejects.
	conn := &model.Connection{
		Provider:             model.ProviderTwitterAds,
		AccountID:            "acc1",
		EncryptedCredentials: []byte(goodTwitterCreds),
		Status:               model.StatusActive,
	}
	d := NewTwitterDispatcher(
		fakeConnReader{conn: conn}, identityEncryptor{},
		twitter.WithBaseURL(api.URL), twitter.WithAPIVersion("12"), twitter.WithWriteDelay(0),
	)
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderTwitterAds,
		twitterToggleCampaign("cmp1", "li1"), model.CampaignRunPaused); err != nil {
		t.Fatalf("a pause must not require funding_instrument_id: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), paths...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("issued %d PUTs, want 2 (campaign + line item): %v", len(got), got)
	}
}

// TestTwitter_ReadMetrics_HappyPath verifies the dispatcher resolves creds,
// calls the client's GetCampaignMetrics, and returns the mapped metrics.
func TestTwitter_ReadMetrics_HappyPath(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"cmp1","id_data":[{"metrics":{"impressions":[1000],"clicks":[50],"billed_charge_local_micro":[100000000]}}]}]}`))
	}))
	defer api.Close()

	d := NewTwitterDispatcher(
		fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(api.URL), twitter.WithAPIVersion("12"), twitter.WithWriteDelay(0),
	)

	metrics, err := d.ReadMetrics(
		context.Background(), "proj", model.ProviderTwitterAds,
		twitterToggleCampaign("cmp1", "li1"),
		model.MetricsWindowLast7Days,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.CampaignID != "cmp1" {
		t.Errorf("expected campaignID cmp1, got %s", metrics.CampaignID)
	}
	if metrics.Impressions != 1000 {
		t.Errorf("expected 1000 impressions, got %d", metrics.Impressions)
	}
	if metrics.Clicks != 50 {
		t.Errorf("expected 50 clicks, got %d", metrics.Clicks)
	}
	if metrics.CostMicros != 100_000_000 {
		t.Errorf("expected 100000000 costMicros, got %d", metrics.CostMicros)
	}
	if metrics.Window != model.MetricsWindowLast7Days {
		t.Errorf("expected window last_7_days, got %s", metrics.Window)
	}
}

// TestTwitter_ReadMetrics_UnsupportedWindow verifies the dispatcher returns
// an error when the client rejects an unsupported window (e.g. LAST_30_DAYS).
func TestTwitter_ReadMetrics_UnsupportedWindow(t *testing.T) {
	d := NewTwitterDispatcher(
		fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL("https://api.example.com"), twitter.WithAPIVersion("12"),
	)

	_, err := d.ReadMetrics(
		context.Background(), "proj", model.ProviderTwitterAds,
		twitterToggleCampaign("cmp1", "li1"),
		model.MetricsWindowLast30Days, // Unsupported: exceeds 7-day limit
	)
	if err == nil {
		t.Fatal("expected error for unsupported LAST_30_DAYS window, got nil")
	}
	if !strings.Contains(err.Error(), "7 days") {
		t.Errorf("expected error mentioning 7-day API limit, got: %v", err)
	}
	if !errors.Is(err, domain.ErrMetricsWindowUnsupported) {
		t.Errorf("expected err to wrap domain.ErrMetricsWindowUnsupported (so brief.go maps it to 400), got: %v", err)
	}
}

// TestTwitter_ReadMetrics_YesterdayIsSupported verifies that YESTERDAY window
// (a single-day range within the 7-day limit) is correctly mapped and produces
// query params with the right start_time and end_time.
func TestTwitter_ReadMetrics_YesterdayIsSupported(t *testing.T) {
	var mu sync.Mutex
	var gotQuery string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotQuery = r.URL.RawQuery
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"li1","id_data":[{"metrics":{"impressions":[100],"clicks":[10],"billed_charge_local_micro":[500000]}}]}]}`))
	}))
	defer api.Close()

	d := NewTwitterDispatcher(
		fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(api.URL), twitter.WithAPIVersion("12"), twitter.WithWriteDelay(0),
	)

	metrics, err := d.ReadMetrics(
		context.Background(), "proj", model.ProviderTwitterAds,
		twitterToggleCampaign("cmp1", "li1"),
		model.MetricsWindowYesterday,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.Impressions != 100 || metrics.Clicks != 10 || metrics.CostMicros != 500000 {
		t.Errorf("expected impressions=100 clicks=10 costMicros=500000, got impressions=%d clicks=%d costMicros=%d",
			metrics.Impressions, metrics.Clicks, metrics.CostMicros)
	}

	// Verify the query contained start_time and end_time (exact values depend on fixed clock,
	// tested separately in twitter/metrics_test.go::TestGetCampaignMetrics_YesterdayQueryParams).
	mu.Lock()
	query := gotQuery
	mu.Unlock()
	if !strings.Contains(query, "start_time=") || !strings.Contains(query, "end_time=") {
		t.Errorf("expected start_time and end_time in query, got: %s", query)
	}
}

// TestTwitter_ReadMetrics_ConnectionResolutionFails verifies that connection
// resolution errors are propagated (unlike Dispatch which wraps them).
func TestTwitter_ReadMetrics_ConnectionResolutionFails(t *testing.T) {
	d := NewTwitterDispatcher(
		fakeConnReader{err: errors.New("connection not found")}, identityEncryptor{},
	)

	_, err := d.ReadMetrics(
		context.Background(), "proj", model.ProviderTwitterAds,
		twitterToggleCampaign("cmp1", "li1"),
		model.MetricsWindowLast7Days,
	)
	if err == nil {
		t.Fatal("expected error for failed connection resolution, got nil")
	}
}

// TestTwitter_ReadMetrics_ZeroCampaignActivity returns zero-value metrics
// when the server returns no data (campaign had no activity).
func TestTwitter_ReadMetrics_ZeroCampaignActivity(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer api.Close()

	d := NewTwitterDispatcher(
		fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(api.URL), twitter.WithAPIVersion("12"), twitter.WithWriteDelay(0),
	)

	metrics, err := d.ReadMetrics(
		context.Background(), "proj", model.ProviderTwitterAds,
		twitterToggleCampaign("cmp1", "li1"),
		model.MetricsWindowLast7Days,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metrics.Impressions != 0 || metrics.Clicks != 0 || metrics.CostMicros != 0 {
		t.Errorf("expected all zero-value metrics for empty campaign activity, got impressions=%d clicks=%d costMicros=%d",
			metrics.Impressions, metrics.Clicks, metrics.CostMicros)
	}
}

// X has no single conversions metric to read. Its analytics endpoint splits conversions
// across per-event-type metrics (conversion_purchases, conversion_sign_ups, …), each a JSON
// OBJECT carrying counts and sale amounts rather than a scalar, and only under the
// WEB_CONVERSION / MOBILE_CONVERSION metric groups this client does not request.
//
// The fixture includes one of those per-event objects in X's published shape, so the test
// fails if a future change starts folding them into a single count — which would require
// picking which event types count as conversions, a policy this service was never given.
func TestTwitter_ReadMetrics_ConversionsAbsentNotZero(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"cmp1","id_data":[{"metrics":{` +
			`"impressions":[1000],"clicks":[50],"billed_charge_local_micro":[100000000],` +
			`"conversion_purchases":{"metric":[4],"order_quantity":[4],"sale_amount":[null]}}}]}]}`))
	}))
	defer api.Close()

	d := NewTwitterDispatcher(
		fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(api.URL), twitter.WithAPIVersion("12"), twitter.WithWriteDelay(0),
	)
	metrics, err := d.ReadMetrics(
		context.Background(), "proj", model.ProviderTwitterAds,
		twitterToggleCampaign("cmp1", "li1"),
		model.MetricsWindowLast7Days,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics.Conversions != nil {
		t.Errorf("Conversions = %v for X, which reports conversions only as per-event-type "+
			"objects; a single count here would be invented rather than measured", *metrics.Conversions)
	}
	if metrics.Impressions != 1000 || metrics.Clicks != 50 {
		t.Errorf("the rest of the read was disturbed: %+v", metrics)
	}
}

// TestTwitter_ToggleStatus_ForeignAccountIs409AndNeverMutates pins the account-provenance
// guard on the TOGGLE path. The mutating endpoints this test covers are nested under
// /accounts/{account_id}/ (the metrics read is account-scoped too, but as the trailing-segment
// /stats/accounts/{account_id}), and campaign ids are unique only WITHIN an account, so once a
// project's connection is re-pointed the stored id addressed against the new account can
// collide with an unrelated campaign and PAUSE OR ACTIVATE something this project does not
// own. The refusal must be a non-retryable ErrCampaignAccountMismatch (409) raised before X is
// contacted, and it must sit above BOTH branches.
//
// The ACTIVATE case pins the ORDERING against the line-item guard: a foreign-account campaign
// with NO stored line item must answer the MISMATCH, not "its line item is not known". The
// latter is a fact about a campaign in a different account.
//
// Unlike google-ads/microsoft/linkedin/meta there is NO url fallback to cover: TwitterURL is
// the bare ads-manager constant, so only the explicit AccountID is checkable — which is
// exactly what twitterCreationAccountID documents.
func TestTwitter_ToggleStatus_ForeignAccountIs409AndNeverMutates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
	}{
		{"line item known", `{"CampaignID":"cmp1","LineItemID":"li1","AccountID":"acc_other"}`},
		// The ordering probe: no line item, so the not-provisioned guard would fire on
		// ACTIVATE if it ran first.
		{"no line item", `{"CampaignID":"cmp1","LineItemID":"","AccountID":"acc_other"}`},
	} {
		for _, status := range []string{model.CampaignRunPaused, model.CampaignRunActive} {
			t.Run(tc.name+"/status="+status, func(t *testing.T) {
				api := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					t.Errorf("X must not be mutated for a campaign owned by another ad account: %s %s", r.Method, r.URL.Path)
				}))
				defer api.Close()
				// activeTwitterConn resolves to account acc1; the rows record acc_other.
				d := NewTwitterDispatcher(
					fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
					twitter.WithBaseURL(api.URL), twitter.WithAPIVersion("12"), twitter.WithWriteDelay(0),
				)
				camp := &model.Campaign{
					Platform:           model.ProviderTwitterAds,
					PlatformCampaignID: "cmp1",
					Result:             json.RawMessage(tc.result),
				}
				err := d.ToggleStatus(context.Background(), "proj", model.ProviderTwitterAds, camp, status)
				if err == nil {
					t.Fatal("expected a mismatch error")
				}
				if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
					t.Errorf("error must wrap ErrCampaignAccountMismatch (409), got %T: %v", err, err)
				}
				if errors.Is(err, domain.ErrCampaignNotProvisioned) {
					t.Errorf("a foreign-account campaign must answer the mismatch, not a provisioning verdict: %v", err)
				}
				if !strings.Contains(err.Error(), "acc_other") || !strings.Contains(err.Error(), "acc1") {
					t.Errorf("error must name the created account (acc_other) and the resolved one (acc1), got %v", err)
				}
			})
		}
	}
}

// TestTwitter_ToggleStatus_MatchingOrUnknownAccountStillToggles is the guard's other half: a
// row recording the SAME account, and a row recording none at all, must still toggle. Absence
// means "unknown, proceed" — and on X that covers EVERY row written before the AccountID field
// existed, because there is no URL fallback to recover the account from.
func TestTwitter_ToggleStatus_MatchingOrUnknownAccountStillToggles(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
	}{
		{"matching AccountID", `{"CampaignID":"cmp1","LineItemID":"li1","AccountID":"acc1"}`},
		{"no provenance recorded", `{"CampaignID":"cmp1","LineItemID":"li1"}`},
		{"unparseable result blob", `not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var puts int
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					mu.Lock()
					puts++
					mu.Unlock()
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"data":{"id":"x"}}`))
			}))
			defer api.Close()
			d := NewTwitterDispatcher(
				fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
				twitter.WithBaseURL(api.URL), twitter.WithAPIVersion("12"), twitter.WithWriteDelay(0),
			)
			camp := &model.Campaign{
				Platform:           model.ProviderTwitterAds,
				PlatformCampaignID: "cmp1",
				Result:             json.RawMessage(tc.result),
			}
			if err := d.ToggleStatus(context.Background(), "proj", model.ProviderTwitterAds, camp, model.CampaignRunPaused); err != nil {
				t.Fatalf("ToggleStatus must proceed for a matching/unknown account, got %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			// The guard let the mutation THROUGH. Asserting only "no error" would also pass
			// if the toggle silently did nothing.
			if puts == 0 {
				t.Error("a matching/unknown-account campaign must actually be toggled, but no PUT reached X")
			}
		})
	}
}

// TestTwitter_ReadMetrics_ForeignAccountIs409AndNeverQueries pins the same guard on the READ
// path: the stats endpoint is nested under /accounts/{account_id}/, so the stored campaign id
// read under a re-pointed connection returns either nothing — a false "no data" — or an
// unrelated campaign's numbers rendered as this campaign's measurement.
func TestTwitter_ReadMetrics_ForeignAccountIs409AndNeverQueries(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("X must not be queried for a campaign owned by another ad account: %s %s", r.Method, r.URL.Path)
	}))
	defer api.Close()
	d := NewTwitterDispatcher(
		fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(api.URL), twitter.WithAPIVersion("12"), twitter.WithWriteDelay(0),
	)
	camp := &model.Campaign{
		Platform:           model.ProviderTwitterAds,
		PlatformCampaignID: "cmp1",
		Result:             json.RawMessage(`{"CampaignID":"cmp1","LineItemID":"li1","AccountID":"acc_other"}`),
	}
	got, err := d.ReadMetrics(context.Background(), "proj", model.ProviderTwitterAds, camp, model.MetricsWindowLast7Days)
	if err == nil {
		t.Fatal("expected a mismatch error")
	}
	if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
		t.Errorf("error must wrap ErrCampaignAccountMismatch (409), got %T: %v", err, err)
	}
	if got != nil {
		t.Errorf("a refused read must return no metrics, got %+v", got)
	}
	if !strings.Contains(err.Error(), "acc_other") || !strings.Contains(err.Error(), "acc1") {
		t.Errorf("error must name the created account (acc_other) and the resolved one (acc1), got %v", err)
	}
}

// TestTwitter_ReadMetrics_MatchingOrUnknownAccountStillReads is the read guard's other half: a
// row that cannot PROVE a mismatch must still be read.
func TestTwitter_ReadMetrics_MatchingOrUnknownAccountStillReads(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
	}{
		{"matching AccountID", `{"CampaignID":"cmp1","LineItemID":"li1","AccountID":"acc1"}`},
		{"no provenance recorded", `{"CampaignID":"cmp1","LineItemID":"li1"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var queried bool
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				queried = true
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"data":[{"id":"cmp1","id_data":[{"metrics":{"impressions":[1000],"clicks":[50],"billed_charge_local_micro":[100000000]}}]}]}`))
			}))
			defer api.Close()
			d := NewTwitterDispatcher(
				fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
				twitter.WithBaseURL(api.URL), twitter.WithAPIVersion("12"), twitter.WithWriteDelay(0),
			)
			camp := &model.Campaign{
				Platform:           model.ProviderTwitterAds,
				PlatformCampaignID: "cmp1",
				Result:             json.RawMessage(tc.result),
			}
			got, err := d.ReadMetrics(context.Background(), "proj", model.ProviderTwitterAds, camp, model.MetricsWindowLast7Days)
			if err != nil {
				t.Fatalf("ReadMetrics must proceed for a matching/unknown account, got %v", err)
			}
			if got == nil || got.Impressions != 1000 {
				t.Errorf("want the platform's metrics (1000 impressions), got %+v", got)
			}
			mu.Lock()
			defer mu.Unlock()
			if !queried {
				t.Error("a matching/unknown-account campaign must actually be read, but X was never queried")
			}
		})
	}
}

// TestTwitter_DispatchStampsCreatingAccount closes the loop the guard depends on: it drives a
// REAL create through the client and asserts the persisted Result blob records the account,
// readable by the very function the guard calls.
//
// Without this the guard is untestably inert on X: twitterCreationAccountID has no URL
// fallback (TwitterURL is a bare constant), so if the create path stopped stamping AccountID
// every row would answer "unknown, proceed" and the mismatch could never fire — while every
// hand-written-blob guard test kept passing. Asserting through twitterCreationAccountID rather
// than a literal key also pins reader and writer to the SAME persisted shape, the way
// TestTwitter_ToggleStatus_ChildIDsMatchPersistedShape does for the line item.
func TestTwitter_DispatchStampsCreatingAccount(t *testing.T) {
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
	// activeTwitterConn resolves to account acc1, so that is what a created row must record.
	if got := twitterCreationAccountID(camp); got != "acc1" {
		t.Errorf("a created row must record its creating account: twitterCreationAccountID = %q, want %q (blob: %s)", got, "acc1", camp.Result)
	}
}
