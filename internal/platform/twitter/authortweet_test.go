// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"errors"
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

// TestBuildTwitterUTMURL_KeepsQueryDropsFragment pins the deliberate divergence
// from displayTwitterUtmURL. This URL is the ad's real click destination, so the
// brief's own routing parameters have to survive alongside the generated utm_*
// set; the fragment never reaches a server, so it is dropped rather than
// published.
func TestBuildTwitterUTMURL_KeepsQueryDropsFragment(t *testing.T) {
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
	if u.Fragment != "" || u.RawFragment != "" {
		t.Errorf("fragment survived: %q (raw %q) in %q", u.Fragment, u.RawFragment, got)
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
// So the handler spawns a goroutine that sleeps briefly and then cancels, and the
// client is built with a write delay an order of magnitude larger. The authoring
// POST completes against a local httptest server in well under a millisecond, the
// following pace(ctx) then blocks for the whole write delay, and the cancel lands
// squarely inside it. The margin is in the safe direction: a slow machine delays
// the cancel further INTO the pace window, it does not move it back into the POST.
func TestCreateCampaign_AbortBetweenAuthoringAndPromotionRetainsTweetID(t *testing.T) {
	const (
		writeDelay  = 500 * time.Millisecond
		cancelAfter = 50 * time.Millisecond
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, rec := newAuthorTweetTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		go func() {
			time.Sleep(cancelAfter)
			cancel()
		}()
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
