// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// tweetRecorder captures what the authoring endpoint saw. Both fields are written
// on the server's goroutine and read from the test goroutine, so they are returned
// through accessors that take the mutex rather than as bare pointers: a raw
// *int32/*url.Values pair puts the burden of remembering the edge on every call
// site, and the url.Values half cannot be made atomic at all.
type tweetRecorder struct {
	mu     sync.Mutex
	calls  int
	params url.Values
}

func (r *tweetRecorder) record(q url.Values) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.params = q
}

func (r *tweetRecorder) Calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// Param returns one captured query parameter. Returns "" when the endpoint was
// never called, which the Calls() assertions distinguish.
func (r *tweetRecorder) Param(k string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.params.Get(k)
}

// newAuthorTweetTestServer builds an httptest server that serves the campaign +
// line item + promotable_users + tweet + promoted_tweets create/list endpoints a
// happy-path CreateCampaign run touches, recording each request it sees so tests
// can assert on what was (or was not) sent. handleTweet overrides the /tweet
// response when non-nil; otherwise it returns a fixed numeric id with its
// matching id_str, mirroring the real Ads API's legacy v1.1-shaped tweet
// object (a NUMERIC "id" — extractTweetID reads "id_str" instead).
func newAuthorTweetTestServer(t *testing.T, handleTweet http.HandlerFunc, promotableUsers string) (*httptest.Server, *tweetRecorder) {
	t.Helper()
	rec := &tweetRecorder{}
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
			rec.record(r.URL.Query())
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
	return srv, rec
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
	srv, rec := newAuthorTweetTestServer(t, nil, `{"data":[{"user_id":"u1","promotable_user_type":"FULL"}]}`)
	defer srv.Close()

	c := newAuthorTweetTestClient(srv.URL)
	res, err := c.CreateCampaign(context.Background(), baseAuthorInput("Join us at KubeCon"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Calls() != 1 {
		t.Fatalf("expected exactly 1 call to the tweet-authoring endpoint, got %d", rec.Calls())
	}
	// nullcast must be sent EXPLICITLY, never relied on as a default.
	if got := rec.Param("nullcast"); got != "true" {
		t.Errorf("nullcast param = %q, want \"true\"", got)
	}
	if got := rec.Param("as_user_id"); got != "u1" {
		t.Errorf("as_user_id param = %q, want the auto-resolved single promotable user \"u1\"", got)
	}
	if !strings.Contains(rec.Param("text"), "Join us at KubeCon") {
		t.Errorf("text param = %q, want it to contain the caller's tweet text", rec.Param("text"))
	}
	if !strings.Contains(rec.Param("text"), "https://events.lf.org/kubecon") {
		t.Errorf("text param = %q, want the destination URL appended", rec.Param("text"))
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
	srv, rec := newAuthorTweetTestServer(t, nil, `{"data":[{"user_id":"u1"}]}`)
	defer srv.Close()

	c := newAuthorTweetTestClient(srv.URL)
	in := baseAuthorInput("this text must be ignored")
	in.TweetID = "1234567890"
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Calls() != 0 {
		t.Fatalf("explicit TweetID must win: expected 0 calls to the tweet-authoring endpoint, got %d", rec.Calls())
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
	srv, _ := newAuthorTweetTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
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
	srv, _ := newAuthorTweetTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
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
	srv, _ := newAuthorTweetTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
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
	srv, _ := newAuthorTweetTestServer(t, nil, `{"data":[{"user_id":"u1"}]}`)
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
// candidate is auto-used; several candidates with none pinned are refused
// rather than guessed at.
//
// The multiple-candidate case additionally pins what the refusal may SAY. These
// errors surface to an operator through the campaign's warning and steps, which
// are persisted and rendered — so the message carries the count, which is what
// makes it actionable, and must not carry the ids, which publishes the account's
// promotable X handles to every reader of the campaign. The assertion is written
// as an absence on purpose: naming the candidates is the easy, helpful-looking
// regression, and only a test that fails on the ids appearing will catch it.
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
		if strings.Contains(err.Error(), "u1") || strings.Contains(err.Error(), "u2") {
			t.Errorf("error leaks the promotable user ids into an operator-facing message: %v", err)
		}
		if !strings.Contains(err.Error(), "2 promotable users") {
			t.Errorf("error should give the count so the operator knows to pin one, got: %v", err)
		}
	})

	t.Run("pinned user on a later page is found", func(t *testing.T) {
		// One candidate per page. Before pagination this returned "not among this
		// account's promotable users" for u2 — a refusal derived from a list the
		// client had only read the first page of.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("cursor") == "c1" {
				_, _ = w.Write([]byte(`{"data":[{"user_id":"u2"}],"next_cursor":null}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"user_id":"u1"}],"next_cursor":"c1"}`))
		}))
		defer srv.Close()
		c := newAuthorTweetTestClient(srv.URL)
		id, err := c.resolvePromotableUser(context.Background(), "u2")
		if err != nil {
			t.Fatalf("pinned user on page 2 was not found: %v", err)
		}
		if id != "u2" {
			t.Errorf("resolvePromotableUser = %q, want %q", id, "u2")
		}
	})

	t.Run("auto-resolve counts every page before concluding", func(t *testing.T) {
		// The dangerous shape: page one holds exactly one candidate, so the
		// single-candidate shortcut would auto-pick u1 and publish under a handle
		// the caller never chose. Two candidates across two pages must refuse.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("cursor") == "c1" {
				_, _ = w.Write([]byte(`{"data":[{"user_id":"u2"}],"next_cursor":null}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"user_id":"u1"}],"next_cursor":"c1"}`))
		}))
		defer srv.Close()
		c := newAuthorTweetTestClient(srv.URL)
		id, err := c.resolvePromotableUser(context.Background(), "")
		if err == nil {
			t.Fatalf("resolvePromotableUser auto-picked %q from a paginated list of 2", id)
		}
		if !strings.Contains(err.Error(), "2 promotable users") {
			t.Errorf("error should report both pages' candidates, got: %v", err)
		}
	})

	t.Run("a repeated cursor is refused rather than looped", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"user_id":"u1"}],"next_cursor":"same"}`))
		}))
		defer srv.Close()
		c := newAuthorTweetTestClient(srv.URL)
		if _, err := c.resolvePromotableUser(context.Background(), ""); err == nil {
			t.Fatal("expected a refusal on a cursor that never advances")
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

// TestWeightedTweetLen_CountsEveryURLAtTcoWeight pins the scanning behaviour the
// 280-character pre-create gate depends on. The gate exists to reject only what X
// itself would reject, so each case below is a shape X accepts and the assertion
// is that weightedTweetLen agrees about its cost.
//
// The pre-existing rejection test uses text with no URL in it at all, so it passes
// identically against an implementation that weights only the URL the composer
// appended — which is the bug these cases are here to catch.
func TestWeightedTweetLen_CountsEveryURLAtTcoWeight(t *testing.T) {
	t.Parallel()

	const (
		longURL  = "https://events.linuxfoundation.org/kubecon-cloudnativecon-north-america/register/?utm_source=x"
		shortURL = "https://lfx.dev" // 15 runes — SHORTER than t.co's fixed 23
	)

	tests := []struct {
		name string
		text string
		want int
	}{
		{
			name: "no url is a plain rune count",
			text: "plain copy with no link at all",
			want: utf8.RuneCountInString("plain copy with no link at all"),
		},
		{
			// A caller whose own text already embeds the destination: the composer
			// skips the append entirely, so an implementation that weights only
			// what it appended counts this URL at its raw 95 runes and rejects
			// copy X would have accepted.
			name: "single embedded url counts as the t.co weight",
			text: "Register now " + longURL,
			want: utf8.RuneCountInString("Register now ") + tcoURLWeight,
		},
		{
			// X wraps EVERY link it posts, so both are discounted. An
			// implementation that handled only the first occurrence over-counts.
			name: "two urls are each counted at the t.co weight",
			text: "See " + longURL + " and " + shortURL,
			want: utf8.RuneCountInString("See ") + tcoURLWeight +
				utf8.RuneCountInString(" and ") + tcoURLWeight,
		},
		{
			// t.co is a fixed weight, not a cap: a URL shorter than 23 runes makes
			// the weighted length go UP. Asserting the direction is what stops a
			// future "optimisation" to min(raw, 23), which would under-count and
			// let genuinely over-long copy through to X.
			name: "url shorter than the t.co weight increases the count",
			text: shortURL,
			want: tcoURLWeight,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := weightedTweetLen(tc.text); got != tc.want {
				t.Fatalf("weightedTweetLen(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}

// TestBuildTwitterUTMURL_KeepsQueryAndFragment pins the deliberate divergence
// from displayTwitterUtmURL. This URL is the ad's real click destination, so the
// brief's own routing parameters have to survive alongside the generated utm_*
// set — and so does the fragment.
//
// The fragment used to be dropped here, on the reasoning that it never reaches a
// server. It reaches the PAGE: `#agenda` scrolls to and focuses that section, and a
// hash-router SPA reads the fragment as its route, so stripping it lands paid traffic
// on the front page instead. It failed silently, too — the create succeeded and every
// step we print showed a destination that looked right.
func TestBuildTwitterUTMURL_KeepsQueryAndFragment(t *testing.T) {
	t.Parallel()

	in := baseAuthorInput("")
	in.RegistrationURL = "https://events.lf.org/kubecon?ref=partner&lang=de#agenda"

	got, err := buildTwitterUTMURL(in)
	if err != nil {
		t.Fatalf("buildTwitterUTMURL: %v", err)
	}

	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.Fragment != "agenda" {
		t.Errorf("fragment not preserved: got %q, want %q, in %q", u.Fragment, "agenda", got)
	}
	// The fragment must come AFTER the query, or the UTM params land inside it and
	// never reach the server that reads them.
	if i, j := strings.Index(got, "?"), strings.Index(got, "#"); i < 0 || j < 0 || i > j {
		t.Errorf("query and fragment are out of order: %q", got)
	}
	q := u.Query()
	if q.Get("ref") != "partner" {
		t.Errorf("pre-existing query param ref dropped: %q", got)
	}
	if q.Get("lang") != "de" {
		t.Errorf("pre-existing query param lang dropped: %q", got)
	}
	if q.Get("utm_source") == "" {
		t.Errorf("utm_source not added: %q", got)
	}

	// The display form is the counterpart and must still strip the same query —
	// it is written to the unencrypted campaigns.result column, where the brief's
	// parameters are a persistence risk rather than a routing need.
	if disp := displayTwitterUtmURL(in); strings.Contains(disp, "ref=partner") {
		t.Errorf("displayTwitterUtmURL leaked the brief's query: %q", disp)
	}
}

// TestCreateCampaign_AbortBetweenAuthoringAndPromotionRetainsTweetID drives the
// one window in which a published tweet can be stranded: the pace(ctx) gate that
// runs AFTER the authoring POST has committed a tweet and BEFORE the
// promoted_tweets POST associates it. A tweet is the only irreversible artifact
// this flow creates, so the partial result has to carry its id — the campaign and
// line item are PAUSED and are found-or-created by name on a retry.
//
// Getting the cancellation to land in that window takes some care, and getting it
// wrong makes the test silently assert nothing:
//
//   - Cancelling INLINE in the /tweet handler kills the in-flight authoring POST
//     itself. That diverts into the UNCONFIRMED branch, which is non-fatal and
//     returns a nil error — so an assertion on the abort message never runs and
//     the test passes without having exercised the path at all.
//   - Cancelling from the test goroutine after the call has started races the
//     handler with no ordering at all.
//
// This test used to hit the window by timing: the handler spawned a goroutine that
// slept 50ms while the client's write delay was 500ms, betting that the authoring
// POST would finish and the following pace(ctx) would still be parked when the
// cancel landed. The bet is a good one and it is still a bet — a stalled scheduler
// or a loaded CI box can put the cancel anywhere, and every way of losing lands on
// one of the two silent-pass modes above, because nothing in the assertions can
// tell a window that was missed from a window that was hit.
//
// It now cancels from onPaceWait, which pace calls with writeMu held at the instant
// it is about to sleep out a reservation. `rec.Calls() == 1` identifies that wait as
// the one AFTER the authoring POST committed and before the promotion POST — the
// window itself, named rather than estimated. No sleeps, no write delay to
// out-wait, and the cancel cannot land anywhere else.
func TestCreateCampaign_AbortBetweenAuthoringAndPromotionRetainsTweetID(t *testing.T) {
	// Only needs to be non-zero. The old 500ms was the margin the timing bet was won
	// with; with the cancel delivered by the hook there is nothing left to out-wait,
	// and the client's clock is stubbed, so every pace before the gate still waits —
	// it just no longer costs the suite half a second per write to do it.
	const writeDelay = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, rec := newAuthorTweetTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"id":123456789,"id_str":"123456789"}}`))
	}, `{"data":[{"user_id":"u1","promotable_user_type":"FULL"}]}`)
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
		WithBaseURL(srv.URL),
		WithWriteDelay(writeDelay),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime
	c.onPaceWait = func(_ context.Context, _ time.Duration) {
		// Exactly one tweet POST has been served: the tweet exists, the promotion
		// has not been issued, and this goroutine is holding the gate between them.
		if rec.Calls() == 1 {
			cancel()
		}
	}

	result, err := c.CreateCampaign(ctx, baseAuthorInput("Join us at KubeCon"))

	if rec.Calls() != 1 {
		t.Fatalf("tweet endpoint called %d times, want exactly 1 (the tweet must have been published for this test to mean anything)", rec.Calls())
	}
	if err == nil {
		t.Fatal("expected an abort error, got nil — the run took the non-fatal degrade path instead of the pace(ctx) abort, so this test verified nothing")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("abort error does not wrap context.Canceled: %v", err)
	}
	if result == nil {
		t.Fatal("abort returned a nil result — the orchestrator's claim and the published tweet's id are both lost")
	}
	// The assertion this whole test exists for.
	if result.AuthoredTweetID != "123456789" {
		t.Errorf("AuthoredTweetID = %q, want %q — a tweet that provably exists was reported as unpublished", result.AuthoredTweetID, "123456789")
	}
	if result.PromotedTweetID != "" {
		t.Errorf("PromotedTweetID = %q, want empty (promotion never ran)", result.PromotedTweetID)
	}
	// The operator has to be told the tweet is live but unattached, by id, or the
	// only trace of it is a prose Steps entry.
	if !strings.Contains(err.Error(), "authored tweet 123456789 PUBLISHED, not yet promoted") {
		t.Errorf("abort error does not name the published tweet: %v", err)
	}
}

// TestCreateCampaign_RejectsCredentialQueryParamBeforeAnythingIsCreated covers the
// one workflow that PUBLISHES the registration URL's pre-existing query: authoring a
// tweet from TweetText builds the destination from RegistrationURL and puts it in
// the tweet body, where it is world-readable forever. A registration link pasted out
// of a logged-in browser can carry a session token in that query.
//
// Two things are asserted, and the second is the point. The create must be refused —
// and it must be refused with NOTHING created, which is why the check sits in the
// up-front validation block rather than next to the authoring call. A refusal after
// the campaign and line item exist leaves an operator to clean up; a refusal here
// costs them a corrected brief.
func TestCreateCampaign_RejectsCredentialQueryParamBeforeAnythingIsCreated(t *testing.T) {
	for _, param := range []string{"access_token", "API-KEY", "sessionId", "jwt", "pwd"} {
		t.Run(param, func(t *testing.T) {
			var writes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					writes.Add(1)
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer srv.Close()

			in := baseAuthorInput("Register now")
			in.RegistrationURL = "https://events.lf.org/kubecon?" + param + "=s3cr3t"

			c := newAuthorTweetTestClient(srv.URL)
			_, err := c.CreateCampaign(context.Background(), in)
			if err == nil {
				t.Fatalf("a registration URL carrying %q was accepted for publication", param)
			}
			// The refusal points at the parameter by the VOCABULARY WORD that
			// classified it, not by the caller's spelling — a name is free text and
			// can carry a secret itself. The operator still finds the parameter by
			// searching their own URL for that word.
			assertNamesClassifyingTerm(t, err, param)
			if strings.Contains(err.Error(), "s3cr3t") {
				t.Errorf("error echoes the credential VALUE back: %v", err)
			}
			if n := writes.Load(); n != 0 {
				t.Errorf("%d write(s) were issued before the refusal; the check must run before anything is created", n)
			}
		})
	}
}

// TestCreateCampaign_AllowsOrdinaryRegistrationQueryParams is the other half of the
// guard, and the reason it is a denylist rather than an allowlist. LF event pages
// carry real routing and attribution parameters that cannot be enumerated in
// advance; rejecting an unrecognised one would break working briefs to protect
// against nothing. `code` is here deliberately — a discount code is the common
// meaning on a registration link, and it is not a credential.
func TestCreateCampaign_AllowsOrdinaryRegistrationQueryParams(t *testing.T) {
	srv, rec := newAuthorTweetTestServer(t, nil, `{"data":[{"user_id":"u1"}]}`)
	defer srv.Close()

	in := baseAuthorInput("Register now")
	in.RegistrationURL = "https://events.lf.org/kubecon?ref=partner&lang=de&code=SAVE20&pin=4"

	c := newAuthorTweetTestClient(srv.URL)
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("an ordinary registration URL was refused: %v", err)
	}
	if rec.Calls() != 1 {
		t.Fatalf("tweet endpoint called %d times, want 1", rec.Calls())
	}
}

// TestCreateCampaign_429OnAuthoringIssuesExactlyOneRequest pins the retry decision
// that makes tweet authoring different from every other create in this package.
//
// A 429 is normally retried, and for the promoted_tweets create that is right: X
// answers a repeat with DUPLICATE_PROMOTABLE_ENTITY, which is convergence the
// SERVER performs, inside the retry loop. The campaign and line-item creates do
// NOT qualify — their find-by-name dedup runs in the caller, above the loop — and
// they are marked non-idempotent too (see the two tests below). A tweet has no
// convergence at all. X can report a 429 at OR AFTER accepting the write, so a
// retried authoring POST can publish a second tweet under the LF handle — and the
// request layer would have done it twice more before this function returned.
//
// The endpoint therefore must be hit exactly once, and the resulting failure must
// stay the non-fatal degrade (an operator is told to check X Ads Manager), not a
// hard error that loses the campaign.
func TestCreateCampaign_429OnAuthoringIssuesExactlyOneRequest(t *testing.T) {
	srv, rec := newAuthorTweetTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"errors":[{"code":"RATE_LIMIT","message":"too many requests"}]}`))
	}, `{"data":[{"user_id":"u1"}]}`)
	defer srv.Close()

	c := newAuthorTweetTestClient(srv.URL)
	res, err := c.CreateCampaign(context.Background(), baseAuthorInput("Join us at KubeCon"))
	if err != nil {
		t.Fatalf("a throttled authoring POST must degrade, not fail the campaign: %v", err)
	}
	if rec.Calls() != 1 {
		t.Fatalf("tweet endpoint called %d times, want exactly 1 — a retried 429 can publish a duplicate tweet", rec.Calls())
	}
	if res == nil || res.CampaignID == "" {
		t.Fatal("campaign id lost on the degrade path")
	}
	if res.PromotedTweetWarning == "" {
		t.Error("operator was not warned that the tweet may or may not have been published")
	}
}

// TestCreateCampaign_PacesImmediatelyBeforeAuthoring pins the ORDER of the
// promotable-user lookup and the pacer.
//
// pace RESERVES the next write slot; it does not hold one open. With the
// reservation taken first, the promotable_users GET sat inside the reservation, and
// a concurrent writer sharing this client could reserve and issue in that window —
// so the two writes landed together and rebuilt the burst the pacer exists to
// prevent. The read is unpaced and costs nothing to move, so it runs first and the
// reservation is taken immediately before the POST it spaces out.
//
// The assertion is on the sequence of observed events rather than on elapsed time:
// a duration assertion passes for the wrong reason whenever the machine is slow.
func TestCreateCampaign_PacesImmediatelyBeforeAuthoring(t *testing.T) {
	var mu sync.Mutex
	var events []string
	note := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, s)
	}

	srv, _ := newAuthorTweetTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		note("POST tweet")
		_, _ = w.Write([]byte(`{"data":{"id":1,"id_str":"1"}}`))
	}, `{"data":[{"user_id":"u1"}]}`)
	defer srv.Close()

	// A non-zero write delay is required: pace returns before it reserves anything
	// when the delay is zero, so onAdmit never fires and the test would observe an
	// ordering that does not exist.
	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
		WithBaseURL(srv.URL),
		WithWriteDelay(time.Millisecond),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime
	c.onAdmit = func(_ context.Context, _ time.Time) { note("admit") }

	// Wrap the transport so the promotable_users GET is observed at the same level
	// as the admissions, which is the only way to place one relative to the other.
	base := c.httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "promotable_users") {
			note("GET promotable_users")
		}
		return base.RoundTrip(r)
	})

	if _, err := c.CreateCampaign(context.Background(), baseAuthorInput("Join us at KubeCon")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	get, admit, post := -1, -1, -1
	for i, e := range events {
		switch {
		case e == "GET promotable_users" && get < 0:
			get = i
		case e == "POST tweet" && post < 0:
			post = i
		}
	}
	// The admission that matters is the LAST one before the tweet POST.
	for i := 0; i < post; i++ {
		if events[i] == "admit" {
			admit = i
		}
	}
	if get < 0 || admit < 0 || post < 0 {
		t.Fatalf("missing events, got %v", events)
	}
	if get >= admit || admit >= post {
		t.Errorf("want GET promotable_users -> admit -> POST tweet, got %v", events)
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestWeightedTweetLen_WeightsNonLatinAtTwo pins the half of twitter-text the
// counter used to ignore entirely: X does not count characters, it counts WEIGHT.
// Latin, digits and common punctuation weigh 1; everything outside the four
// weight-1 ranges — CJK, Cyrillic, Arabic, emoji — weighs 2. A rune count therefore
// under-counts a CJK tweet by half, and 280 CJK characters were accepted here and
// rejected by X.
//
// Direction matters in both cases, which is why the emoji rows are here. An
// UNDER-count sends copy X refuses: a wasted round trip and an operator-facing
// error. An OVER-count invents a rejection of copy X would have accepted, which no
// retry fixes and no error explains. So an emoji presentation sequence weighs 2 in
// TOTAL, not 2 per codepoint, and a country flag weighs 2 rather than 4.
func TestWeightedTweetLen_WeightsNonLatinAtTwo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want int
	}{
		{"ascii weighs one each", "abc", 3},
		{"latin-1 accents stay weight 1", "café", 4},
		{"cjk weighs two each", "日本語", 6},
		// Cyrillic, Greek, Hebrew and Arabic all sit inside twitter-text's first
		// weight-1 range ([0,4351]) — only CJK and beyond weigh 2. Asserting the
		// obvious-looking 2 here would encode a stricter counter than X's and
		// reject Russian copy X accepts.
		{"cyrillic stays weight 1", "Привет", 6},
		{"general punctuation in the weight-1 range", "–—", 2},
		{"mixed script sums per rune", "Hi 日本", 3 + 4},
		// A lone BMP symbol is NOT an emoji cluster unless emoji presentation is
		// requested with U+FE0F. Bare © is U+00A9, inside the weight-1 range; the
		// same character WITH the selector is an emoji presentation sequence and
		// weighs 2 as one cluster, not 1+2.
		{"bare copyright sign", "©", 1},
		{"copyright with emoji presentation", "©️", 2},
		{"single emoji weighs two", "🎉", 2},
		{"emoji with skin tone weighs two", "👍🏽", 2},
		{"zwj family sequence weighs two", "👨‍👩‍👧‍👦", 2},
		{"keycap sequence weighs two", "1️⃣", 2},
		{"country flag weighs two", "🇺🇸", 2},
		{"two country flags weigh two each", "🇺🇸🇯🇵", 4},
		{"emoji adjacent to text", "Hi 🎉", 3 + 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := weightedTweetLen(tc.text); got != tc.want {
				t.Fatalf("weightedTweetLen(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}

// TestCreateCampaign_RejectsOverWeightCJKText is the end-to-end half: 200 CJK
// characters are 200 runes and 400 weighted, so the pre-create gate must refuse them
// with nothing created. docs/api-catalog.md promises exactly this rejection.
func TestCreateCampaign_RejectsOverWeightCJKText(t *testing.T) {
	var writes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writes.Add(1)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	text := strings.Repeat("日", 200)
	if utf8.RuneCountInString(text) > maxTweetWeightedChars {
		t.Fatalf("test text is already over the cap by rune count (%d); it would be rejected without weighting", utf8.RuneCountInString(text))
	}

	c := newAuthorTweetTestClient(srv.URL)
	_, err := c.CreateCampaign(context.Background(), baseAuthorInput(text))
	if err == nil {
		t.Fatal("200 CJK characters weigh 400 and X refuses them; the gate accepted the text")
	}
	if n := writes.Load(); n != 0 {
		t.Errorf("%d write(s) issued before the refusal; the length gate must run before anything is created", n)
	}
}

// TestIsCredentialQueryKey pins the classifier the publication gate rests on, in
// both directions. An exact-name denylist was the original shape and it leaked:
// credential parameters COMPOSE, so `secret_token` and `access_key` are obvious
// credentials that no enumerated set happened to contain. The rejection rows are
// therefore all compounds; the acceptance rows are the collisions that make the
// fragment rules non-obvious, and they matter just as much — a false positive is a
// refused brief an operator cannot work around.
func TestIsCredentialQueryKey(t *testing.T) {
	t.Parallel()

	credential := []string{
		// Compounds that an exact-name set missed entirely.
		"secret_token", "access_key", "auth_key", "signing_key", "consumer_key",
		"csrf_token", "x_request_signature", "encryption_key",
		// The standard OAuth/OIDC parameter names.
		"oauth_token", "oauth_token_secret", "oauth_verifier", "oauth_consumer_key",
		"authorization_code", "client_assertion",
		// Spelling variants the normalizer folds onto the same name.
		"Access-Token", "API.KEY", "SESSION_ID",
	}
	for _, key := range credential {
		t.Run("rejects "+key, func(t *testing.T) {
			t.Parallel()
			if !isCredentialQueryKey(key) {
				t.Errorf("%q was not classified as a credential; it would be published verbatim", key)
			}
		})
	}

	benign := []string{
		// Ordinary routing and attribution parameters on a real LF event page.
		"ref", "lang", "utm_source", "utm_campaign", "referrer", "source",
		// Deliberately admitted: a discount code and a PIN are not credentials, and
		// refusing them would break briefs. See credentialQueryKeys' doc.
		"code", "pin", "promo_code", "discount",
		// The collisions that force `key`, `pass`, `sig` and `auth` to stay
		// exact-match rather than joining the fragment list.
		"keyword", "bypass", "design", "monkey", "keynote", "passenger",
	}
	for _, key := range benign {
		t.Run("allows "+key, func(t *testing.T) {
			t.Parallel()
			if isCredentialQueryKey(key) {
				t.Errorf("%q was refused as a credential; an ordinary brief would fail to create", key)
			}
		})
	}
}

// TestCreateCampaign_RejectsCredentialQueryParamInCallerTweetCopy covers the half of
// the exposure the registration-URL-only check could never see.
//
// composeTweetText publishes the caller's own copy verbatim next to the destination
// URL. A second link pasted into that copy is the likeliest carrier of a session
// token of all — it is the one copied straight out of a logged-in browser — and it
// reached X without passing any screen at all. Asserting zero writes is again the
// load-bearing half: the refusal has to land before the campaign and line item exist.
func TestCreateCampaign_RejectsCredentialQueryParamInCallerTweetCopy(t *testing.T) {
	for _, param := range []string{"session_token", "oauth_token", "access_key"} {
		t.Run(param, func(t *testing.T) {
			var writes atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					writes.Add(1)
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			defer srv.Close()

			// The registration URL is clean; the credential rides in the operator's copy.
			in := baseAuthorInput("Full agenda here: https://sched.lf.org/kc?" + param + "=s3cr3t")

			c := newAuthorTweetTestClient(srv.URL)
			_, err := c.CreateCampaign(context.Background(), in)
			if err == nil {
				t.Fatalf("tweet copy carrying %q was accepted for publication", param)
			}
			// The refusal points at the parameter by the VOCABULARY WORD that
			// classified it, not by the caller's spelling — a name is free text and
			// can carry a secret itself. The operator still finds the parameter by
			// searching their own URL for that word.
			assertNamesClassifyingTerm(t, err, param)
			if strings.Contains(err.Error(), "s3cr3t") {
				t.Errorf("error echoes the credential VALUE back: %v", err)
			}
			if n := writes.Load(); n != 0 {
				t.Errorf("%d write(s) were issued before the refusal; the check must run before anything is created", n)
			}
		})
	}
}

// TestWeightedTweetLen_URLRunBoundaries pins where a matched run stops being the
// link. Both directions are failures a user sees: a link counted at its raw length
// rejects valid copy before the create, and punctuation swallowed into the fixed t.co
// weight lets copy through that X then refuses once the campaign and line item exist.
func TestWeightedTweetLen_URLRunBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want int
	}{
		{
			// RFC 3986 makes the scheme case-insensitive and X wraps this too. Counted
			// at its raw 24 runes, this rejected copy X accepts.
			name: "uppercase scheme still counts as the t.co weight",
			text: "HTTPS://events.lf.org/kc",
			want: tcoURLWeight,
		},
		{
			name: "mixed-case scheme still counts as the t.co weight",
			text: "Https://events.lf.org/kc",
			want: tcoURLWeight,
		},
		{
			// The full stop ends the sentence, not the link: it is one more ordinary
			// character on top of the link's fixed weight, not free inside it.
			name: "sentence-final punctuation is not part of the link",
			text: "Register at https://lfx.dev.",
			want: utf8.RuneCountInString("Register at ") + tcoURLWeight + 1,
		},
		{
			name: "trailing comma is not part of the link",
			text: "https://lfx.dev, and more",
			want: tcoURLWeight + utf8.RuneCountInString(", and more"),
		},
		{
			// A closing bracket with no opener inside the run belongs to the prose.
			name: "unmatched closing paren is not part of the link",
			text: "(see https://lfx.dev)",
			want: utf8.RuneCountInString("(see ") + tcoURLWeight + 1,
		},
		{
			// ...but one the URL itself opened does belong to it, so it must not be
			// trimmed back off.
			name: "balanced parens inside the url are kept",
			text: "https://lf.org/a_(b)",
			want: tcoURLWeight,
		},
		{
			// The ideographic full stop is the realistic case: LF runs KubeCon China and
			// Open Source Summit Japan, CJK sentences put no space before the stop, and
			// `\S+` swallows it into the run. It weighs 2 on its own, so a byte-wise trim
			// that cannot see it undercounts by 2 at the 280 boundary.
			name: "ideographic full stop is not part of the link",
			text: "詳細 https://lfx.dev。",
			want: (2 * utf8.RuneCountInString("詳細")) + 1 + tcoURLWeight + 2,
		},
		{
			name: "ideographic comma is not part of the link",
			text: "https://lfx.dev、そして",
			want: tcoURLWeight + 2 + (2 * utf8.RuneCountInString("そして")),
		},
		{
			// <https://…> is the plain-text convention for delimiting a bare link;
			// neither bracket belongs to it.
			name: "angle brackets around a link are not part of it",
			text: "<https://lfx.dev>",
			want: 1 + tcoURLWeight + 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := weightedTweetLen(tc.text); got != tc.want {
				t.Fatalf("weightedTweetLen(%q) = %d, want %d", tc.text, got, tc.want)
			}
		})
	}
}

// TestResolvePromotableUser_PageCapIsNotAWholeList covers the bounded cousin of the
// read-page-one bug. Paginating fixed the case where the walk stopped after one page;
// it left the case where the walk stops after maxListPages with a cursor still
// outstanding, and falling out of that loop looked exactly like finishing it.
//
// The dangerous shape is the one asserted here: ONE user on the first page and more
// pages still to come. Concluding from the truncated list auto-picks that user as the
// tweet's author — the single-candidate shortcut firing on a list that was never
// enumerated — which is precisely the "never silently pick" guarantee the function
// exists to keep.
func TestResolvePromotableUser_PageCapIsNotAWholeList(t *testing.T) {
	t.Parallel()

	var pages atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := pages.Add(1)
		body := `{"data":[],"next_cursor":"` + fmt.Sprintf("c%d", n) + `"}`
		if n == 1 {
			body = `{"data":[{"user_id":"u1"}],"next_cursor":"c1"}`
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := newAuthorTweetTestClient(srv.URL)
	got, err := c.resolvePromotableUser(context.Background(), "")
	if err == nil {
		t.Fatalf("auto-resolved %q from a list that was never enumerated", got)
	}
	if !strings.Contains(err.Error(), "could not be fully enumerated") {
		t.Errorf("error should say the list was truncated, got: %v", err)
	}
	if n := pages.Load(); int(n) != maxListPages {
		t.Errorf("walked %d pages, want the full cap of %d before giving up", n, maxListPages)
	}
}

// TestResolvePromotableUser_ErrorsOmitTheUpstreamCursor guards the leak channel this
// function's errors sit on. A page cursor is opaque text decoded out of an upstream
// response body, and these errors are rendered into PromotedTweetWarning and a
// persisted steps entry — so a cursor rendered into one, or folded into the request
// path an apiError records, publishes upstream response text into the campaign record.
func TestResolvePromotableUser_ErrorsOmitTheUpstreamCursor(t *testing.T) {
	t.Parallel()

	const cursor = "CURSORSECRETVALUE"

	t.Run("a repeated cursor is reported without its value", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"user_id":"u1"}],"next_cursor":"` + cursor + `"}`))
		}))
		defer srv.Close()

		c := newAuthorTweetTestClient(srv.URL)
		_, err := c.resolvePromotableUser(context.Background(), "")
		if err == nil {
			t.Fatal("expected a refusal on a cursor that never advances")
		}
		if strings.Contains(err.Error(), cursor) {
			t.Errorf("error carries the upstream cursor into a persisted string: %v", err)
		}
	})

	t.Run("an upstream failure mid-walk is reported without the cursor", func(t *testing.T) {
		t.Parallel()
		var pages atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if pages.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"data":[{"user_id":"u1"}],"next_cursor":"` + cursor + `"}`))
				return
			}
			// The second page is fetched WITH the cursor on the wire, and fails: the
			// resulting apiError records the request path.
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		c := newAuthorTweetTestClient(srv.URL)
		_, err := c.resolvePromotableUser(context.Background(), "")
		if err == nil {
			t.Fatal("expected the upstream 500 to fail the lookup")
		}
		if strings.Contains(err.Error(), cursor) {
			t.Errorf("error carries the upstream cursor into a persisted string: %v", err)
		}
	})
}

// TestIsCredentialQueryKey_SessionIDCompounds pins the two standard spellings a web
// session cookie takes when it ends up in a URL. Both normalize to names the exact
// set never had — `jsessionid` and `aspnetsessionid` — so the exact-only entry for
// `sessionid` cleared them for publication. `sessionid` is a fragment now; bare
// `session` still is not, and the benign rows below are why.
func TestIsCredentialQueryKey_SessionIDCompounds(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"JSESSIONID", "ASP.NET_SessionId", "phpsessionid", "session-id"} {
		if !isCredentialQueryKey(key) {
			t.Errorf("%q was not classified as a credential; a live session would be published", key)
		}
	}
	for _, key := range []string{"sessionize", "sessions", "session_title", "breakout_session"} {
		if isCredentialQueryKey(key) {
			t.Errorf("%q was refused; an ordinary conference-agenda parameter would break the brief", key)
		}
	}
}

// TestIsCredentialQueryKey_CompoundComponents pins the gap the separator normalizer
// opened. Folding `-`, `_` and `.` away before matching is what lets `auth-key` and
// `ASP.NET_SessionId` be recognized — and the same fold is what turned `auth_cookie` and
// `connect.sid` into `authcookie` and `connectsid`, names that match no exact entry and
// contain no listed fragment. Both cleared the screen and would have been published
// alongside a live session. `PHPSESSID` was a third spelling of the same miss.
//
// The benign half is the part that constrains the fix. `author` and `aside` contain the
// credential words as substrings, so the repair has to be component-wise rather than a
// wider substring list — and `session_title` proves `session` stayed OUT of the component
// set: this is an events service, and a brief whose registration link carries a session
// name is normal input, not an attack.
func TestIsCredentialQueryKey_CompoundComponents(t *testing.T) {
	t.Parallel()

	for _, key := range []string{
		"auth_cookie", "connect.sid", "PHPSESSID", "session_cookie",
		"auth-ticket", "user.pwd", "AUTH",
	} {
		if !isCredentialQueryKey(key) {
			t.Errorf("%q was not classified as a credential; it would be published verbatim in the tweet", key)
		}
	}
	for _, key := range []string{
		"author", "authors", "aside", "president", "subsidy",
		"session_title", "day_pass", "utm_campaign", "keyword",
	} {
		if isCredentialQueryKey(key) {
			t.Errorf("%q was refused; an ordinary registration-link parameter would break the brief", key)
		}
	}
}

// TestRejectCredentialQueryParams_ScreensTheFragment covers the component the query
// screen never looked at. The OAuth implicit flow returns its bearer token AFTER the
// `#` — `https://app.example.org/cb#access_token=…` — so a pasted post-login URL carries
// a live token in a URL that has no query string at all, and the gate cleared it.
//
// The benign rows are the reason this is not simply "reject any fragment": `#register`
// and `#agenda-day-2` are how a brief links to a section of the registration page, and
// refusing them would fail working copy for an exposure that is not there.
func TestRejectCredentialQueryParams_ScreensTheFragment(t *testing.T) {
	t.Parallel()

	t.Run("implicit-flow token in the fragment is refused", func(t *testing.T) {
		t.Parallel()
		raw := "https://events.lf.org/cb#access_token=SECRET-abc123&token_type=bearer"
		err := rejectCredentialQueryParams(raw)
		if err == nil {
			t.Fatal("a bearer token in the fragment was cleared for publication")
		}
		if strings.Contains(err.Error(), "SECRET-abc123") {
			t.Errorf("the refusal reproduced the token it refused: %v", err)
		}
	})

	t.Run("section anchors still pass", func(t *testing.T) {
		t.Parallel()
		for _, raw := range []string{
			"https://events.lf.org/kubecon#register",
			"https://events.lf.org/kubecon#agenda-day-2",
			"https://events.lf.org/kubecon?utm_source=x#speakers",
		} {
			if err := rejectCredentialQueryParams(raw); err != nil {
				t.Errorf("rejectCredentialQueryParams(%q) refused an ordinary anchor: %v", raw, err)
			}
		}
	})
}

// TestRejectCredentialQueryParams_FailsClosedOnAnUnparseableQuery covers the hole
// url.URL.Query() opens by DISCARDING ParseQuery's error: a query Go refuses to decode
// arrives as an empty map, so the screen sees no parameters at all and clears a URL
// whose credential it never read. Go rejects a bare `;` as a separator, which is the
// shape asserted here.
func TestRejectCredentialQueryParams_FailsClosedOnAnUnparseableQuery(t *testing.T) {
	t.Parallel()

	const raw = "https://sched.lf.org/kc?ref=abc;session_token=s3cr3t"
	err := rejectCredentialQueryParams(raw)
	if err == nil {
		t.Fatal("an unparseable query was cleared for publication; the credential in it was never inspected")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("the refusal renders the credential value: %v", err)
	}
}

// TestRejectCredentialQueryParams_NamesRealKeysAndRedactsBareOnes pins the split that
// decides whether a key may be reproduced at all. A key written as `name=value` IS a
// parameter name — the secret is the value, which the parser holds separately and which
// is never rendered — so it is named, because the operator cannot find the offending
// parameter otherwise. A BARE component has no `=` behind it and is therefore not a name:
// the whole run landed in the key position and may be the credential itself, so it is
// named only as a category. Truncating it would bound the leak without redacting it.
func TestRejectCredentialQueryParams_NamesRealKeysAndRedactsBareOnes(t *testing.T) {
	t.Parallel()

	bare := "oauth_token_" + strings.Repeat("A", 300)
	err := rejectCredentialQueryParams("https://sched.lf.org/kc?" + bare)
	if err == nil {
		t.Fatal("a bare credential-shaped query component was cleared for publication")
	}
	// No PREFIX of the bare token may survive, however short — a credential prefix is
	// still credential material. 12 runes is well inside the old 40-rune bound, so this
	// assertion fails against truncation-only behaviour.
	if strings.Contains(err.Error(), bare[:12]) {
		t.Errorf("the refusal echoes part of a bare caller-controlled token: %v", err)
	}

	named := rejectCredentialQueryParams("https://sched.lf.org/kc?oauth_token=s3cr3t")
	if named == nil {
		t.Fatal("a credential-shaped query parameter was cleared for publication")
	}
	if !strings.Contains(named.Error(), `"token"`) {
		t.Errorf("a valued key must be pointed at by its classifying word to stay actionable, got: %v", named)
	}
	if strings.Contains(named.Error(), "s3cr3t") {
		t.Errorf("the refusal rendered the parameter VALUE, which is the secret: %v", named)
	}

	// The refusal no longer renders the caller's key at all — not even bounded and
	// control-stripped — because a parameter NAME is free text and can carry the secret
	// itself. What it names is the fixed vocabulary word that classified it, so control
	// characters and over-long names have nothing to ride in on.
	for _, key := range []string{"oauth_token_s3cr3t-LEAKED", "a\x00b\nc_token", strings.Repeat("k", 300) + "_secret"} {
		err := rejectCredentialQueryParams("https://events.lf.org/r?" + url.QueryEscape(key) + "=x")
		if err == nil {
			t.Fatalf("key %q was not refused", key)
		}
		msg := err.Error()
		if strings.Contains(msg, "LEAKED") {
			t.Errorf("the refusal reproduced credential material from the parameter NAME: %v", err)
		}
		if strings.ContainsAny(msg, "\x00\n") {
			t.Errorf("control characters from the key reached an error that lands in the log: %q", msg)
		}
		if len(msg) > 400 {
			t.Errorf("an over-long parameter name was echoed into the error: %d bytes", len(msg))
		}
	}
}

// TestRejectCredentialQueryParams_RejectsEmbeddedUserinfo covers the hole the
// query-only screen left wide open: validateRegistrationURL refuses userinfo, but a
// link the caller pasted into their OWN copy never reaches that validator, and a URL
// like https://user:password@host/path has no query at all — so it passed a screen that
// only ever read query keys, and was published verbatim.
func TestRejectCredentialQueryParams_RejectsEmbeddedUserinfo(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"https://user:password@example.com/path",
		"https://deploykey@example.com/path?utm_source=x",
	} {
		err := rejectCredentialQueryParams(raw)
		if err == nil {
			t.Errorf("a URL with embedded userinfo was cleared for publication: %s", raw)
			continue
		}
		if strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "deploykey") {
			t.Errorf("the refusal reproduced the embedded credential: %v", err)
		}
	}

	// The whole point is that this runs on composed text, not only on the registration
	// URL, so the same link inside caller copy must be refused too.
	if err := rejectCredentialQueryParamsInText("Register now https://user:password@example.com/path today"); err == nil {
		t.Error("userinfo inside caller-supplied tweet copy was cleared for publication")
	}
}

// TestComposeTweetText_RejectsAnOversizedRawBody guards the gap the weighted cap
// leaves open by design: a URL weighs a fixed 23 however long it really is, so one
// enormous link passes the 280 check and is only rejected once it has been encoded
// into the tweet-create request URI — after the campaign and line item exist.
func TestComposeTweetText_RejectsAnOversizedRawBody(t *testing.T) {
	t.Parallel()

	huge := "https://sched.lf.org/kc?ref=" + strings.Repeat("x", maxTweetRawBytes)
	if _, err := composeTweetText("Agenda: "+huge, "https://sched.lf.org/kc"); err == nil {
		t.Fatal("an oversized raw body passed validation; it would fail only after the campaign was created")
	}

	// The bound must not reach any legitimate brief: a full-width 280-weight tweet is
	// four-byte runes throughout and still an order of magnitude below the cap.
	// 2 weight per emoji, plus the appended URL's t.co weight of 23 and the separating
	// space — the whole budget, spent on the widest runes there are.
	full := strings.Repeat("😀", (maxTweetWeightedChars-tcoURLWeight-1)/2)
	if _, err := composeTweetText(full, "https://sched.lf.org/kc"); err != nil {
		t.Errorf("a legitimate maximum-weight tweet was rejected by the raw cap: %v", err)
	}
}

// TestExtractTweetID_RejectsAMalformedIDStr closes the asymmetry between an explicit
// TweetID, which is held to tweetIDRe and the int64 range before any mutating call,
// and an AUTHORED id, which was taken on trust from a 2xx body. The caller only tests
// it for emptiness, so a non-numeric value was recorded as a confirmed authored tweet
// and persisted into Steps before failing at promoted_tweets.
func TestExtractTweetID_RejectsAMalformedIDStr(t *testing.T) {
	t.Parallel()

	valid := &apiResponse{Data: []byte(`{"id_str":"1770000000000000001"}`)}
	if got := extractTweetID(valid); got != "1770000000000000001" {
		t.Fatalf("a well-formed id was not extracted: %q", got)
	}

	for _, body := range []string{
		`{"id_str":"not-a-tweet"}`,
		`{"id_str":"0"}`,
		`{"id_str":"0177"}`,
		`{"id_str":"9999999999999999999"}`, // 19 digits, above max int64
		`{"id_str":"<html>error</html>"}`,
	} {
		if got := extractTweetID(&apiResponse{Data: []byte(body)}); got != "" {
			t.Errorf("%s yielded %q; a malformed id must take the UNCONFIRMED path, not be reported as success", body, got)
		}
	}
}

// TestResolvePromotableUser_FullPageWithNoUsableCursorIsRefused asserts the one shape
// that leaves the list genuinely unconfirmed: a FULL page, which X's contract says
// owes a next_cursor, carrying one it gives no meaning to. Concluding there would
// report a not-found or auto-pick an author out of a list that may have more.
//
// The complementary case is the important one for live accounts and is covered by the
// existing single-candidate test: a SHORT page is conclusively the last one under X's
// own documented rule, needs no cursor, and must still resolve.
func TestResolvePromotableUser_FullPageWithNoUsableCursorIsRefused(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	b.WriteString(`{"data":[`)
	for i := 0; i < listPageSize; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"user_id":"u%d"}`, i)
	}
	b.WriteString(`],"next_cursor":""}`)
	body := b.String()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := newAuthorTweetTestClient(srv.URL)
	if _, err := c.resolvePromotableUser(context.Background(), "someone-not-here"); err == nil {
		t.Fatal("reported a pinned user absent from a list that was never confirmed complete")
	} else if !strings.Contains(err.Error(), "cannot be confirmed complete") {
		t.Errorf("error should say the list is unconfirmed, got: %v", err)
	}
}

// TestResolvePromotableUser_AbsentDataFieldIsRefused pins the difference between an
// empty page and a MISSING one. `{"data":[]}` is X saying there are no more users;
// `{}` or `{"data":null}` is X not answering, and reading the second as the first is
// how the single user seen on page one gets auto-selected off an unconfirmed list —
// publishing a tweet under a handle the caller never chose. findByName already refuses
// this shape; identity selection has strictly more to lose than a duplicate lookup.
func TestResolvePromotableUser_AbsentDataFieldIsRefused(t *testing.T) {
	t.Parallel()

	for name, terminal := range map[string]string{
		"absent data field": `{"next_cursor":null}`,
		"explicit null":     `{"data":null,"next_cursor":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var calls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if calls == 1 {
					// A full page, so the walk is obliged to continue — and one user,
					// so a wrongly-cleared terminal page would auto-select it.
					var b strings.Builder
					b.WriteString(`{"data":[{"user_id":"u1"}`)
					for i := 1; i < listPageSize; i++ {
						fmt.Fprintf(&b, `,{"user_id":"u%d"}`, i)
					}
					b.WriteString(`],"next_cursor":"c2"}`)
					_, _ = w.Write([]byte(b.String()))
					return
				}
				_, _ = w.Write([]byte(terminal))
			}))
			defer srv.Close()

			c := newAuthorTweetTestClient(srv.URL)
			if _, err := c.resolvePromotableUser(context.Background(), ""); err == nil {
				t.Fatal("a page with no data field was read as an empty page and the list concluded")
			} else if !strings.Contains(err.Error(), "no data field") {
				t.Errorf("error should name the missing data field, got: %v", err)
			}
		})
	}
}

// TestBuildTwitterUTMURL_FailsClosedOnAnUnreadableQuery covers the same u.Query() trap
// one call site away from where it was first fixed, where it is worse: the unreadable
// pairs are not merely invisible to the credential screen, they are OVERWRITTEN when
// the re-encoded query replaces RawQuery. A brief would have been created with its
// routing parameters silently dropped, sending real click traffic to the wrong page —
// and the credential screen downstream would have found nothing left to object to.
func TestBuildTwitterUTMURL_FailsClosedOnAnUnreadableQuery(t *testing.T) {
	t.Parallel()

	in := baseAuthorInput("Join us")
	in.RegistrationURL = "https://sched.lf.org/kc?ref=partner;session_token=s3cr3t"

	got, err := buildTwitterUTMURL(in)
	if err == nil {
		t.Fatalf("an unreadable registration query was silently rewritten into %q", got)
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("the refusal reproduced the credential it was refusing: %v", err)
	}
}

// TestWeightedTweetLen_BMPEmojiSequencesAndDecomposedText guards the OVER-count
// direction, the one the concept file says no retry fixes and no error explains.
func TestWeightedTweetLen_BMPEmojiSequencesAndDecomposedText(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		in   string
		want int
	}{
		// U+270A is BMP and carries NO U+FE0F before its skin-tone modifier; X charges
		// 2 for the whole sequence, a per-rune pass charged 4.
		"BMP base with skin tone": {"✊\U0001F3FD", 2},
		// A keycap without the optional variation selector: digit + U+20E3.
		"bare keycap": {"1⃣", 2},
		// Still one cluster when the selector IS present, and still 2.
		"keycap with selector": {"1️⃣", 2},
		// Decomposed "é" is two runes and one character to X after NFC.
		"decomposed accent": {"e\u0301", 1},
		// Precomposed must agree with decomposed — that is the whole point of NFC.
		"precomposed accent": {"\u00e9", 1},
		// Two decomposed characters, to catch a fold that only handles the first.
		"decomposed pair": {"e\u0301a\u0301", 2},
		// A bare BMP codepoint with no request stays weight 1, unchanged.
		"bare copyright": {"©", 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := weightedTweetLen(tc.in); got != tc.want {
				t.Errorf("weightedTweetLen(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestCreateCampaign_429OnCampaignCreateIssuesExactlyOneRequest is the regression
// for the retry-safety flag on the campaign create.
//
// The flag was originally true on the reasoning that this create is "found or
// created by name, so re-issuing one converges". It does not: the by-name lookup
// runs in CreateCampaign, ABOVE doRequestAbs's retry loop, so a retry inside that
// loop re-POSTs without consulting it and X — which does not dedupe campaign names
// itself — can accept both. A 429 may also be reported at or after the write was
// accepted, so the duplicate is a real paid resource.
//
// Exactly one POST must therefore reach /campaigns, and the outcome must be
// UNCONFIRMED rather than a clean failure: the first write may have committed.
func TestCreateCampaign_429OnCampaignCreateIssuesExactlyOneRequest(t *testing.T) {
	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "campaigns"):
			atomic.AddInt32(&posts, 1)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"errors":[{"code":"RATE_LIMIT","message":"too many requests"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := newAuthorTweetTestClient(srv.URL)
	res, err := c.CreateCampaign(context.Background(), baseAuthorInput("Join us at KubeCon"))
	if err == nil {
		t.Fatal("expected an error when the campaign create is rate limited")
	}
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Errorf("campaign create POSTed %d times, want exactly 1 — a retry can create a duplicate paid campaign", got)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a mutating 429 may have committed; error must be UNCONFIRMED, got: %v", err)
	}
	// The name-carrying partial is what makes the UNCONFIRMED outcome reconcilable.
	if res == nil || res.CampaignName == "" {
		t.Errorf("expected a name-carrying partial result for reconciliation, got %+v", res)
	}
}

// TestCreateCampaign_429OnLineItemCreateIssuesExactlyOneRequest is the same
// regression one step down the chain. The line-item create shared the campaign
// create's disproven "converges by name" justification and has the same exposure:
// its find-by-name lookup is in the caller, so an in-loop retry duplicates a paid
// line item under an already-created campaign.
func TestCreateCampaign_429OnLineItemCreateIssuesExactlyOneRequest(t *testing.T) {
	var posts int32
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
			atomic.AddInt32(&posts, 1)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"errors":[{"code":"RATE_LIMIT","message":"too many requests"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := newAuthorTweetTestClient(srv.URL)
	res, err := c.CreateCampaign(context.Background(), baseAuthorInput("Join us at KubeCon"))
	if err == nil {
		t.Fatal("expected an error when the line item create is rate limited")
	}
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Errorf("line item create POSTed %d times, want exactly 1 — a retry can create a duplicate paid line item", got)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") {
		t.Errorf("a mutating 429 may have committed; error must be UNCONFIRMED, got: %v", err)
	}
	// The already-created campaign must come back so the orphan is identifiable.
	if res == nil || res.CampaignID != "cmp1" {
		t.Errorf("expected a partial result carrying the created campaign cmp1, got %+v", res)
	}
}

// TestRedactURLForError_KeepsOnlySchemeAndHost pins the redactor's line.
//
// It used to keep the path, on the reasoning that only a query parameter can hold
// a secret. A magic-link or password-reset credential lives in a path segment at
// least as often — and this helper now screens ARBITRARY caller copy, not only an
// operator-typed registration URL. A host cannot be the secret and is what tells
// the operator which link to fix; everything past it can be and is dropped.
func TestRedactURLForError_KeepsOnlySchemeAndHost(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"path segment is dropped", "https://example.com/reset/SECRET", "https://example.com"},
		{"query and fragment go too", "https://example.com/r?token=SECRET#SECRET", "https://example.com"},
		{"userinfo never appears", "https://user:SECRET@example.com/x", "https://example.com"}, // secretlint-disable-line -- fixture asserting userinfo is dropped
		{"port is part of the host", "https://example.com:8443/a/SECRET", "https://example.com:8443"},
		{"ipv6 literal host", "https://[2001:db8::1]/reset/SECRET", "https://[2001:db8::1]"},
		{"surrounding space is trimmed", "  https://example.com/SECRET  ", "https://example.com"},
		{"relative url has no host to name", "/reset/SECRET", "(redacted)"},
		{"unparseable url", "https://exa mple.com/\x7f/SECRET", "(redacted)"},
		{"empty", "", "(redacted)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := redactURLForError(tc.in)
			if got != tc.want {
				t.Errorf("redactURLForError(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "SECRET") {
				t.Errorf("redactURLForError(%q) reproduced the secret: %q", tc.in, got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Round-7 review fixes
// ---------------------------------------------------------------------------

// injectedTransportError replaces the tweet-authoring round trip with an error of the
// caller's choosing. This is the threat hubspot.safeCause's doc comment names in as many
// words and that this client's own safeTransportCause used to dismiss: WithHTTPClient is
// a supported option, so the INNERMOST cause is text this package cannot vouch for, and
// an http.Client wraps whatever a RoundTripper returns in a *url.Error — peeling that
// wrapper hands the caller's text straight through.
type injectedTransportError struct {
	base  http.RoundTripper
	cause error
}

func (t *injectedTransportError) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tweet") {
		return nil, t.cause
	}
	return t.base.RoundTrip(r)
}

// TestSafeTransportCause_DoesNotRenderACustomTransportsText proves the fixed-vocabulary
// allowlist, and proves it where it matters: PromotedTweetWarning and Steps are
// PERSISTED, so a transport error that embeds the signed request URL used to be written
// into the campaign row verbatim.
//
// The unit half pins the vocabulary; the end-to-end half pins the sinks. Both are needed
// — a clean Error() proves nothing about a string that reaches Steps by another route.
func TestSafeTransportCause_DoesNotRenderACustomTransportsText(t *testing.T) {
	const secret = "https://ads-api.x.com/12/accounts/acc1/tweet?signature=SECRET-abc123&text=copy"

	t.Run("the allowlist collapses an unrecognized cause", func(t *testing.T) {
		t.Parallel()
		// As http.Client.Do delivers it: the caller's error inside a *url.Error.
		wrapped := &url.Error{Op: "Post", URL: secret, Err: errors.New("proxy rejected " + secret)}
		if got := safeTransportCause(wrapped); got != "transport failure" {
			t.Errorf("safeTransportCause = %q, want the default-deny %q", got, "transport failure")
		}
		// The named causes still classify — the allowlist must not cost diagnosability
		// for the shapes that actually occur.
		for cause, want := range map[error]string{
			context.Canceled:          "context canceled",
			context.DeadlineExceeded:  "context deadline exceeded",
			&net.DNSError{Err: "nxd"}: "dns lookup failed",
		} {
			if got := safeTransportCause(&url.Error{Op: "Post", URL: secret, Err: cause}); got != want {
				t.Errorf("safeTransportCause(%v) = %q, want %q", cause, got, want)
			}
		}
	})

	t.Run("nothing reaches the persisted warning or steps", func(t *testing.T) {
		srv, _ := newAuthorTweetTestServer(t, nil, `{"data":[{"user_id":"u1","promotable_user_type":"FULL"}]}`)
		defer srv.Close()

		c := NewClient(
			Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
			AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
			WithBaseURL(srv.URL),
			WithWriteDelay(0),
			WithHTTPClient(&http.Client{Transport: &injectedTransportError{
				base:  http.DefaultTransport,
				cause: errors.New("upstream said: " + secret),
			}}),
		)
		c.nonceFn = func() string { return "n" }
		c.timeFn = staticTime

		res, err := c.CreateCampaign(context.Background(), baseAuthorInput("Join us at KubeCon"))
		if err != nil {
			t.Fatalf("an authoring failure is non-fatal; CreateCampaign returned: %v", err)
		}
		if strings.Contains(res.PromotedTweetWarning, "SECRET-abc123") || strings.Contains(res.PromotedTweetWarning, "signature=") {
			t.Errorf("PromotedTweetWarning carries the injected transport text: %q", res.PromotedTweetWarning)
		}
		for _, s := range res.Steps {
			if strings.Contains(s, "SECRET-abc123") || strings.Contains(s, "signature=") {
				t.Errorf("a persisted Steps entry carries the injected transport text: %q", s)
			}
		}
	})
}

// TestRejectCredentialQueryParams_BareAndValuedOccurrencesOfTheSameKey covers the hole
// the name-vs-category split left: safety was tracked per KEY, so one occurrence written
// as `name=value` marked the key renderable and the OTHER occurrence — the bare one,
// whose whole text is the caller's token — was echoed into an error that is logged and
// persisted. Safety is now tracked per OCCURRENCE: a key is renderable only if EVERY
// occurrence was written with a value.
func TestRejectCredentialQueryParams_BareAndValuedOccurrencesOfTheSameKey(t *testing.T) {
	t.Parallel()

	const bare = "oauth_token_SECRETMATERIAL"

	err := rejectCredentialQueryParams("https://sched.lf.org/kc?" + bare + "&" + bare + "=x")
	if err == nil {
		t.Fatal("a credential-shaped query component was cleared for publication")
	}
	if strings.Contains(err.Error(), "SECRETMATERIAL") {
		t.Errorf("the refusal echoed a bare credential component because a valued twin made it look like a name: %v", err)
	}

	// The fragment path shares the helper and shared the defect.
	frag := rejectCredentialQueryParams("https://app.lf.org/cb#" + bare + "&" + bare + "=x")
	if frag == nil {
		t.Fatal("a credential-shaped fragment component was cleared for publication")
	}
	if strings.Contains(frag.Error(), "SECRETMATERIAL") {
		t.Errorf("the fragment refusal echoed a bare credential component: %v", frag)
	}
}

// TestCredentialFragmentError_NamesAValuedKeyAndRedactsABareOne pins the split the
// round-6 fragment screen was written without: it rendered the key unconditionally, so a
// fragment whose entire text is an implicit-flow token — `#access_token_<token>`, no `=`
// anywhere — was reproduced in the refusal. The query path had had this right since
// round 4; the new fragment path did not inherit it.
func TestCredentialFragmentError_NamesAValuedKeyAndRedactsABareOne(t *testing.T) {
	t.Parallel()

	named := rejectCredentialQueryParams("https://app.lf.org/cb#access_token=s3cr3t")
	if named == nil {
		t.Fatal("a credential-shaped fragment parameter was cleared for publication")
	}
	if !strings.Contains(named.Error(), `"accesstoken"`) {
		t.Errorf("a valued fragment key must be pointed at by its classifying word, got: %v", named)
	}
	if strings.Contains(named.Error(), "s3cr3t") {
		t.Errorf("the refusal rendered the fragment parameter VALUE, which is the secret: %v", named)
	}

	// A BARE component inside a screened fragment — the implicit flow's
	// `#access_token_<token>&state=xyz` shape, where the credential IS the component
	// text. Round 6 rendered the key unconditionally and reproduced it.
	//
	// A fragment with no `=` at all is deliberately not screened and is not asserted
	// here: that is the section-anchor form (`#speakers`, `#agenda-day-2`), and
	// refusing it would fail a working brief for no gain.
	bare := rejectCredentialQueryParams("https://app.lf.org/cb#access_token_SECRETMATERIAL&state=xyz")
	if bare == nil {
		t.Fatal("a bare credential-shaped fragment component was cleared for publication")
	}
	if strings.Contains(bare.Error(), "SECRETMATERIAL") {
		t.Errorf("the refusal echoed a bare fragment token: %v", bare)
	}
}

// TestBuildTwitterUTMURL_PreservesTheRawQueryBytes proves the "verbatim" promise the
// concept file and docs/api-catalog.md both make about the ad's real click destination.
// The builder used to round-trip through url.Values.Encode, which SORTS keys and
// re-canonicalizes escaping — `%20` becomes `+` — so a destination whose routing depends
// on either was silently rewritten before it was published.
func TestBuildTwitterUTMURL_PreservesTheRawQueryBytes(t *testing.T) {
	t.Parallel()

	in := baseAuthorInput("")
	in.RegistrationURL = "https://events.lf.org/kubecon?z=last&a=first&q=hello%20world"

	got, err := buildTwitterUTMURL(in)
	if err != nil {
		t.Fatalf("buildTwitterUTMURL: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if !strings.HasPrefix(u.RawQuery, "z=last&a=first&q=hello%20world&") {
		t.Errorf("the pre-existing query was reordered or re-encoded: %q", u.RawQuery)
	}
	if strings.Contains(u.RawQuery, "hello+world") {
		t.Errorf("%%20 was rewritten to '+': %q", u.RawQuery)
	}
	if u.Query().Get("utm_source") != "twitter" {
		t.Errorf("the UTM parameters were not appended: %q", u.RawQuery)
	}

	// A pre-existing key that COLLIDES with a UTM name is dropped rather than
	// duplicated — two utm_source values make click attribution depend on which one
	// the landing page reads first.
	in.RegistrationURL = "https://events.lf.org/kubecon?utm_source=stale&ref=partner"
	got, err = buildTwitterUTMURL(in)
	if err != nil {
		t.Fatalf("buildTwitterUTMURL: %v", err)
	}
	if strings.Contains(got, "utm_source=stale") {
		t.Errorf("a colliding pre-existing utm_source survived: %q", got)
	}
	if !strings.Contains(got, "ref=partner") {
		t.Errorf("a non-colliding pre-existing parameter was dropped: %q", got)
	}
}

// TestComposeTweetText_AppendsWhenTheDestinationIsOnlyNestedInAnotherURL covers the
// silent failure a substring test allowed: a URL is a substring of any URL that carries
// it in a redirect or tracking parameter, so copy holding
// `https://click.example.net/r?next=<dest>` satisfied strings.Contains, the append was
// skipped, and X wrapped the whole run as the OTHER link. The ad then had no direct
// click destination at all — and the create succeeded, so nothing surfaced it.
func TestComposeTweetText_AppendsWhenTheDestinationIsOnlyNestedInAnotherURL(t *testing.T) {
	t.Parallel()

	const dest = "https://events.lf.org/kc"

	nested := "Register via https://click.example.net/r?next=" + dest
	got, err := composeTweetText(nested, dest)
	if err != nil {
		t.Fatalf("composeTweetText: %v", err)
	}
	if got != nested+" "+dest {
		t.Errorf("the destination was not appended when it only appeared nested in another URL: %q", got)
	}

	// The append must still be skipped when the destination really is its own link,
	// including at the end of a sentence — an operator writes the trailing period.
	for _, text := range []string{
		"Register at " + dest,
		"Register at " + dest + ".",
	} {
		got, err := composeTweetText(text, dest)
		if err != nil {
			t.Fatalf("composeTweetText(%q): %v", text, err)
		}
		if got != text {
			t.Errorf("the destination was appended twice for %q, got %q", text, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Round-8 review fixes
// ---------------------------------------------------------------------------

// TestBuildTwitterUTMURL_KeepsAHashRouterRoute is the shape that made dropping the
// fragment a routing bug rather than a cosmetic one. A hash-router SPA serves ONE
// document and reads everything after `#` as the path, so a destination stripped of
// its fragment is not "the same page without an anchor" — it is a different page.
func TestBuildTwitterUTMURL_KeepsAHashRouterRoute(t *testing.T) {
	t.Parallel()

	in := baseAuthorInput("")
	in.RegistrationURL = "https://events.lf.org/#/register/kubecon-na"

	got, err := buildTwitterUTMURL(in)
	if err != nil {
		t.Fatalf("buildTwitterUTMURL: %v", err)
	}
	if !strings.Contains(got, "#/register/kubecon-na") {
		t.Errorf("hash route lost: %q", got)
	}
	if !strings.Contains(got, "utm_source=twitter") {
		t.Errorf("utm params not added: %q", got)
	}
}

// TestCreateCampaign_RefusesACredentialFragmentOnTheRegistrationURL closes the loop
// the fragment strip had quietly opened. credentialFragmentError was written to screen
// exactly this, but buildTwitterUTMURL removed the fragment BEFORE the composed text
// reached rejectCredentialQueryParamsInText — so the arm only ever saw links the
// operator typed into their own copy, never the registration URL it was written for.
//
// The refusal must also land before any mutating call: a campaign and line item that
// exist with no promoted tweet are the expensive failure this screen's placement
// avoids.
func TestCreateCampaign_RefusesACredentialFragmentOnTheRegistrationURL(t *testing.T) {
	t.Parallel()

	// atomic, not a bare int: the handler goroutine writes this and the test goroutine
	// reads it, and the read IS the assertion — see the test-hygiene knowledge base,
	// `httptest-handler-state-needs-synchronized-handoff`.
	var mutations atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mutations.Add(1)
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	in := baseAuthorInput("Register now for KubeCon")
	in.RegistrationURL = "https://events.lf.org/kc#access_token=s3cr3t-fragment-value"

	c := newAuthorTweetTestClient(srv.URL)
	_, err := c.CreateCampaign(context.Background(), in)
	if err == nil {
		t.Fatalf("expected a refusal for a credential-shaped fragment")
	}
	if n := mutations.Load(); n != 0 {
		t.Errorf("refused only after %d mutating call(s); the screen must run first", n)
	}
	if strings.Contains(err.Error(), "s3cr3t-fragment-value") {
		t.Errorf("the refusal reproduced the credential VALUE: %q", err)
	}
	if !strings.Contains(err.Error(), "accesstoken") {
		t.Errorf("the refusal should point at the offending key by its classifying word: %q", err)
	}
}

// ---------------------------------------------------------------------------
// Round-9 review fixes
// ---------------------------------------------------------------------------

// TestCredentialFragmentError_ScreensABareFragmentToo closes the exemption that
// round 8 turned into a publish path. A fragment with no `=` used to pass
// unconditionally, which was sound while buildTwitterUTMURL stripped the fragment —
// there was no publish path behind it. Now the fragment IS published, so `#access_token`
// standing alone would go out in the tweet unexamined.
//
// The anchors an operator actually writes still have to clear it, or the fix trades a
// leak for a broken brief.
func TestCredentialFragmentError_ScreensABareFragmentToo(t *testing.T) {
	t.Parallel()

	for _, anchor := range []string{"register", "agenda-day-2", "speakers", "sessions", "session-track", "schedule", "sponsors", "venue", "keynote", "day-pass"} {
		raw := "https://events.lf.org/kc#" + anchor
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if err := credentialFragmentError(raw, u); err != nil {
			t.Errorf("section anchor %q refused: %v", anchor, err)
		}
	}

	for _, frag := range []string{"access_token", "jwt", "sessionid", "api_key"} {
		raw := "https://events.lf.org/kc#" + frag
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		err = credentialFragmentError(raw, u)
		if err == nil {
			t.Errorf("bare credential-shaped fragment %q was not refused", frag)
			continue
		}
		// With no `=`, the whole fragment landed in the key position, so its text may
		// BE the credential — named as a category, never echoed.
		if strings.Contains(err.Error(), frag) {
			t.Errorf("the refusal echoed the bare fragment %q: %v", frag, err)
		}
	}
}

// TestAppendUTMToRawQuery_DoesNotReassembleANonCollidingQuery pins the last gap in
// "byte for byte". Splitting on `&` and rejoining normalises a query's empty
// components, so `a=1&&b=2&` came back as `a=1&b=2` — equivalent to every parser, and
// still a rewrite of a destination that needed no rewriting. When nothing collides the
// original bytes are now used as written.
func TestAppendUTMToRawQuery_DoesNotReassembleANonCollidingQuery(t *testing.T) {
	t.Parallel()

	utm := map[string]string{"utm_source": "twitter", "utm_medium": "paid-social"}

	got := appendUTMToRawQuery("a=1&&b=2&", utm)
	const wantPrefix = "a=1&&b=2&&"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("pre-existing query bytes were rewritten: got %q, want prefix %q", got, wantPrefix)
	}
	if !strings.Contains(got, "utm_source=twitter") {
		t.Errorf("utm params not appended: %q", got)
	}

	// An empty query gets the UTM set alone — no leading separator.
	if got := appendUTMToRawQuery("", utm); strings.HasPrefix(got, "&") {
		t.Errorf("empty query produced a leading separator: %q", got)
	}

	// The collision path still drops the stale key, and still keeps empty components
	// around it — they cannot name a UTM key.
	got = appendUTMToRawQuery("a=1&&utm_source=stale&b=2", utm)
	if strings.Contains(got, "utm_source=stale") {
		t.Errorf("colliding key survived: %q", got)
	}
	if !strings.HasPrefix(got, "a=1&&b=2&") {
		t.Errorf("empty component dropped on the collision path: %q", got)
	}
}


// assertNamesClassifyingTerm checks that a refusal points the operator at the offending
// parameter by the fixed vocabulary word that classified it, and never by reproducing
// the caller's own spelling of the key — which is free text and can hold a secret.
func assertNamesClassifyingTerm(t *testing.T, err error, key string) {
	t.Helper()

	term, bad := credentialQueryKeyMatch(key)
	if !bad {
		t.Fatalf("test key %q does not classify as a credential at all", key)
	}
	if !strings.Contains(err.Error(), term) {
		t.Errorf("refusal for %q does not name its classifying word %q, so the operator cannot find the parameter: %v", key, term, err)
	}
	// Every term must be a literal from the client's own lists. A term that is not one
	// is a slice of the caller's key, which is the leak this shape exists to prevent.
	if !isFixedCredentialVocabulary(term) {
		t.Errorf("refusal for %q named %q, which is not a fixed vocabulary word", key, term)
	}
}

// isFixedCredentialVocabulary reports whether term is one of the literals the client
// itself declares, independently of how the classifier reached it.
func isFixedCredentialVocabulary(term string) bool {
	if _, ok := credentialQueryKeys[term]; ok {
		return true
	}
	if _, ok := credentialQueryComponents[term]; ok {
		return true
	}
	for _, frag := range credentialQuerySubstrings {
		if term == frag {
			return true
		}
	}
	// The qualified-`key` tier renders two declared literals joined by `_`. Both halves
	// have to come from the client's own maps for the term to be our vocabulary.
	if qualifier, ok := strings.CutSuffix(term, "_key"); ok {
		if _, declared := credentialKeyQualifiers[qualifier]; declared {
			return true
		}
	}
	return term == "key"
}

// ---------------------------------------------------------------------------
// Round-10 review fixes
// ---------------------------------------------------------------------------

// TestIsCredentialQueryKey_CatchesCSRFAndSAML closes the last standard credential
// names every tier missed. `csrf_token` was already caught by `token`, but the bare
// `_csrf` that Rails, Spring Security and Express emit normalizes to `csrf` — no
// exact entry, no fragment, not a component — and `SAMLResponse` carries a signed
// assertion in a parameter matching nothing at all. Their values were copied verbatim
// into the published tweet.
func TestIsCredentialQueryKey_CatchesCSRFAndSAML(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"_csrf", "csrf", "CSRFToken", "xsrf", "X-XSRF-TOKEN", "SAMLResponse", "SAMLRequest", "saml_assertion"} {
		if !isCredentialQueryKey(key) {
			t.Errorf("credential parameter %q still clears the publication screen", key)
		}
	}

	// The additions are fragments, so they must not catch routing parameters an
	// events page really uses.
	for _, key := range []string{"session_track", "day_pass", "keyword", "author", "aside", "design", "speaker", "venue", "sponsor_tier"} {
		if isCredentialQueryKey(key) {
			t.Errorf("ordinary events parameter %q was refused", key)
		}
	}
}

// TestCredentialQueryKeyMatch_NeverReturnsCallerText is the structural half of the
// round-10 fix: the term a refusal renders must be a literal this package declares,
// whatever the caller wrote. A parameter NAME is free text — `?oauth_token_<secret>=x`
// classifies on `oauth` and, under the old contract, reproduced the whole key in an
// error bound for the dispatcher, persisted Steps and the service log.
//
// The cases here are keys the classifier already catches. It does NOT catch every
// secret-bearing name — `access_key_AKIA…` normalizes to something that no longer ENDS
// in `key`, so the suffix tier misses it, and `key` is deliberately not a component
// match because `key_metrics`-shaped routing parameters exist. That gap is a denylist
// limit, weighed where the tiers are declared; this test covers what the refusal RENDERS
// once a key does classify, which is a different property and holds regardless.
func TestCredentialQueryKeyMatch_NeverReturnsCallerText(t *testing.T) {
	t.Parallel()

	for _, key := range []string{
		"oauth_token_s3cr3t", "x_csrf_LEAK", "jwt.eyJhbGciOi", "auth_DEADBEEF",
		"SAMLResponse_PHNhbWxw", "connect.sid_LEAK", "session_token_LEAK", "my_api_key",
	} {
		term, bad := credentialQueryKeyMatch(key)
		if !bad {
			t.Errorf("credential-shaped key %q was cleared", key)
			continue
		}
		if !isFixedCredentialVocabulary(term) {
			t.Errorf("key %q produced term %q, which is caller text rather than a declared literal", key, term)
		}
	}
}

// ---------------------------------------------------------------------------
// Round-11 review fixes
// ---------------------------------------------------------------------------

// TestRejectCredentialQueryParamsInText_UnderscorePrefixedURL pins the boundary bug:
// Go's `\b` counts `_` as a word character, so `_https://…` carried no boundary and the
// whole run went unscanned. The credential was then published verbatim.
func TestRejectCredentialQueryParamsInText_UnderscorePrefixedURL(t *testing.T) {
	for _, text := range []string{
		"_https://events.example/cb?access_token=PLAINTEXT",
		"register here _https://events.example/cb?access_token=PLAINTEXT_ today",
		"__https://events.example/cb?jwt=PLAINTEXT",
		"see -https://events.example/cb?api_key=PLAINTEXT",
	} {
		err := rejectCredentialQueryParamsInText(text)
		if err == nil {
			t.Errorf("text %q was not screened: an underscore before the scheme is not a word boundary", text)
			continue
		}
		if strings.Contains(err.Error(), "PLAINTEXT") {
			t.Errorf("refusal for %q echoed the credential value: %v", text, err)
		}
	}
}

// TestRejectCredentialQueryParamsInText_SchemelessLink covers links X linkifies and
// publishes but tweetURLRe never saw, because it requires an explicit scheme.
func TestRejectCredentialQueryParamsInText_SchemelessLink(t *testing.T) {
	refused := []string{
		"register at www.events.example/cb?access_token=PLAINTEXT",
		"register at events.example/cb?access_token=PLAINTEXT",
		"events.example/r#jwt=PLAINTEXT is the link",
		"EVENTS.EXAMPLE:8443/r?sessionid=PLAINTEXT",
	}
	for _, text := range refused {
		err := rejectCredentialQueryParamsInText(text)
		if err == nil {
			t.Errorf("scheme-less text %q was not screened", text)
			continue
		}
		if strings.Contains(err.Error(), "PLAINTEXT") {
			t.Errorf("refusal for %q echoed the credential value: %v", text, err)
		}
	}

	// Ordinary copy must still pass. The `?`/`#` requirement is what keeps dotted
	// prose out of the candidate set; a run with neither has no parameter to read.
	for _, text := range []string{
		"Join us — see agenda.md and section 3.2? Details at https://events.lf.org/agenda",
		"Built with Node.js v1.2 and Go 1.25, talk at 3pm",
		"Register: https://events.lf.org/r?utm_source=x&utm_medium=paid",
		"Questions? ask us. We're at events.lf.org today",
		"Read more at events.lf.org/blog/why-we-ship",
	} {
		if err := rejectCredentialQueryParamsInText(text); err != nil {
			t.Errorf("ordinary tweet copy %q was refused: %v", text, err)
		}
	}
}

// TestCredentialQueryKeyMatch_QualifiedKeyComponent covers the gap round 10 recorded and
// left open: a key name with its own value appended no longer ENDS in `key`, so the
// suffix tier never fired and `api_key_LEAK` cleared the screen outright.
func TestCredentialQueryKeyMatch_QualifiedKeyComponent(t *testing.T) {
	for _, key := range []string{
		"api_key_LEAK", "access_key_AKIAIOSFODNN7", "secret_key_abc",
		"API-KEY-LEAK", "client.key.LEAK", "signing_key_v2", "consumer_key_1",
	} {
		term, bad := credentialQueryKeyMatch(key)
		if !bad {
			t.Errorf("key %q cleared the screen; a qualified `key` component is a credential in every spelling", key)
			continue
		}
		if !isFixedCredentialVocabulary(term) {
			t.Errorf("key %q named %q, which is not a fixed vocabulary word", key, term)
		}
		if strings.Contains(strings.ToLower(term), "leak") || strings.Contains(term, "AKIA") {
			t.Errorf("key %q leaked caller text through the term %q", key, term)
		}
	}

	// The reason `key` is not a plain component match: these are real parameters on a
	// real conference page, and refusing them is the working-brief regression the
	// denylist shape exists to avoid. An unqualified `key` component still passes.
	for _, key := range []string{"key_metrics", "key_takeaways", "key_note", "keyword"} {
		if _, bad := credentialQueryKeyMatch(key); bad {
			t.Errorf("routing parameter %q was refused; only a QUALIFIED `key` component is a credential", key)
		}
	}
}

// TestTweetURLRuns_UnderscoreIsADelimiter covers the half of the boundary fix the
// credential screen does not: weightedTweetLen and textCarriesURL read the same scanner,
// and while `_https://…` went unmatched, X still wrapped that link in a t.co and this
// client still charged every rune of it — rejecting, before the create, copy X accepts —
// and still appended a second copy of a destination the text already carried.
func TestTweetURLRuns_UnderscoreIsADelimiter(t *testing.T) {
	long := "https://events.lf.org/" + strings.Repeat("x", 120)

	if got, want := weightedTweetLen("_"+long), 1+tcoURLWeight; got != want {
		t.Errorf("weightedTweetLen of an underscore-prefixed link = %d, want %d (the underscore plus one t.co weight)", got, want)
	}
	// Only the LEADING delimiter is the boundary rule's business. A trailing `_` is
	// swept into the run like any other non-stop character — tweetURLTrailingPunct
	// does not list it — so the closing half of markdown italics is left out here
	// deliberately rather than pinned as working.
	if !textCarriesURL("register _"+long+" today", long) {
		t.Error("textCarriesURL missed an underscore-delimited destination, so the client would append a second copy of it")
	}
	// A scheme genuinely inside a longer word is still not a link, which is what the
	// boundary rule is for.
	if textCarriesURL("foo"+long, long) {
		t.Error("textCarriesURL matched a scheme buried inside a word")
	}
}

// Round-12 review fixes

// TestRejectCredentialQueryParamsInText_IPv4AndPunycodeHosts pins the gap the round-11
// scheme-less screen opened: its `[a-z]{2,}` final label read as "a TLD is a word", which
// is true of neither a dotted-quad host nor any internationalized TLD, every one of which
// is spelled `xn--…` on the wire.
func TestRejectCredentialQueryParamsInText_IPv4AndPunycodeHosts(t *testing.T) {
	refused := []string{
		"register at 198.51.100.7/r?access_token=PLAINTEXT",
		"register at events.xn--p1ai/r?access_token=PLAINTEXT",
		"198.51.100.7:8443/r#jwt=PLAINTEXT",
		"XN--80AK6AA92E.XN--P1AI/cb?sessionid=PLAINTEXT",
	}
	for _, text := range refused {
		err := rejectCredentialQueryParamsInText(text)
		if err == nil {
			t.Errorf("scheme-less text %q was not screened", text)
			continue
		}
		if strings.Contains(err.Error(), "PLAINTEXT") {
			t.Errorf("refusal for %q echoed the credential value: %v", text, err)
		}
	}

	// The TLD must still START with a letter, or a version string with a query-looking
	// tail behind it becomes a candidate. `3.2?` was already covered by the old
	// two-character minimum and must stay covered by the new shape.
	for _, text := range []string{
		"Built on v1.25 — section 3.2? see the agenda",
		"Ships 2026.10? we'll confirm",
	} {
		if err := rejectCredentialQueryParamsInText(text); err != nil {
			t.Errorf("ordinary tweet copy %q was refused: %v", text, err)
		}
	}
}

// Round-13 review fixes

// TestRejectCredentialQueryParamsInText_CJKAdjacentLink pins the boundary bug the
// round-11 helper introduced: CJK copy puts no space before a link, so treating any
// Unicode letter as "this run is part of a longer word" dropped a real link, carrying a
// real credential, out of the candidate set before it was ever screened.
func TestRejectCredentialQueryParamsInText_CJKAdjacentLink(t *testing.T) {
	refused := []string{
		"登録events.example/r?access_token=PLAINTEXT",
		"詳細はhttps://events.example/r?access_token=PLAINTEXT",
		"登録はこちら198.51.100.7/r?sessionid=PLAINTEXT",
	}
	for _, text := range refused {
		err := rejectCredentialQueryParamsInText(text)
		if err == nil {
			t.Errorf("CJK-adjacent text %q was not screened", text)
			continue
		}
		if strings.Contains(err.Error(), "PLAINTEXT") {
			t.Errorf("refusal for %q echoed the credential value: %v", text, err)
		}
	}

	// An ASCII word hard against a scheme is still not a link — that is the case the
	// boundary rule exists for, and narrowing it to ASCII must not give it up.
	long := "https://events.lf.org/" + strings.Repeat("x", 120)
	if textCarriesURL("foo"+long, long) {
		t.Error("a scheme buried in an ASCII word was treated as a link run")
	}
}

// TestRejectCredentialQueryParamsInText_SchemelessUserinfo covers the scheme-less shape
// that carries a credential with no query to carry it. The scheme-ful
// `https://bob:pw@host` was already refused; the scheme-less one beside it was not.
func TestRejectCredentialQueryParamsInText_SchemelessUserinfo(t *testing.T) {
	refused := []string{
		"bob:PLAINTEXT@events.example is the link",
		"bob:PLAINTEXT@events.example/r?utm_source=x",
		"admin:PLAINTEXT@198.51.100.7:8443/portal",
	}
	for _, text := range refused {
		err := rejectCredentialQueryParamsInText(text)
		if err == nil {
			t.Errorf("scheme-less userinfo text %q was not screened", text)
			continue
		}
		if strings.Contains(err.Error(), "PLAINTEXT") {
			t.Errorf("refusal for %q echoed the credential value: %v", text, err)
		}
	}

	// The colon is the whole discriminator. An email address has none, and refusing
	// those would make the screen worse than the hole it closes.
	for _, text := range []string{
		"contact bob@events.example for details",
		"questions? email user-1@example.com",
		"Ratio 3:4@events tomorrow",
		"Doors 9:30 — see events.lf.org/agenda",
	} {
		if err := rejectCredentialQueryParamsInText(text); err != nil {
			t.Errorf("ordinary tweet copy %q was refused: %v", text, err)
		}
	}
}

// Round-14 review fixes

// TestRejectCredentialQueryParamsInText_ClockAgainstHost pins the false positive the
// round-13 userinfo scanner shipped with: a time of day written hard against a host is
// the RFC 3986 userinfo production byte for byte, and an events platform writes that
// sentence every day. The round-13 negative rows all happened to put punctuation between
// the clock and the host, so none of them caught it.
func TestRejectCredentialQueryParamsInText_ClockAgainstHost(t *testing.T) {
	for _, text := range []string{
		"session 9:30@main.stage tomorrow",
		"keynote 14:00@events.example",
		"finals 3:4@events.example/bracket",
		"doors 09:00@events.example/r?utm_source=x",
	} {
		if err := rejectCredentialQueryParamsInText(text); err != nil {
			t.Errorf("ordinary tweet copy %q was refused: %v", text, err)
		}
	}

	// Digits on BOTH sides is what makes it a clock. One non-digit side and it is a
	// credential pair again — this is the coverage the narrower "numeric username"
	// spelling would have given up.
	for _, text := range []string{
		"9:PLAINTEXT@events.example is the link",
		"ops9:PLAINTEXT@events.example/portal",
	} {
		err := rejectCredentialQueryParamsInText(text)
		if err == nil {
			t.Errorf("scheme-less userinfo text %q was not screened", text)
			continue
		}
		if strings.Contains(err.Error(), "PLAINTEXT") {
			t.Errorf("refusal for %q echoed the credential value: %v", text, err)
		}
	}
}

// TestWeightedRunLen_TextPresentationSelector pins U+FE0E ending an emoji cluster rather
// than joining one. It requests TEXT presentation, so a sequence carrying it is not an
// emoji sequence; twitter-text@3.1.0 weighs U+1F5A5 U+FE0E as 4, and absorbing the
// selector charged 2.
func TestWeightedRunLen_TextPresentationSelector(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want int
	}{
		{"text presentation is two weighed runes", "\U0001F5A5︎", 4},
		{"emoji presentation is one cluster", "\U0001F5A5️", 2},
		{"bare base is one cluster", "\U0001F5A5", 2},
		{"a skin tone still clusters", "✊\U0001F3FD", 2},
		{"a flag pair still clusters", "\U0001F1EF\U0001F1F5", 2},
	} {
		if got := weightedRunLen(tc.in); got != tc.want {
			t.Errorf("%s: weightedRunLen(%q) = %d, want %d", tc.name, tc.in, got, tc.want)
		}
	}
}
