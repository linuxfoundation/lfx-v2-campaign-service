// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// variantsFakeX answers a CreateCampaign that authors and promotes tweet variants, recording every
// write in order and the text of each authored tweet.
type variantsFakeX struct {
	mu sync.Mutex
	// failTweet / failPromote make the n-th tweet / promoted_tweets POST (1-based) answer status.
	failTweet, failPromote int
	status                 int
	// reuseLineItem makes the line-item lookup find li9; existing is its promoted_tweets list body.
	reuseLineItem bool
	existing      string
	writes        []string
	texts         []string
	mediaKeys     []string
	tweets        int
	promotes      int
}

func (f *variantsFakeX) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/accounts/acc1"):
		_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
	case r.Method == http.MethodGet && strings.HasSuffix(p, "promotable_users"):
		_, _ = w.Write([]byte(`{"data":[{"user_id":"u1","promotable_user_type":"FULL"}]}`))
	case r.Method == http.MethodGet && strings.HasSuffix(p, "promoted_tweets"):
		body := f.existing
		if body == "" {
			body = `{"data":[],"next_cursor":null}`
		}
		_, _ = w.Write([]byte(body))
	case r.Method == http.MethodGet && strings.Contains(p, "line_items") && f.reuseLineItem:
		_, _ = w.Write([]byte(`{"data":[{"id":"li9","name":"` + q.Get("q") + `"}]}`))
	case r.Method == http.MethodGet:
		_, _ = w.Write([]byte(`{"data":[]}`))
	case strings.HasSuffix(p, "/campaigns"):
		f.writes = append(f.writes, "POST campaigns")
		_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
	case strings.HasSuffix(p, "/line_items"):
		f.writes = append(f.writes, "POST line_items")
		_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
	case strings.HasSuffix(p, "/tweet"):
		f.tweets++
		f.writes = append(f.writes, fmt.Sprintf("POST tweet %d", f.tweets))
		f.texts = append(f.texts, q.Get("text"))
		f.mediaKeys = append(f.mediaKeys, q.Get("media_keys"))
		if f.tweets == f.failTweet {
			w.WriteHeader(f.status)
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":{"id_str":"%d"}}`, 1000+f.tweets)
	case strings.HasSuffix(p, "/promoted_tweets"):
		f.promotes++
		f.writes = append(f.writes, "POST promoted_tweets "+q.Get("tweet_ids"))
		if f.promotes == f.failPromote {
			w.WriteHeader(f.status)
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":[{"id":"pt%d","tweet_id":%q}]}`, f.promotes, q.Get("tweet_ids"))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *variantsFakeX) snapshot() (writes, texts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...), append([]string(nil), f.texts...)
}

func variantsClient(t *testing.T, f *variantsFakeX) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return newAuthorTweetTestClient(srv.URL)
}

func variantsInput(texts ...string) CampaignInput {
	in := baseAuthorInput("")
	in.TweetVariants = texts
	return in
}

func TestNormalizeTweetVariants(t *testing.T) {
	for _, tc := range []struct {
		name            string
		texts           []string
		tweetID, single string
		want            string
		ok              bool
	}{
		{"none", nil, "", "", "", true},
		{"trimmed", []string{" A ", "B"}, "", "", "A|B", true},
		{"with tweetId", []string{"A"}, "123", "", "cannot be combined with tweetId", false},
		{"with tweetText", []string{"A"}, "", "x", "cannot be combined with a top-level tweetText", false},
		{"too many", []string{"1", "2", "3", "4", "5", "6"}, "", "", "at most 5 variants", false},
		{"blank", []string{"A", " "}, "", "", "entry 1 tweetText is blank", false},
		{"repeat ignoring case", []string{"Join us", "JOIN US"}, "", "", "entry 1 repeats entry 0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeTweetVariants(tc.texts, tc.tweetID, tc.single)
			if !tc.ok {
				var tErr *TargetingError
				if !errors.As(err, &tErr) || tErr.Field != FieldTweetVariants || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v, want a variants TargetingError containing %q", err, tc.want)
				}
				return
			}
			if err != nil || strings.Join(got, "|") != tc.want {
				t.Fatalf("got (%v, %v), want %q", got, err, tc.want)
			}
		})
	}
}

// Every variant is authored and promoted on the one line item, in order, each with its own
// utm_content=variant-N destination and the shared image; every promoted tweet is recorded.
func TestCreateCampaign_TweetVariantsAuthorAndPromoteEach(t *testing.T) {
	f := &variantsFakeX{}
	c := variantsClient(t, f)
	res, err := c.CreateCampaign(context.Background(), variantsInput("Join us at KubeCon", "Register for KubeCon today", "See you at KubeCon"))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	writes, texts := f.snapshot()
	want := []string{"POST campaigns", "POST line_items",
		"POST tweet 1", "POST promoted_tweets 1001",
		"POST tweet 2", "POST promoted_tweets 1002",
		"POST tweet 3", "POST promoted_tweets 1003"}
	if strings.Join(writes, "\n") != strings.Join(want, "\n") {
		t.Fatalf("writes =\n%s\nwant\n%s", strings.Join(writes, "\n"), strings.Join(want, "\n"))
	}
	for i, txt := range texts {
		if !strings.Contains(txt, url.QueryEscape("utm_content")+"="+fmt.Sprintf("variant-%d", i+1)) && !strings.Contains(txt, fmt.Sprintf("utm_content=variant-%d", i+1)) {
			t.Errorf("variant %d text %q lacks utm_content=variant-%d", i+1, txt, i+1)
		}
	}
	if len(res.PromotedTweets) != 3 || res.PromotedTweetWarning != "" {
		t.Fatalf("PromotedTweets=%+v warning=%q", res.PromotedTweets, res.PromotedTweetWarning)
	}
	for i, r := range res.PromotedTweets {
		if r.Variant != i+1 || !r.Authored || r.TweetID != fmt.Sprint(1001+i) || r.PromotedTweetID != fmt.Sprintf("pt%d", i+1) || r.Warning != "" {
			t.Errorf("variant %d result %+v", i+1, r)
		}
	}
	if res.PromotedTweetID != "pt1" || res.AuthoredTweetID != "1001" {
		t.Errorf("single fields = %q/%q, want the first variant's", res.PromotedTweetID, res.AuthoredTweetID)
	}
}

// The decided outcome (#564, 2026-11-14): a later variant failing after a tweet was published
// leaves a degraded success naming the failed variant, keeps the published tweets (no rollback),
// and still runs the variants after it.
func TestCreateCampaign_FailedVariantIsNamedAndNothingRolledBack(t *testing.T) {
	for _, tc := range []struct {
		name        string
		f           *variantsFakeX
		failed      string
		unconfirmed bool
		published   int
	}{
		{"authoring refused", &variantsFakeX{failTweet: 2, status: http.StatusBadRequest}, "variant(s) 2 of 3", false, 2},
		{"authoring ambiguous", &variantsFakeX{failTweet: 2, status: http.StatusBadGateway}, "variant(s) 2 of 3", true, 2},
		{"promotion refused", &variantsFakeX{failPromote: 3, status: http.StatusBadRequest}, "variant(s) 3 of 3", false, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := variantsClient(t, tc.f)
			res, err := c.CreateCampaign(context.Background(), variantsInput("one", "two", "three"))
			if err != nil {
				t.Fatalf("CreateCampaign: %v (a variant failure is a degraded success, not an error)", err)
			}
			if !strings.Contains(res.PromotedTweetWarning, tc.failed) {
				t.Errorf("warning = %q, want it to name %q", res.PromotedTweetWarning, tc.failed)
			}
			if got := strings.Contains(res.PromotedTweetWarning, "UNCONFIRMED"); got != tc.unconfirmed {
				t.Errorf("UNCONFIRMED wording = %v, want %v: %q", got, tc.unconfirmed, res.PromotedTweetWarning)
			}
			published := 0
			for _, r := range res.PromotedTweets {
				if r.Authored {
					published++
				}
			}
			if published != tc.published || len(res.PromotedTweets) != 3 {
				t.Errorf("results = %+v, want %d published of 3 recorded", res.PromotedTweets, tc.published)
			}
			writes, _ := tc.f.snapshot()
			for _, w := range writes {
				if strings.Contains(w, "DELETE") {
					t.Fatalf("a published tweet was rolled back: %v", writes)
				}
			}
			if !strings.Contains(strings.Join(res.Steps, "\n"), "nothing was rolled back") {
				t.Errorf("steps lack the no-rollback summary: %v", res.Steps)
			}
		})
	}
}

// A reused line item: none promoted authors every variant; as many as the variants reuses them
// all and publishes nothing; any other count authors nothing.
func TestCreateCampaign_TweetVariantsOnAReusedLineItem(t *testing.T) {
	two := `{"data":[{"id":"pa","tweet_id":"501","line_item_id":"li9"},{"id":"pb","tweet_id":"502","line_item_id":"li9"}],"next_cursor":null}`
	one := `{"data":[{"id":"pa","tweet_id":"501","line_item_id":"li9"}],"next_cursor":null}`
	for _, tc := range []struct {
		name     string
		existing string
		tweets   int
		warning  string
	}{
		{"none yet", "", 2, ""},
		{"all already promoted", two, 0, ""},
		{"count mismatch", one, 0, "already promotes 1 tweets but 2 variants"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &variantsFakeX{reuseLineItem: true, existing: tc.existing}
			c := variantsClient(t, f)
			res, err := c.CreateCampaign(context.Background(), variantsInput("one", "two"))
			if err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			if f.tweets != tc.tweets {
				t.Errorf("%d tweets authored, want %d", f.tweets, tc.tweets)
			}
			if tc.warning == "" && res.PromotedTweetWarning != "" || tc.warning != "" && !strings.Contains(res.PromotedTweetWarning, tc.warning) {
				t.Errorf("warning = %q, want %q", res.PromotedTweetWarning, tc.warning)
			}
			if tc.existing == two && (len(res.PromotedTweets) != 2 || res.PromotedTweetID != "pa" || res.PromotedTweets[0].Authored) {
				t.Errorf("reuse results = %+v", res.PromotedTweets)
			}
		})
	}
}

// Variants with a tweetId, or one whose composed copy is too long, are refused before any request.
func TestCreateCampaign_BadVariantsRefusedBeforeAnyRequest(t *testing.T) {
	for name, in := range map[string]CampaignInput{
		"with tweetId": func() CampaignInput { in := variantsInput("a"); in.TweetID = "123"; return in }(),
		"too long":     variantsInput("ok", strings.Repeat("x", 300)),
		"six":          variantsInput("1", "2", "3", "4", "5", "6"),
	} {
		t.Run(name, func(t *testing.T) {
			f := &variantsFakeX{}
			c := variantsClient(t, f)
			res, err := c.CreateCampaign(context.Background(), in)
			if res != nil || !isRefusal(err) {
				t.Fatalf("res=%v err=%v, want a pre-create refusal", res, err)
			}
			if writes, _ := f.snapshot(); len(writes) != 0 {
				t.Fatalf("writes = %v, want none", writes)
			}
		})
	}
}

// An abort between writes returns the partial naming every tweet already published.
func TestCreateCampaign_VariantAbortNamesThePublishedTweets(t *testing.T) {
	f := &variantsFakeX{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := newAuthorTweetTestClient(srv.URL)
	// A 1ns pace under a frozen clock waits before every write after the first, so the wait hook
	// sees each one: campaign (no wait), line item, tweet 1, promote 1, tweet 2, promote 2. The
	// fifth wait is promote 2's: cancel there, after tweet 2 is published and before its promotion.
	c.writeDelay = time.Nanosecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := 0
	c.onPaceWait = func(context.Context, time.Duration) {
		waits++
		if waits == 5 {
			cancel()
		}
	}
	res, err := c.CreateCampaign(ctx, variantsInput("one", "two", "three"))
	if err == nil || res == nil {
		t.Fatalf("res=%v err=%v, want a partial and an error", res, err)
	}
	if !strings.Contains(err.Error(), "authored tweets 1001, 1002 PUBLISHED") {
		t.Errorf("error = %q, want both published tweets named", err.Error())
	}
	if len(res.PromotedTweets) != 2 || !res.PromotedTweets[1].Authored || res.PromotedTweets[1].PromotedTweetID != "" {
		t.Errorf("partial PromotedTweets = %+v", res.PromotedTweets)
	}
	if f.promotes != 1 {
		t.Errorf("%d promotions, want 1", f.promotes)
	}
}

// The creative image is accepted with variants (it rides on every variant's tweet): the image is
// judged on its own rules, not refused for want of a tweetText.
func TestCreateCampaign_ImageIsJudgedWithVariants(t *testing.T) {
	c := variantsClient(t, &variantsFakeX{})
	in := variantsInput("one", "two")
	in.Image = &ImageInput{MIME: "image/png"}
	_, err := c.CreateCampaign(context.Background(), in)
	if err == nil || strings.Contains(err.Error(), "requires tweetText") || !strings.Contains(err.Error(), "no image bytes") {
		t.Fatalf("err = %v, want the image's own refusal", err)
	}
}
