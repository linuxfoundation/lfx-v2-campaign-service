// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeX is a STATEFUL stand-in for the X Ads API: what a create commits, a later list
// returns. That is what a retry test needs — the second CreateCampaign must see exactly what
// the first one left behind upstream, including a write whose response was lost.
type fakeX struct {
	mu sync.Mutex

	campaigns      []map[string]any
	lineItems      []map[string]any
	promotedTweets []map[string]any

	// failPromotedTweetOnce commits the first promoted_tweets POST and then answers 502, so
	// the caller sees an UNCONFIRMED outcome for an association that does exist.
	failPromotedTweetOnce bool
	// overlapPromotedTweetPages answers the promoted_tweets list in two pages that BOTH carry
	// every association: X's cursor walk may repeat a row, and a repeat is not a second one.
	overlapPromotedTweetPages bool

	posts       map[string]int
	nextID      int
	nextTweetID int64
}

func newFakeX() *fakeX {
	return &fakeX{posts: map[string]int{}, nextTweetID: 1700000000000000000}
}

func (f *fakeX) id(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s%d", prefix, f.nextID)
}

// promotedTweetID returns the id of the i-th recorded association, under the lock the
// handler writes it with.
func (f *fakeX) promotedTweetID(i int) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.promotedTweets[i]["id"]
}

func (f *fakeX) postCount(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts[kind]
}

func writeList(w http.ResponseWriter, items []map[string]any) {
	if items == nil {
		items = []map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"data": items, "next_cursor": nil})
	_, _ = w.Write(b)
}

func (f *fakeX) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/accounts/acc1"):
		_, _ = w.Write([]byte(`{"data":{"id":"acc1","name":"LF Events"}}`))
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/campaigns"):
		var out []map[string]any
		for _, c := range f.campaigns {
			if strings.HasPrefix(strings.ToLower(c["name"].(string)), strings.ToLower(q.Get("q"))) {
				out = append(out, c)
			}
		}
		writeList(w, out)
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/line_items"):
		var out []map[string]any
		for _, li := range f.lineItems {
			if li["campaign_id"] == q.Get("campaign_ids") && strings.HasPrefix(strings.ToLower(li["name"].(string)), strings.ToLower(q.Get("q"))) {
				out = append(out, li)
			}
		}
		writeList(w, out)
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/promoted_tweets"):
		var out []map[string]any
		for _, pt := range f.promotedTweets {
			if pt["line_item_id"] == q.Get("line_item_ids") {
				out = append(out, pt)
			}
		}
		if f.overlapPromotedTweetPages && q.Get("cursor") == "" {
			if out == nil {
				out = []map[string]any{}
			}
			b, _ := json.Marshal(map[string]any{"data": out, "next_cursor": "p2"})
			_, _ = w.Write(b)
			return
		}
		writeList(w, out)
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/promotable_users"):
		_, _ = w.Write([]byte(`{"data":[{"user_id":"u1","promotable_user_type":"FULL"}],"next_cursor":null}`))
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/campaigns"):
		f.posts["campaign"]++
		c := map[string]any{"id": f.id("c"), "name": q.Get("name"), "funding_instrument_id": q.Get("funding_instrument_id")}
		f.campaigns = append(f.campaigns, c)
		b, _ := json.Marshal(map[string]any{"data": c})
		_, _ = w.Write(b)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/line_items"):
		f.posts["line_item"]++
		li := map[string]any{"id": f.id("li"), "name": q.Get("name"), "campaign_id": q.Get("campaign_id")}
		f.lineItems = append(f.lineItems, li)
		b, _ := json.Marshal(map[string]any{"data": li})
		_, _ = w.Write(b)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/tweet"):
		f.posts["tweet"]++
		f.nextTweetID++
		_, _ = fmt.Fprintf(w, `{"data":{"id":%d,"id_str":"%d"}}`, f.nextTweetID, f.nextTweetID)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/promoted_tweets"):
		f.posts["promoted_tweet"]++
		pt := map[string]any{"id": f.id("pt"), "line_item_id": q.Get("line_item_id"), "tweet_id": q.Get("tweet_ids"), "deleted": false}
		f.promotedTweets = append(f.promotedTweets, pt)
		if f.failPromotedTweetOnce {
			f.failPromotedTweetOnce = false
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		b, _ := json.Marshal(map[string]any{"data": []map[string]any{pt}})
		_, _ = w.Write(b)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newFakeXClient(t *testing.T, f *fakeX) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return newAuthorTweetTestClient(srv.URL)
}

// TestCreateCampaign_RetryAfterUnconfirmedPromotionReusesAuthoredTweet is the gap this
// change closes: the first attempt authors and promotes a tweet, but the promoted_tweets
// response is lost (502 after commit), so the outcome is UNCONFIRMED. The retry must find the
// campaign, line item AND the association upstream and reuse them — not publish a second
// tweet under the LF handle and attach a second spending creative.
func TestCreateCampaign_RetryAfterUnconfirmedPromotionReusesAuthoredTweet(t *testing.T) {
	f := newFakeX()
	f.failPromotedTweetOnce = true
	c := newFakeXClient(t, f)
	in := baseAuthorInput("Join us at KubeCon")

	first, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	if !strings.Contains(first.PromotedTweetWarning, "UNCONFIRMED") {
		t.Fatalf("first attempt must be UNCONFIRMED, got warning %q", first.PromotedTweetWarning)
	}

	second, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	for kind, want := range map[string]int{"campaign": 1, "line_item": 1, "tweet": 1, "promoted_tweet": 1} {
		if got := f.postCount(kind); got != want {
			t.Errorf("%s POSTs after retry = %d, want %d (a retry must reuse, not duplicate)", kind, got, want)
		}
	}
	if second.CampaignID != first.CampaignID || second.LineItemID != first.LineItemID {
		t.Errorf("retry ids = %s/%s, want the first attempt's %s/%s", second.CampaignID, second.LineItemID, first.CampaignID, first.LineItemID)
	}
	if want := f.promotedTweetID(0); second.PromotedTweetID == "" || second.PromotedTweetID != want {
		t.Errorf("retry PromotedTweetID = %q, want the committed association %v", second.PromotedTweetID, want)
	}
	if second.AuthoredTweetID != "" {
		t.Errorf("retry AuthoredTweetID = %q, want empty: this call authored nothing", second.AuthoredTweetID)
	}
	if second.PromotedTweetWarning != "" {
		t.Errorf("a confirmed reuse carries no promoted-tweet warning, got %q", second.PromotedTweetWarning)
	}
	if !second.Reused {
		t.Error("Reused must be true on the retry")
	}
	if !stepsContain(second.Steps, "no new tweet authored") || !stepsContain(second.Steps, "NOT compared with this request") {
		t.Errorf("expected reuse + warning steps for the promoted tweet, got %v", second.Steps)
	}
}

// TestCreateCampaign_RetryWithExplicitTweetReusesAssociation: the explicit-TweetID flow on a
// retry finds its existing association and does not re-POST it (which would answer
// DUPLICATE_PROMOTABLE_ENTITY and degrade to "verify manually").
func TestCreateCampaign_RetryWithExplicitTweetReusesAssociation(t *testing.T) {
	f := newFakeX()
	f.failPromotedTweetOnce = true
	c := newFakeXClient(t, f)
	in := baseAuthorInput("")
	in.TweetID = "1234567890"

	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	second, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := f.postCount("promoted_tweet"); got != 1 {
		t.Errorf("promoted_tweets POSTs = %d, want 1", got)
	}
	if second.PromotedTweetID != f.promotedTweetID(0) || second.PromotedTweetWarning != "" {
		t.Errorf("retry promoted = %q warning %q, want the existing association and no warning", second.PromotedTweetID, second.PromotedTweetWarning)
	}
}

// TestCreateCampaign_ReusedLineItemWithTwoTweetsRefusesAuthoring: two live associations on the
// reused line item cannot be told apart, so nothing is authored or promoted.
func TestCreateCampaign_ReusedLineItemWithTwoTweetsRefusesAuthoring(t *testing.T) {
	f := newFakeX()
	c := newFakeXClient(t, f)
	in := baseAuthorInput("Join us at KubeCon")
	first, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	f.mu.Lock()
	f.promotedTweets = append(f.promotedTweets, map[string]any{"id": "ptx", "line_item_id": first.LineItemID, "tweet_id": "99", "deleted": false})
	f.mu.Unlock()

	second, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := f.postCount("tweet"); got != 1 {
		t.Errorf("tweet POSTs = %d, want 1 (ambiguous reuse must not author)", got)
	}
	if got := f.postCount("promoted_tweet"); got != 1 {
		t.Errorf("promoted_tweets POSTs = %d, want 1", got)
	}
	if second.PromotedTweetID != "" || !strings.Contains(second.PromotedTweetWarning, "already promotes 2 tweets") {
		t.Errorf("want no promoted tweet and an ambiguity warning, got %q / %q", second.PromotedTweetID, second.PromotedTweetWarning)
	}
}

// TestCreateCampaign_PromotedTweetLookupFailureBlocksAuthoring: when the reused line item's
// associations cannot be read, an existing one cannot be ruled out, so nothing is written.
func TestCreateCampaign_PromotedTweetLookupFailureBlocksAuthoring(t *testing.T) {
	f := newFakeX()
	var failing bool
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fail := failing
		mu.Unlock()
		if fail && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/promoted_tweets") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c := newAuthorTweetTestClient(srv.URL)
	in := baseAuthorInput("Join us at KubeCon")
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	mu.Lock()
	failing = true
	mu.Unlock()
	second, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if f.postCount("tweet") != 1 || f.postCount("promoted_tweet") != 1 {
		t.Errorf("a failed association read must block writes: tweet=%d promoted=%d", f.postCount("tweet"), f.postCount("promoted_tweet"))
	}
	if !strings.Contains(second.PromotedTweetWarning, "could not read the promoted tweets") {
		t.Errorf("want a lookup-failure warning, got %q", second.PromotedTweetWarning)
	}
}

// TestCreateCampaign_FreshCreateDoesNotReadPromotedTweets: a first create is unchanged — the
// association read runs only on a reused line item.
func TestCreateCampaign_FreshCreateDoesNotReadPromotedTweets(t *testing.T) {
	f := newFakeX()
	var reads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/promoted_tweets") {
			reads.Add(1)
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c := newAuthorTweetTestClient(srv.URL)
	res, err := c.CreateCampaign(context.Background(), baseAuthorInput("Join us at KubeCon"))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if n := reads.Load(); n != 0 {
		t.Errorf("promoted_tweets GETs on a fresh create = %d, want 0", n)
	}
	if res.Reused || res.AuthoredTweetID == "" || res.PromotedTweetID == "" || res.PromotedTweetWarning != "" {
		t.Errorf("fresh create changed: %+v", res)
	}
	for kind := range map[string]bool{"campaign": true, "line_item": true, "tweet": true, "promoted_tweet": true} {
		if f.postCount(kind) != 1 {
			t.Errorf("%s POSTs = %d, want 1", kind, f.postCount(kind))
		}
	}
}

// TestCreateCampaign_DuplicateCampaignNamesRefuse: two live campaigns with the exact
// deterministic name cannot be told apart, so the create refuses before any write.
func TestCreateCampaign_DuplicateCampaignNamesRefuse(t *testing.T) {
	f := newFakeX()
	name := buildTwitterCampaignName(baseAuthorInput(""))
	f.campaigns = []map[string]any{{"id": "c1", "name": name}, {"id": "c2", "name": name}}
	c := newFakeXClient(t, f)
	res, err := c.CreateCampaign(context.Background(), baseAuthorInput("Join us at KubeCon"))
	if err == nil || !strings.Contains(err.Error(), "more than one live element") {
		t.Fatalf("want an ambiguity refusal, got res=%v err=%v", res, err)
	}
	if res != nil {
		t.Errorf("a refusal before any write returns no partial, got %+v", res)
	}
	for _, kind := range []string{"campaign", "line_item", "tweet", "promoted_tweet"} {
		if f.postCount(kind) != 0 {
			t.Errorf("%s POSTs = %d, want 0", kind, f.postCount(kind))
		}
	}
}

// TestCreateCampaign_DuplicateLineItemNamesRefuse: same rule one level down.
func TestCreateCampaign_DuplicateLineItemNamesRefuse(t *testing.T) {
	f := newFakeX()
	in := baseAuthorInput("Join us at KubeCon")
	name := buildTwitterCampaignName(in)
	liName := "Events | LFX Retest | Promoted Tweets | AUTO"
	f.campaigns = []map[string]any{{"id": "c1", "name": name, "funding_instrument_id": "fi1"}}
	f.lineItems = []map[string]any{{"id": "l1", "name": liName, "campaign_id": "c1"}, {"id": "l2", "name": liName, "campaign_id": "c1"}}
	c := newFakeXClient(t, f)
	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "more than one live element") {
		t.Fatalf("want an ambiguity refusal, got err=%v", err)
	}
	if res == nil || res.CampaignID != "c1" || res.LineItemID != "" {
		t.Errorf("want a partial carrying the reused campaign only, got %+v", res)
	}
	if f.postCount("line_item") != 0 || f.postCount("tweet") != 0 {
		t.Errorf("no write may follow an ambiguous lookup: line_item=%d tweet=%d", f.postCount("line_item"), f.postCount("tweet"))
	}
}

// TestCreateCampaign_SameNameDifferentFundingInstrumentRefuses: provenance — a same-named
// campaign on another funding instrument is neither reused nor duplicated.
func TestCreateCampaign_SameNameDifferentFundingInstrumentRefuses(t *testing.T) {
	f := newFakeX()
	f.campaigns = []map[string]any{{"id": "c1", "name": buildTwitterCampaignName(baseAuthorInput("")), "funding_instrument_id": "otherfi"}}
	c := newFakeXClient(t, f)
	_, err := c.CreateCampaign(context.Background(), baseAuthorInput("Join us at KubeCon"))
	if err == nil || !strings.Contains(err.Error(), "different funding instrument") {
		t.Fatalf("want a provenance refusal, got %v", err)
	}
	if f.postCount("campaign") != 0 {
		t.Errorf("campaign POSTs = %d, want 0", f.postCount("campaign"))
	}
}

// TestFindLineItemByName_ForeignParentRefuses: a match whose parent campaign is not the one
// filtered on means the filter was ignored; refused, not reused.
func TestFindLineItemByName_ForeignParentRefuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"l9","name":"target","campaign_id":"other"}],"next_cursor":null}`))
	}))
	defer srv.Close()
	c := newAuthorTweetTestClient(srv.URL)
	id, err := c.findLineItemByName(context.Background(), "camp1", "target")
	if err == nil || id != "" || !strings.Contains(err.Error(), "different parent campaign") {
		t.Fatalf("want a provenance refusal, got id=%q err=%v", id, err)
	}
}

// TestFindByName_DeletedMatchIsNotReused: a match X reports deleted is skipped, and a
// deleted twin does not make a live match ambiguous.
func TestFindByName_DeletedMatchIsNotReused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"old","name":"target","deleted":true},{"id":"live","name":"target"}],"next_cursor":null}`))
	}))
	defer srv.Close()
	c := newAuthorTweetTestClient(srv.URL)
	id, err := c.findCampaignByName(context.Background(), "target")
	if err != nil || id != "live" {
		t.Fatalf("want the live match, got id=%q err=%v", id, err)
	}
}

// TestListPromotedTweets_FailsClosed covers the read's own refusals.
func TestListPromotedTweets_FailsClosed(t *testing.T) {
	cases := map[string]string{
		"no data field":     `{"next_cursor":null}`,
		"foreign line item": `{"data":[{"id":"pt1","line_item_id":"other","tweet_id":"5"}],"next_cursor":null}`,
		"no id":             `{"data":[{"line_item_id":"li1","tweet_id":"5"}],"next_cursor":null}`,
		"bad tweet id":      `{"data":[{"id":"pt1","line_item_id":"li1","tweet_id":"x5"}],"next_cursor":null}`,
		// encoding/json would read the trailing empty array: "no associations" (PR #358 review).
		"duplicated data key":         `{"data":[{"id":"pt1","line_item_id":"li1","tweet_id":"5"}],"data":[],"next_cursor":null}`,
		"one association, two tweets": `{"data":[{"id":"pt1","line_item_id":"li1","tweet_id":"5"},{"id":"pt1","line_item_id":"li1","tweet_id":"6"}],"next_cursor":null}`,
		"full page no cursor":         `{"data":[` + strings.TrimSuffix(strings.Repeat(`{"id":"pt1","line_item_id":"li1","tweet_id":"5"},`, promotedTweetsPageSize), ",") + `]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			c := newAuthorTweetTestClient(srv.URL)
			if refs, err := c.listPromotedTweets(context.Background(), "li1"); err == nil {
				t.Fatalf("want an error, got %v", refs)
			}
		})
	}
	t.Run("deleted skipped", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"id":"pt1","line_item_id":"li1","tweet_id":"5","deleted":true}],"next_cursor":null}`))
		}))
		defer srv.Close()
		c := newAuthorTweetTestClient(srv.URL)
		refs, err := c.listPromotedTweets(context.Background(), "li1")
		if err != nil || len(refs) != 0 {
			t.Fatalf("want no refs, got %v %v", refs, err)
		}
	})
}

// TestCreateCampaign_RepeatedAssociationAcrossPagesIsReused: X's cursor walk returning the same
// association on two pages is ONE association (PR #358 review), so both retry paths — the
// authored-text one and the explicit-TweetID one — reuse it instead of refusing it as ambiguous.
func TestCreateCampaign_RepeatedAssociationAcrossPagesIsReused(t *testing.T) {
	for name, tweetID := range map[string]string{"authored text": "", "explicit tweet": "1234567890"} {
		t.Run(name, func(t *testing.T) {
			f := newFakeX()
			f.failPromotedTweetOnce = true
			c := newFakeXClient(t, f)
			in := baseAuthorInput("Join us at KubeCon")
			if tweetID != "" {
				in = baseAuthorInput("")
				in.TweetID = tweetID
			}
			if _, err := c.CreateCampaign(context.Background(), in); err != nil {
				t.Fatalf("first attempt: %v", err)
			}
			f.mu.Lock()
			f.overlapPromotedTweetPages = true
			f.mu.Unlock()
			second, err := c.CreateCampaign(context.Background(), in)
			if err != nil {
				t.Fatalf("retry: %v", err)
			}
			if got := f.postCount("promoted_tweet"); got != 1 {
				t.Errorf("promoted_tweets POSTs = %d, want 1", got)
			}
			if got := f.postCount("tweet"); tweetID == "" && got != 1 {
				t.Errorf("tweet POSTs = %d, want 1", got)
			}
			if second.PromotedTweetID != f.promotedTweetID(0) || second.PromotedTweetWarning != "" {
				t.Errorf("retry promoted = %q warning %q, want the one existing association and no warning", second.PromotedTweetID, second.PromotedTweetWarning)
			}
		})
	}
}

// TestFindByName_TwinOnLaterPageRefuses pins the read-every-page rule: a same-named twin on
// page 2 must refuse, not be missed by a walk that stopped at page 1's match. The same id
// repeated across pages is one element, not two.
func TestFindByName_TwinOnLaterPageRefuses(t *testing.T) {
	for name, page2ID := range map[string]string{"twin": "b", "same element": "a"} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("cursor") == "" {
					_, _ = w.Write([]byte(`{"data":[{"id":"a","name":"wanted"}],"next_cursor":"p2"}`))
					return
				}
				_, _ = fmt.Fprintf(w, `{"data":[{"id":%q,"name":"wanted"}],"next_cursor":null}`, page2ID)
			}))
			defer srv.Close()
			c := newAuthorTweetTestClient(srv.URL)
			id, err := c.findByName(context.Background(), "campaigns?q=wanted", "wanted")
			if page2ID == "a" {
				if err != nil || id != "a" {
					t.Fatalf("want a, got id=%q err=%v", id, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "more than one live element") {
				t.Fatalf("want an ambiguity refusal, got id=%q err=%v", id, err)
			}
		})
	}
}

// explicitTweetRetry runs one explicit-TweetID create, lets seed adjust the upstream state,
// optionally fails the association read, and runs the retry.
func explicitTweetRetry(t *testing.T, seed func(f *fakeX, lineItemID string), failRead bool) (*fakeX, *CampaignResult) {
	t.Helper()
	f := newFakeX()
	var failing atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/promoted_tweets") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c := newAuthorTweetTestClient(srv.URL)
	in := baseAuthorInput("")
	in.TweetID = "1234567890"
	first, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	f.mu.Lock()
	seed(f, first.LineItemID)
	f.mu.Unlock()
	failing.Store(failRead)
	second, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	return f, second
}

// TestCreateCampaign_ExplicitTweetReadFailureStillPromotes: an explicit tweet publishes
// nothing, so a failed association read does not block the POST.
func TestCreateCampaign_ExplicitTweetReadFailureStillPromotes(t *testing.T) {
	f, res := explicitTweetRetry(t, func(f *fakeX, _ string) { f.promotedTweets = nil }, true)
	if got := f.postCount("promoted_tweet"); got != 2 {
		t.Errorf("promoted_tweets POSTs = %d, want 2 (the retry still promotes)", got)
	}
	if res.PromotedTweetID == "" || !stepsContain(res.Steps, "promoting tweet 1234567890 anyway") {
		t.Errorf("want a promoted tweet and the read-failure step, got %q / %v", res.PromotedTweetID, res.Steps)
	}
}

// TestCreateCampaign_ExplicitTweetAssociatedTwiceRefuses: the same tweet twice on the reused
// line item is ambiguous, so nothing is promoted.
func TestCreateCampaign_ExplicitTweetAssociatedTwiceRefuses(t *testing.T) {
	f, res := explicitTweetRetry(t, func(f *fakeX, li string) {
		f.promotedTweets = append(f.promotedTweets, map[string]any{"id": "ptdup", "line_item_id": li, "tweet_id": "1234567890", "deleted": false})
	}, false)
	if got := f.postCount("promoted_tweet"); got != 1 {
		t.Errorf("promoted_tweets POSTs = %d, want 1", got)
	}
	if res.PromotedTweetID != "" || !strings.Contains(res.PromotedTweetWarning, "promoted more than once") {
		t.Errorf("want a refusal, got %q / %q", res.PromotedTweetID, res.PromotedTweetWarning)
	}
}

// TestCreateCampaign_ExplicitTweetNotYetAssociatedIsPromoted: the reused line item promotes a
// DIFFERENT tweet; the requested one is promoted as asked (the pass-back path).
func TestCreateCampaign_ExplicitTweetNotYetAssociatedIsPromoted(t *testing.T) {
	f, res := explicitTweetRetry(t, func(f *fakeX, li string) {
		f.promotedTweets = []map[string]any{{"id": "ptother", "line_item_id": li, "tweet_id": "99", "deleted": false}}
	}, false)
	if got := f.postCount("promoted_tweet"); got != 2 {
		t.Errorf("promoted_tweets POSTs = %d, want 2", got)
	}
	if res.PromotedTweetID == "" || res.PromotedTweetID == "ptother" || res.PromotedTweetWarning != "" {
		t.Errorf("want a fresh association, got %q / %q", res.PromotedTweetID, res.PromotedTweetWarning)
	}
}
