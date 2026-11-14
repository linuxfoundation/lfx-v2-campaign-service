// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// twitterVariantsCfg is a create that authors (no tweetId), plus extra.
func twitterVariantsCfg(extra string) json.RawMessage {
	return json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10"` + extra + `}}`)
}

// twitterVariantsServer answers a create that authors and promotes tweets; failTweet makes the
// n-th tweet POST answer 400. It records each status toggle PUT's path.
func twitterVariantsServer(t *testing.T, conn *model.Connection, failTweet int) (*TwitterDispatcher, *atomic.Int64, *atomic.Int64, func() []string) {
	t.Helper()
	var tweets, calls atomic.Int64
	var mu sync.Mutex
	var puts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		p := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(p, "promotable_users"):
			_, _ = w.Write([]byte(`{"data":[{"user_id":"u1","promotable_user_type":"FULL"}]}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPut:
			mu.Lock()
			puts = append(puts, p)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"data":{"id":"x","entity_status":"` + r.URL.Query().Get("entity_status") + `"}}`))
		case strings.HasSuffix(p, "/campaigns"):
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case strings.HasSuffix(p, "/line_items"):
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case strings.HasSuffix(p, "/tweet"):
			n := tweets.Add(1)
			if int(n) == failTweet {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprintf(w, `{"data":{"id_str":"%d"}}`, 1000+n)
		case strings.HasSuffix(p, "/promoted_tweets"):
			_, _ = fmt.Fprintf(w, `{"data":[{"id":"pt-%s"}]}`, r.URL.Query().Get("tweet_ids"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	d := NewTwitterDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0))
	return d, &tweets, &calls, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), puts...) }
}

// Gap 10 (#564): variants sit behind their own default-off flag (a synchronous 400 naming it,
// refused again at dispatch before any X call) and are refused synchronously when mis-shaped.
func TestTwitterTweetVariants_GateAndShape(t *testing.T) {
	t.Setenv(constants.EnvTwitterTweetVariantsEnabled, "")
	d, _, calls, _ := twitterVariantsServer(t, activeTwitterConn(goodTwitterCreds), 0)
	cfg := twitterVariantsCfg(`,"variants":[{"tweetText":"a"},{"tweetText":"b"}]`)
	requireConfigInvalid(t, d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, cfg), constants.EnvTwitterTweetVariantsEnabled)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, cfg)
	requirePreCreate(t, err)
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d X calls with the flag off; want 0", n)
	}
	for _, extra := range []string{`,"variants":[],"tweetText":"x"`, `,"variants":null,"tweetText":"x"`} {
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterVariantsCfg(extra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Errorf("%s with the flag off: %v, want not applicable", extra, err)
		}
	}

	t.Setenv(constants.EnvTwitterTweetVariantsEnabled, "true")
	for extra, want := range map[string]string{
		`,"variants":"a"`:                                     "twitterConfig.variants: must be an array",
		`,"variants":["a"]`:                                   "twitterConfig.variants: entry 0 must be a",
		`,"variants":[{"tweetText":1}]`:                       "twitterConfig.variants: entry 0 tweetText must be a string",
		`,"variants":[{"tweetText":"a","imageAssetId":"x"}]`:  "twitterConfig.variants: entry 0 has an unknown key",
		`,"variants":[{"tweetText":"a"}],"tweetId":"123"`:     "cannot be combined with tweetId",
		`,"variants":[{"tweetText":"a"}],"tweetText":"b"`:     "cannot be combined with a top-level tweetText",
		`,"variants":[{"tweetText":"a"},{"tweetText":" A "}]`: "entry 1 repeats entry 0",
		`,"variants":[{"tweetText":"1"},{"tweetText":"2"},{"tweetText":"3"},{"tweetText":"4"},{"tweetText":"5"},{"tweetText":"6"}]`: "at most 5 variants",
		`,"budgetAmount":"bad","variants":[{"tweetText":" "}]`:                                                                      "twitterConfig.variants: entry 0 tweetText is blank",
	} {
		requireConfigInvalid(t, d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterVariantsCfg(extra)), want)
	}
}

// On: every variant is authored and promoted, the row is created and records every promoted
// tweet; a variant that fails after another was published makes the row created_degraded with a
// step naming it, and the published tweets are kept.
func TestTwitterTweetVariants_DispatchRecordsEveryPromotedTweet(t *testing.T) {
	t.Setenv(constants.EnvTwitterTweetVariantsEnabled, "true")
	d, tweets, _, _ := twitterVariantsServer(t, activeTwitterConn(goodTwitterCreds), 0)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, twitterVariantsCfg(`,"variants":[{"tweetText":"Join us"},{"tweetText":"Register now"}]`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated || tweets.Load() != 2 {
		t.Fatalf("status=%q tweets=%d, want created and 2", camp.Status, tweets.Load())
	}
	var blob twitter.CampaignResult
	if err := json.Unmarshal(camp.Result, &blob); err != nil {
		t.Fatal(err)
	}
	if len(blob.PromotedTweets) != 2 || blob.PromotedTweets[1].PromotedTweetID != "pt-1002" {
		t.Fatalf("PromotedTweets = %+v", blob.PromotedTweets)
	}
	// The stored snapshot reduces a link in variant copy, as it does tweetText's.
	camp, err = d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, twitterVariantsCfg(`,"variants":[{"tweetText":"Join us https://x.test/agenda?ref=SNAPMARK"}]`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if strings.Contains(string(camp.ConfigSnapshot), "SNAPMARK") || !strings.Contains(string(camp.ConfigSnapshot), "Join us") {
		t.Errorf("config snapshot keeps the variant link's query: %s", camp.ConfigSnapshot)
	}

	d2, tweets2, _, _ := twitterVariantsServer(t, activeTwitterConn(goodTwitterCreds), 2)
	camp, err = d2.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, twitterVariantsCfg(`,"variants":[{"tweetText":"one"},{"tweetText":"two"},{"tweetText":"three"}]`))
	if err != nil {
		t.Fatalf("Dispatch: %v (a failed variant is a degraded success)", err)
	}
	if camp.Status != campaignStatusCreatedDegraded || tweets2.Load() != 3 {
		t.Fatalf("status=%q tweets=%d, want created_degraded and all 3 attempted", camp.Status, tweets2.Load())
	}
	if !strings.Contains(string(camp.Result), "Tweet variants NOT fully promoted: variant(s) 2 of 3") {
		t.Errorf("result lacks the step naming variant 2: %s", camp.Result)
	}
}

// Variants publish under a handle too, so the connection's pinned identity binds them: a caller
// naming another handle is refused before any request.
func TestTwitterTweetVariants_AsUserIDIsAuthorized(t *testing.T) {
	t.Setenv(constants.EnvTwitterTweetVariantsEnabled, "true")
	conn := activeTwitterConn(goodTwitterCreds)
	conn.ProviderConfig = map[string]string{"funding_instrument_id": "fi1", "as_user_id": "222"}
	d, _, calls, _ := twitterVariantsServer(t, conn, 0)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, twitterVariantsCfg(`,"asUserId":"333","variants":[{"tweetText":"a"}]`))
	requirePreCreate(t, err)
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d X calls; want 0", n)
	}
}

// Pause and resume act on the campaign and its ONE line item, the gate every variant's promoted
// tweet serves through: a multi-variant row toggles exactly as a single-tweet row does.
func TestTwitterTweetVariants_ToggleGatesEveryVariant(t *testing.T) {
	t.Setenv(constants.EnvTwitterTweetVariantsEnabled, "true")
	d, _, _, puts := twitterVariantsServer(t, activeTwitterConn(goodTwitterCreds), 0)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, twitterVariantsCfg(`,"variants":[{"tweetText":"a"},{"tweetText":"b"},{"tweetText":"c"}]`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got := twitterChildIDs(camp); got != "li1" {
		t.Fatalf("line item read back from a multi-variant row = %q, want li1", got)
	}
	if err := d.ToggleStatus(context.Background(), "cncf", model.ProviderTwitterAds, camp, model.CampaignRunActive); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	got := strings.Join(puts(), ",")
	if !strings.Contains(got, "/campaigns/cmp1") || !strings.Contains(got, "/line_items/li1") {
		t.Fatalf("toggle PUTs = %q, want the campaign and its line item", got)
	}
}

// An image rides on every variant's tweet, so imageAssetId with variants (and no tweetText) is not
// the "requires tweetText" refusal; without variants it still is.
func TestTwitterTweetVariants_ImageIsAcceptedWithVariants(t *testing.T) {
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{})
	cfg := twitterConfig{ImageAssetID: "11111111-2222-4333-8444-555555555555"}
	_, _, err := d.resolveTwitterImage(context.Background(), testBrief(), cfg, "", true)
	if err == nil || strings.Contains(err.Error(), "requires tweetText") || !strings.Contains(err.Error(), "store is not configured") {
		t.Fatalf("with variants: %v, want the store refusal, not the authoring one", err)
	}
	_, _, err = d.resolveTwitterImage(context.Background(), testBrief(), cfg, "", false)
	if err == nil || !strings.Contains(err.Error(), "requires tweetText") {
		t.Fatalf("without variants: %v, want the authoring refusal", err)
	}
}
