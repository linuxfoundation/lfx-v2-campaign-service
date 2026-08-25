// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// newAuthorTweetTestServer builds an httptest server that serves the campaign +
// line item + promotable_users + tweet + promoted_tweets create/list endpoints a
// happy-path CreateCampaign run touches, recording each request it sees so tests
// can assert on what was (or was not) sent. handleTweet overrides the /tweet
// response when non-nil; otherwise it returns a fixed numeric id with its
// matching id_str, mirroring the real Ads API's legacy v1.1-shaped tweet
// object (a NUMERIC "id" — extractTweetID reads "id_str" instead).
func newAuthorTweetTestServer(t *testing.T, handleTweet http.HandlerFunc, promotableUsers string) (*httptest.Server, *int32, *url.Values) {
	t.Helper()
	var tweetCalls int32
	var tweetParams url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "promotable_users"):
			_, _ = w.Write([]byte(promotableUsers))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "line_items"):
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "tweet"):
			atomic.AddInt32(&tweetCalls, 1)
			tweetParams = r.URL.Query()
			if handleTweet != nil {
				handleTweet(w, r)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"id":123456789,"id_str":"123456789"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "promoted_tweets"):
			_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv, &tweetCalls, &tweetParams
}

func newAuthorTweetTestClient(baseURL string) *Client {
	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
		WithBaseURL(baseURL),
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime
	return c
}

func baseAuthorInput(tweetText string) CampaignInput {
	return CampaignInput{
		EventName:       "LFX Retest",
		Project:         "CNCF",
		BudgetUsd:       500,
		StartDate:       "2099-03-01",
		EndDate:         "2099-03-10",
		TweetText:       tweetText,
		RegistrationURL: "https://events.lf.org/kubecon",
	}
}

// TestCreateCampaign_AuthorsAndPromotesTweet covers the happy path: TweetID is
// empty but TweetText is supplied, exactly one promotable user is returned, the
// client authors a nullcast tweet and falls through into the existing
// promoted_tweets POST with the new tweet id.
func TestCreateCampaign_AuthorsAndPromotesTweet(t *testing.T) {
	srv, tweetCalls, tweetParams := newAuthorTweetTestServer(t, nil, `{"data":[{"user_id":"u1","promotable_user_type":"FULL"}]}`)
	defer srv.Close()

	c := newAuthorTweetTestClient(srv.URL)
	res, err := c.CreateCampaign(context.Background(), baseAuthorInput("Join us at KubeCon"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if atomic.LoadInt32(tweetCalls) != 1 {
		t.Fatalf("expected exactly 1 call to the tweet-authoring endpoint, got %d", *tweetCalls)
	}
	// nullcast must be sent EXPLICITLY, never relied on as a default.
	if got := tweetParams.Get("nullcast"); got != "true" {
		t.Errorf("nullcast param = %q, want \"true\"", got)
	}
	if got := tweetParams.Get("as_user_id"); got != "u1" {
		t.Errorf("as_user_id param = %q, want the auto-resolved single promotable user \"u1\"", got)
	}
	if !strings.Contains(tweetParams.Get("text"), "Join us at KubeCon") {
		t.Errorf("text param = %q, want it to contain the caller's tweet text", tweetParams.Get("text"))
	}
	if !strings.Contains(tweetParams.Get("text"), "https://events.lf.org/kubecon") {
		t.Errorf("text param = %q, want the destination URL appended", tweetParams.Get("text"))
	}
	if res.AuthoredTweetID != "123456789" {
		t.Errorf("AuthoredTweetID = %q, want \"123456789\"", res.AuthoredTweetID)
	}
	if res.PromotedTweetID != "pt1" {
		t.Errorf("PromotedTweetID = %q, want \"pt1\" (fall-through into the existing promote step)", res.PromotedTweetID)
	}
	if res.PromotedTweetWarning != "" {
		t.Errorf("PromotedTweetWarning = %q, want empty on a clean run", res.PromotedTweetWarning)
	}
}

// TestCreateCampaign_ExplicitTweetIDWinsOverText verifies an explicit TweetID
// always wins over TweetText: the tweet-authoring endpoint must never be hit,
// the supplied id is promoted as-is, and a step records that the text was
// ignored.
func TestCreateCampaign_ExplicitTweetIDWinsOverText(t *testing.T) {
	srv, tweetCalls, _ := newAuthorTweetTestServer(t, nil, `{"data":[{"user_id":"u1"}]}`)
	defer srv.Close()

	c := newAuthorTweetTestClient(srv.URL)
	in := baseAuthorInput("this text must be ignored")
	in.TweetID = "1234567890"
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if atomic.LoadInt32(tweetCalls) != 0 {
		t.Fatalf("explicit TweetID must win: expected 0 calls to the tweet-authoring endpoint, got %d", *tweetCalls)
	}
	if res.AuthoredTweetID != "" {
		t.Errorf("AuthoredTweetID = %q, want empty — no tweet was authored", res.AuthoredTweetID)
	}
	if res.PromotedTweetID != "pt1" {
		t.Errorf("PromotedTweetID = %q, want \"pt1\" (the explicit id promoted as-is)", res.PromotedTweetID)
	}
	var haveIgnoredStep bool
	for _, s := range res.Steps {
		if strings.Contains(s, "ignored") {
			haveIgnoredStep = true
		}
	}
	if !haveIgnoredStep {
		t.Errorf("expected a step recording that TweetText was ignored, steps = %v", res.Steps)
	}
}

// TestCreateCampaign_WeightedLengthRejectionPreMutation verifies an over-length
// tweet is rejected in the up-front validation block, before ANY mutating call
// (campaign create included) — mirrors the other clients' pre-send validation
// discipline.
func TestCreateCampaign_WeightedLengthRejectionPreMutation(t *testing.T) {
	var campaignCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "campaigns") {
			atomic.AddInt32(&campaignCalls, 1)
		}
		_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
	}))
	defer srv.Close()

	c := newAuthorTweetTestClient(srv.URL)
	in := baseAuthorInput(strings.Repeat("x", maxTweetWeightedChars+1))
	if _, err := c.CreateCampaign(context.Background(), in); err == nil {
		t.Fatal("expected an error for tweet text exceeding the weighted character cap")
	} else if !strings.Contains(err.Error(), "280") && !strings.Contains(err.Error(), "weighted") {
		t.Errorf("expected a weighted-length error, got: %v", err)
	}
	if atomic.LoadInt32(&campaignCalls) != 0 {
		t.Fatalf("an invalid tweet must be rejected before any mutating call, got %d campaign create calls", campaignCalls)
	}
}

// TestCreateCampaign_AuthorTweet2xxNoIDDegrades verifies a 2xx response with no
// tweet id in the body is treated as UNCONFIRMED (a tweet may have been
// published), not a clean success and not a definite failure — mirrors the
// promoted_tweets 2xx-no-id handling.
func TestCreateCampaign_AuthorTweet2xxNoIDDegrades(t *testing.T) {
	srv, _, _ := newAuthorTweetTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{}}`))
	}, `{"data":[{"user_id":"u1"}]}`)
	defer srv.Close()

	c := newAuthorTweetTestClient(srv.URL)
	res, err := c.CreateCampaign(context.Background(), baseAuthorInput("Join us at KubeCon"))
	if err != nil {
		t.Fatalf("an authoring degrade is non-fatal; CreateCampaign returned: %v", err)
	}
	if res.AuthoredTweetID != "" {
		t.Errorf("AuthoredTweetID = %q, want empty on a malformed 2xx", res.AuthoredTweetID)
	}
	if res.PromotedTweetID != "" {
		t.Errorf("PromotedTweetID = %q, want empty — nothing to promote without an authored id", res.PromotedTweetID)
	}
	if !strings.Contains(res.PromotedTweetWarning, "UNCONFIRMED") {
		t.Errorf("PromotedTweetWarning = %q, want it to say UNCONFIRMED", res.PromotedTweetWarning)
	}
	if !strings.Contains(res.PromotedTweetWarning, "verify") {
		t.Errorf("PromotedTweetWarning = %q, want it to instruct verifying before retrying", res.PromotedTweetWarning)
	}
}

// TestCreateCampaign_AuthorTweetAmbiguousIsUnconfirmed covers a 5xx from the
// tweet-authoring endpoint: X may have committed the publish before erroring,
// so the outcome must be reported UNCONFIRMED (verify before retry), never as a
// definite failure that invites a blind retry and a duplicate publish.
func TestCreateCampaign_AuthorTweetAmbiguousIsUnconfirmed(t *testing.T) {
	srv, _, _ := newAuthorTweetTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}, `{"data":[{"user_id":"u1"}]}`)
	defer srv.Close()

	c := newAuthorTweetTestClient(srv.URL)
	res, err := c.CreateCampaign(context.Background(), baseAuthorInput("Join us at KubeCon"))
	if err != nil {
		t.Fatalf("an authoring failure is non-fatal; CreateCampaign returned: %v", err)
	}
	if !strings.Contains(res.PromotedTweetWarning, "UNCONFIRMED") {
		t.Errorf("PromotedTweetWarning = %q, want UNCONFIRMED for an ambiguous 5xx", res.PromotedTweetWarning)
	}
	if !strings.Contains(res.PromotedTweetWarning, "delete any stray tweet") {
		t.Errorf("PromotedTweetWarning = %q, want it to warn about a possible stray published tweet", res.PromotedTweetWarning)
	}
}

// TestCreateCampaign_AuthorTweetDefiniteFailure covers a definite 4xx rejection:
// no tweet was published, so the warning must say so plainly and must NOT say
// UNCONFIRMED (that wording is reserved for outcomes that may have committed).
func TestCreateCampaign_AuthorTweetDefiniteFailure(t *testing.T) {
	srv, _, _ := newAuthorTweetTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}, `{"data":[{"user_id":"u1"}]}`)
	defer srv.Close()

	c := newAuthorTweetTestClient(srv.URL)
	res, err := c.CreateCampaign(context.Background(), baseAuthorInput("Join us at KubeCon"))
	if err != nil {
		t.Fatalf("an authoring failure is non-fatal; CreateCampaign returned: %v", err)
	}
	if strings.Contains(res.PromotedTweetWarning, "UNCONFIRMED") {
		t.Errorf("PromotedTweetWarning = %q, a definite 4xx rejection must not be marked UNCONFIRMED", res.PromotedTweetWarning)
	}
	if !strings.Contains(res.PromotedTweetWarning, "failed") {
		t.Errorf("PromotedTweetWarning = %q, want it to plainly report the failure", res.PromotedTweetWarning)
	}
}

// TestCreateCampaign_AuthorTweetPreSendDialFailure proves a pre-send dial
// failure (the tweet-authoring request never reached X) is reported as a
// definite, safe-to-retry failure — not UNCONFIRMED — mirroring the reddit
// client's postsToDeadPortTransport precedent.
func TestCreateCampaign_AuthorTweetPreSendDialFailure(t *testing.T) {
	deadURL := "http://127.0.0.1:1"
	srv, _, _ := newAuthorTweetTestServer(t, nil, `{"data":[{"user_id":"u1"}]}`)
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
		WithBaseURL(srv.URL),
		WithWriteDelay(0),
		WithHTTPClient(&http.Client{Transport: &tweetToDeadPortTransport{base: http.DefaultTransport, deadURL: deadURL}}),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	res, err := c.CreateCampaign(context.Background(), baseAuthorInput("Join us at KubeCon"))
	if err != nil {
		t.Fatalf("an authoring failure is non-fatal; CreateCampaign returned: %v", err)
	}
	if strings.Contains(res.PromotedTweetWarning, "UNCONFIRMED") {
		t.Errorf("PromotedTweetWarning = %q, a proven pre-send dial failure must not be UNCONFIRMED", res.PromotedTweetWarning)
	}
	if res.AuthoredTweetID != "" {
		t.Errorf("AuthoredTweetID = %q, want empty — the request never reached X", res.AuthoredTweetID)
	}
}

// tweetToDeadPortTransport redirects ONLY the tweet-authoring request
// (POST .../tweet) to a port nothing listens on, so its dial is refused — a
// proven pre-send failure. Every other request passes through untouched.
type tweetToDeadPortTransport struct {
	base    http.RoundTripper
	deadURL string
}

func (t *tweetToDeadPortTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tweet") {
		dead, err := url.Parse(t.deadURL)
		if err != nil {
			return nil, err
		}
		clone := r.Clone(r.Context())
		clone.URL.Scheme = dead.Scheme
		clone.URL.Host = dead.Host
		return t.base.RoundTrip(clone)
	}
	return t.base.RoundTrip(r)
}

// TestResolvePromotableUser covers the three handle-resolution shapes: a pinned
// id absent from the account's promotable users is refused; exactly one
// candidate is auto-used; several candidates with none pinned are refused and
// named rather than guessed at.
func TestResolvePromotableUser(t *testing.T) {
	t.Run("pinned not in list is refused", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"user_id":"u1"}]}`))
		}))
		defer srv.Close()
		c := newAuthorTweetTestClient(srv.URL)
		if _, err := c.resolvePromotableUser(context.Background(), "u2"); err == nil {
			t.Fatal("expected an error: pinned user u2 is not among the account's promotable users")
		}
	})

	t.Run("single candidate is auto-used", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"user_id":"u1"}]}`))
		}))
		defer srv.Close()
		c := newAuthorTweetTestClient(srv.URL)
		id, err := c.resolvePromotableUser(context.Background(), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if id != "u1" {
			t.Errorf("resolvePromotableUser = %q, want the single candidate \"u1\"", id)
		}
	})

	t.Run("multiple candidates with none pinned is refused", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"user_id":"u1"},{"user_id":"u2"}]}`))
		}))
		defer srv.Close()
		c := newAuthorTweetTestClient(srv.URL)
		_, err := c.resolvePromotableUser(context.Background(), "")
		if err == nil {
			t.Fatal("expected an error: several candidates and none pinned must refuse, not guess")
		}
		if !strings.Contains(err.Error(), "u1") || !strings.Contains(err.Error(), "u2") {
			t.Errorf("error should name the candidates, got: %v", err)
		}
	})

	t.Run("no candidates is refused", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer srv.Close()
		c := newAuthorTweetTestClient(srv.URL)
		if _, err := c.resolvePromotableUser(context.Background(), ""); err == nil {
			t.Fatal("expected an error: no promotable users to author as")
		}
	})
}
