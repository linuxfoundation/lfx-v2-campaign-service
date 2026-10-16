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
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

// xNegStub answers the line-item GET with lineItem, every targeting_criteria list with criteria,
// and each POST from posts keyed by targeting_value — by default a faithful echo of what was sent.
type xNegStub struct {
	d     *TwitterDispatcher
	mu    sync.Mutex
	posts []url.Values
	all   []xBidRequest
}

func newXNegStub(t *testing.T, lineItem, criteria string, replies map[string]kwtReply) *xNegStub {
	t.Helper()
	return newXNegStubSlow(t, lineItem, criteria, replies, nil)
}

// newXNegStubSlow is newXNegStub whose POST for a keyword in slow is answered only after that
// delay — long enough for the caller's deadline to pass AFTER the request was sent.
func newXNegStubSlow(t *testing.T, lineItem, criteria string, replies map[string]kwtReply, slow map[string]time.Duration) *xNegStub {
	t.Helper()
	s := &xNegStub{}
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		s.mu.Lock()
		s.all = append(s.all, xBidRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery})
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/targeting_criteria"):
			q := r.URL.Query()
			s.mu.Lock()
			s.posts = append(s.posts, q)
			n++
			id := fmt.Sprintf("neg%d", n)
			s.mu.Unlock()
			if d, ok := slow[q.Get("targeting_value")]; ok {
				time.Sleep(d)
			}
			reply, ok := replies[q.Get("targeting_value")]
			if !ok {
				echo, _ := json.Marshal(map[string]any{"data": map[string]any{
					"id": id, "line_item_id": q.Get("line_item_id"), "targeting_type": q.Get("targeting_type"),
					"targeting_value": q.Get("targeting_value"), "operator_type": q.Get("operator_type"), "deleted": false,
				}})
				reply = kwtReply{http.StatusOK, string(echo)}
			}
			w.WriteHeader(reply.status)
			_, _ = io.WriteString(w, reply.body)
		case strings.HasSuffix(r.URL.Path, "/targeting_criteria"):
			_, _ = io.WriteString(w, criteria)
		default:
			_, _ = io.WriteString(w, lineItem)
		}
	}))
	t.Cleanup(srv.Close)
	s.d = NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0))
	return s
}

func (s *xNegStub) sent() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.Values(nil), s.posts...)
}

func (s *xNegStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.all)
}

func addXNegatives(s *xNegStub, c *model.Campaign, rev string, keywords ...model.NegativeKeyword) ([]model.NegativeKeywordOutcome, error) {
	return s.d.AddNegativeKeywords(context.Background(), "proj", model.ProviderTwitterAds, c, keywords, rev)
}

// Each match type maps onto its X keyword type with operator_type NE, one POST per keyword in
// request order; a keyword the line item already excludes with that type is ALREADY_PRESENT and
// not sent; a positive keyword with the same text is no reason to skip.
func TestTwitter_AddNegativeKeywords_OnePostPerKeywordMappedToNE(t *testing.T) {
	s := newXNegStub(t, xKWTLineItem, xCriteria, nil)
	out, err := addXNegatives(s, xBidCampaign(), "",
		model.NegativeKeyword{Text: "free download", MatchType: "Phrase"},
		model.NegativeKeyword{Text: "jobs", MatchType: "Broad"},
		model.NegativeKeyword{Text: "ebpf", MatchType: "exact"},
		model.NegativeKeyword{Text: "torrent", MatchType: "Broad"},
	)
	if err != nil {
		t.Fatalf("AddNegativeKeywords: %v", err)
	}
	want := []struct{ outcome, id, mt string }{
		{model.KeywordOutcomeApplied, "neg1", "Phrase"},
		{model.KeywordOutcomeAlreadyPresent, "", "Broad"},
		{model.KeywordOutcomeApplied, "neg2", "Exact"},
		{model.KeywordOutcomeApplied, "neg3", "Broad"},
	}
	if len(out) != len(want) {
		t.Fatalf("outcomes = %+v", out)
	}
	for i, w := range want {
		if out[i].Outcome != w.outcome || out[i].NegativeKeywordID != w.id || out[i].MatchType != w.mt {
			t.Errorf("outcome %d = %+v, want %+v", i, out[i], w)
		}
	}
	posts := s.sent()
	wantPosts := [][2]string{{"PHRASE_KEYWORD", "free download"}, {"EXACT_KEYWORD", "ebpf"}, {"BROAD_KEYWORD", "torrent"}}
	if len(posts) != len(wantPosts) {
		t.Fatalf("POSTs = %v", posts)
	}
	for i, w := range wantPosts {
		q := posts[i]
		if q.Get("targeting_type") != w[0] || q.Get("targeting_value") != w[1] || q.Get("operator_type") != "NE" || q.Get("line_item_id") != "li1" {
			t.Errorf("POST %d = %v, want %s %q NE on li1", i, q, w[0], w[1])
		}
	}
}

// X echoes a keyword case-normalised and the create path accepts that, so the ALREADY_PRESENT
// check folds case too: a repeat request must not send a duplicate criterion (PR #321 review).
func TestTwitter_AddNegativeKeywords_AlreadyPresentIgnoresCase(t *testing.T) {
	criteria := `{"data":[{"id":"n7","line_item_id":"li1","targeting_type":"PHRASE_KEYWORD","targeting_value":"Free Download","operator_type":"NE","deleted":false}],"next_cursor":null}`
	s := newXNegStub(t, xKWTLineItem, criteria, nil)
	out, err := addXNegatives(s, xBidCampaign(), "", model.NegativeKeyword{Text: "free download", MatchType: "Phrase"}, model.NegativeKeyword{Text: "free download", MatchType: "Exact"})
	if err != nil {
		t.Fatalf("AddNegativeKeywords: %v", err)
	}
	if out[0].Outcome != model.KeywordOutcomeAlreadyPresent || out[1].Outcome != model.KeywordOutcomeApplied {
		t.Fatalf("outcomes = %+v (the type still distinguishes them)", out)
	}
	if posts := s.sent(); len(posts) != 1 || posts[0].Get("targeting_type") != "EXACT_KEYWORD" {
		t.Fatalf("POSTs = %v, want only the EXACT one", posts)
	}
}

// Partial outcomes: X refuses one, the next is ambiguous, the last applies — each answered on its
// own, and the call itself succeeds because some items got a definite answer.
func TestTwitter_AddNegativeKeywords_PerItemOutcomes(t *testing.T) {
	s := newXNegStub(t, xKWTLineItem, xCriteria, map[string]kwtReply{
		"rejected":  {http.StatusBadRequest, `{"errors":[{"code":"INVALID_PARAMETER","message":"secret upstream text"}]}`},
		"throttled": {http.StatusTooManyRequests, `{}`},
		"wrongecho": {http.StatusOK, `{"data":{"id":"z1","line_item_id":"li1","targeting_type":"BROAD_KEYWORD","targeting_value":"wrongecho","operator_type":"EQ"}}`},
	})
	out, err := addXNegatives(s, xBidCampaign(), "", broad("rejected"), broad("throttled"), broad("wrongecho"), broad("fine"))
	if err != nil {
		t.Fatalf("AddNegativeKeywords: %v", err)
	}
	if out[0].Outcome != model.KeywordOutcomeFailed || out[0].ErrorCode != model.KeywordTargetingErrRejected ||
		out[1].Outcome != model.KeywordOutcomeUnconfirmed || out[2].Outcome != model.KeywordOutcomeUnconfirmed ||
		out[3].Outcome != model.KeywordOutcomeApplied {
		t.Fatalf("outcomes = %+v", out)
	}
	for _, o := range out {
		if strings.Contains(o.ErrorCode, "secret") {
			t.Errorf("upstream text leaked into %+v", o)
		}
	}
	// Sent once each: the 429 is not retried.
	if n := len(s.sent()); n != 4 {
		t.Errorf("POSTs = %d, want exactly 4 (a 429 is never retried)", n)
	}
}

// NOT_SENT is a DEFINITE outcome: when the first keyword's POST is sent and then the deadline
// passes (UNCONFIRMED), the rest are never sent, and those NOT_SENT results must be returned per
// item rather than lost to a batch-wide unconfirmed error (PR #321 review).
func TestTwitter_AddNegativeKeywords_NotSentIsDefinite(t *testing.T) {
	s := newXNegStubSlow(t, xKWTLineItem, xCriteria, nil, map[string]time.Duration{"first": 1500 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	out, err := s.d.AddNegativeKeywords(ctx, "proj", model.ProviderTwitterAds, xBidCampaign(), []model.NegativeKeyword{broad("first"), broad("second"), broad("third")}, "")
	if err != nil {
		t.Fatalf("NOT_SENT items are known, so the batch must answer per item, got %v", err)
	}
	if len(out) != 3 || out[0].Outcome != model.KeywordOutcomeUnconfirmed ||
		out[1].Outcome != model.KeywordOutcomeFailed || out[1].ErrorCode != model.KeywordTargetingErrNotSent ||
		out[2].Outcome != model.KeywordOutcomeFailed || out[2].ErrorCode != model.KeywordTargetingErrNotSent {
		t.Fatalf("outcomes = %+v, want [UNCONFIRMED, FAILED/NOT_SENT, FAILED/NOT_SENT]", out)
	}
	if n := len(s.sent()); n != 1 {
		t.Fatalf("POSTs = %d, want only the first", n)
	}
}

// Request duplicates, the already-present lookup and the echo check all use ONE simple-fold key:
// Σ, σ and ς are one orbit, which strings.ToLower splits (PR #321 review).
func TestTwitter_AddNegativeKeywords_UnicodeCaseFolding(t *testing.T) {
	criteria := `{"data":[{"id":"n7","line_item_id":"li1","targeting_type":"BROAD_KEYWORD","targeting_value":"ΟΣ","operator_type":"NE","deleted":false}],"next_cursor":null}`
	s := newXNegStub(t, xKWTLineItem, criteria, map[string]kwtReply{
		// X echoes the new keyword with a final sigma folded differently: still this keyword.
		"σς": {http.StatusOK, `{"data":{"id":"n8","line_item_id":"li1","targeting_type":"BROAD_KEYWORD","targeting_value":"ΣΣ","operator_type":"NE","deleted":false}}`},
	})
	out, err := addXNegatives(s, xBidCampaign(), "", broad("ος"), broad("σς"))
	if err != nil {
		t.Fatalf("AddNegativeKeywords: %v", err)
	}
	if out[0].Outcome != model.KeywordOutcomeAlreadyPresent || out[1].Outcome != model.KeywordOutcomeApplied || out[1].NegativeKeywordID != "n8" {
		t.Fatalf("outcomes = %+v, want [ALREADY_PRESENT (ΟΣ held), APPLIED (ΣΣ echo accepted)]", out)
	}
	if posts := s.sent(); len(posts) != 1 {
		t.Fatalf("POSTs = %v, want only the new keyword", posts)
	}
	dup := newXNegStub(t, xKWTLineItem, xCriteria, nil)
	if _, err := addXNegatives(dup, xBidCampaign(), "", broad("ΟΣ"), broad("ος")); !errors.Is(err, domain.ErrNegativeKeywordInvalid) || len(dup.sent()) != 0 {
		t.Fatalf("ΟΣ and ος are the same keyword and must be refused as a duplicate, got %v", err)
	}
}

// No definite answer at all is an error: UNCONFIRMED when anything may have applied.
func TestTwitter_AddNegativeKeywords_AllAmbiguousIsUnconfirmed(t *testing.T) {
	for _, reply := range []kwtReply{{http.StatusBadGateway, `{}`}, {http.StatusTooManyRequests, `{}`}, {http.StatusOK, `{"data":null}`}} {
		s := newXNegStub(t, xKWTLineItem, xCriteria, map[string]kwtReply{"free": reply})
		_, err := addXNegatives(s, xBidCampaign(), "", broad("free"))
		var u interface{ Unconfirmed() bool }
		if !errors.As(err, &u) || !u.Unconfirmed() {
			t.Fatalf("%d: want UNCONFIRMED, got %T: %v", reply.status, err, err)
		}
		if n := len(s.sent()); n != 1 {
			t.Fatalf("%d: POSTs = %d, want exactly 1", reply.status, n)
		}
	}
}

// A context already done sends nothing: every item is NOT_SENT and the call is a definite error.
func TestTwitter_AddNegativeKeywords_DeadlineSendsNothing(t *testing.T) {
	s := newXNegStub(t, xKWTLineItem, xCriteria, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.d.AddNegativeKeywords(ctx, "proj", model.ProviderTwitterAds, xBidCampaign(), []model.NegativeKeyword{broad("free")}, "")
	var u interface{ Unconfirmed() bool }
	if err == nil || errors.As(err, &u) {
		t.Fatalf("want a definite error, got %T: %v", err, err)
	}
	if len(s.sent()) != 0 {
		t.Fatal("a POST was sent after the deadline")
	}
}

func TestTwitter_AddNegativeKeywords_RefusalsPostNothing(t *testing.T) {
	noProvenance := xBidCampaign()
	noProvenance.Result = json.RawMessage(`{"CampaignID":"c1","LineItemID":"li1"}`)
	foreign := xBidCampaign()
	foreign.Result = json.RawMessage(`{"AccountID":"acc9","CampaignID":"c1","LineItemID":"li1"}`)
	noLineItem := xBidCampaign()
	noLineItem.Result = json.RawMessage(`{"AccountID":"acc1","CampaignID":"c1"}`)
	twentyOne := make([]model.NegativeKeyword, 21)
	for i := range twentyOne {
		twentyOne[i] = broad(fmt.Sprintf("kw%d", i))
	}
	cases := []struct {
		name     string
		campaign *model.Campaign
		lineItem string
		keywords []model.NegativeKeyword
		rev      string
		want     error
		zeroReqs bool
	}{
		{"over 20 per request", xBidCampaign(), xKWTLineItem, twentyOne, "", domain.ErrNegativeKeywordInvalid, true},
		{"unknown match type", xBidCampaign(), xKWTLineItem, []model.NegativeKeyword{{Text: "free", MatchType: "Unordered"}}, "", domain.ErrNegativeKeywordInvalid, true},
		{"duplicate", xBidCampaign(), xKWTLineItem, []model.NegativeKeyword{broad("free"), broad("FREE")}, "", domain.ErrNegativeKeywordInvalid, true},
		{"revision is reddit-only", xBidCampaign(), xKWTLineItem, []model.NegativeKeyword{broad("free")}, "sha256:x", domain.ErrNegativeKeywordInvalid, true},
		{"provenance unknown", noProvenance, xKWTLineItem, []model.NegativeKeyword{broad("free")}, "", domain.ErrCampaignProvenanceUnknown, true},
		{"account mismatch", foreign, xKWTLineItem, []model.NegativeKeyword{broad("free")}, "", domain.ErrCampaignAccountMismatch, false},
		{"no recorded line item", noLineItem, xKWTLineItem, []model.NegativeKeyword{broad("free")}, "", domain.ErrKeywordTargetingUnaddressable, true},
		{"line item of another campaign", xBidCampaign(), `{"data":{"id":"li1","campaign_id":"c9"}}`, []model.NegativeKeyword{broad("free")}, "", domain.ErrKeywordTargetingUnaddressable, false},
		{"line item deleted", xBidCampaign(), `{"data":{"id":"li1","campaign_id":"c1","deleted":true}}`, []model.NegativeKeyword{broad("free")}, "", domain.ErrKeywordTargetingUnaddressable, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newXNegStub(t, tc.lineItem, xCriteria, nil)
			_, err := addXNegatives(s, tc.campaign, tc.rev, tc.keywords...)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if len(s.sent()) != 0 {
				t.Fatal("a refusal issued a POST")
			}
			if tc.zeroReqs && s.count() != 0 {
				t.Fatalf("a local refusal reached X (%d requests)", s.count())
			}
		})
	}
	t.Run("unreadable criteria list", func(t *testing.T) {
		s := newXNegStub(t, xKWTLineItem, `{"data":null,"next_cursor":null}`, nil)
		if _, err := addXNegatives(s, xBidCampaign(), "", broad("free")); !errors.Is(err, domain.ErrKeywordTargetingUnaddressable) || len(s.sent()) != 0 {
			t.Fatalf("want ErrKeywordTargetingUnaddressable and no POST, got %v", err)
		}
	})
}

// The negative lever reads the same criteria list for its already-present check, so a list
// identityjson refuses is its read failure too — definite, and NO POST is issued.
func TestTwitter_AddNegativeKeywords_UntrustworthyListPostsNothing(t *testing.T) {
	s := newXNegStub(t, xKWTLineItem, xCriteriaDupID, nil)
	_, err := addXNegatives(s, xBidCampaign(), "", broad("free"))
	if !errors.Is(err, domain.ErrKeywordTargetingUnaddressable) || !errors.Is(err, twitter.ErrTargetingUnreadable) {
		t.Fatalf("want the unreadable-list refusal, got %v", err)
	}
	if n := len(s.sent()); n != 0 {
		t.Fatalf("an untrustworthy list issued %d POST(s)", n)
	}
}

// TestTwitterNegativeKey_AgreesAcrossBothSources pins the one property this key exists for: the
// PRESENT side (X's raw TargetingValue, normalised by nobody) and the REQUEST side (normalised by
// validateNegativeKeywordBatch through NormalizeNegativeKeywordText) must produce the same key for
// the same keyword.
//
// Without the collapse inside twitterNegativeKey these rows disagree, and the criterion reads as
// ABSENT and is sent again — a duplicate negated criterion on the line item. That is cheaper than
// the Google equivalent, where the same disagreement fails an ATOMIC batch, but it is the same
// defect: the key is built from two sources that do not arrive in the same shape.
//
// The NBSP case is the one no reader would predict: strings.Fields splits on it, and X is under no
// obligation to store one as a plain space.
func TestTwitterNegativeKey_AgreesAcrossBothSources(t *testing.T) {
	for _, tc := range []struct{ name, stored, requested string }{
		{"identical", "free download", "free download"},
		{"leading and trailing space on the stored criterion", " free download ", "free download"},
		{"a doubled internal space on the stored criterion", "free  download", "free download"},
		{"a non-breaking space on the stored criterion", "free download", "free download"},
		{"a tab on the stored criterion", "free\tdownload", "free download"},
		{"case difference, which the fold already covered", "Free Download", "free download"},
		{"both divergences at once", "  Free   DOWNLOAD ", "free download"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The request side as the dispatcher actually builds it: validated first, so the
			// test cannot pass by normalising the input itself.
			batch, err := validateNegativeKeywordBatch("x", []model.NegativeKeyword{
				{Text: tc.requested, MatchType: negativeMatchPhrase},
			}, maxTwitterNegativeKeywords, negativeMatchExact, negativeMatchPhrase, negativeMatchBroad)
			if err != nil {
				t.Fatalf("the requested text must survive validation: %v", err)
			}
			want := twitterNegativeKey(twitter.TargetingPhraseKeyword, batch[0].Text)
			got := twitterNegativeKey(twitter.TargetingPhraseKeyword, tc.stored)
			if got != want {
				t.Errorf("stored %q keys as %q, requested %q keys as %q — the criterion would read as absent and be sent again as a duplicate",
					tc.stored, got, tc.requested, want)
			}
		})
	}
}

// TestTwitterNegativeKey_StillSeparatesDifferentKeywords is the other half: the collapse must not
// make the key match MORE than X's own notion of one keyword. Over-matching reports a keyword
// ALREADY_PRESENT and never sends it, silently dropping an exclusion the caller asked for — worse
// than the duplicate the collapse prevents, because nothing surfaces it.
func TestTwitterNegativeKey_StillSeparatesDifferentKeywords(t *testing.T) {
	for _, tc := range []struct{ name, a, b string }{
		{"different words", "free download", "paid download"},
		{"a word boundary that the collapse must not erase", "free download", "freedownload"},
		{"a prefix is not the whole text", "free", "freedom"},
		{"punctuation is part of the text", "free-download", "free download"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if twitterNegativeKey(twitter.TargetingPhraseKeyword, tc.a) == twitterNegativeKey(twitter.TargetingPhraseKeyword, tc.b) {
				t.Errorf("%q and %q must not share a key: one would be reported ALREADY_PRESENT and never sent", tc.a, tc.b)
			}
		})
	}
}

// TestTwitterNegativeKey_TargetingTypeStillSeparates keeps the other half of the key honest: the
// same text at two targeting types is two different criteria, and X holds both.
func TestTwitterNegativeKey_TargetingTypeStillSeparates(t *testing.T) {
	text := "free download"
	if twitterNegativeKey(twitter.TargetingPhraseKeyword, text) == twitterNegativeKey(twitter.TargetingExactKeyword, text) {
		t.Fatal("PHRASE_KEYWORD and EXACT_KEYWORD must not share a key")
	}
	if twitterNegativeKey(twitter.TargetingBroadKeyword, text) == twitterNegativeKey(twitter.TargetingPhraseKeyword, text) {
		t.Fatal("BROAD_KEYWORD and PHRASE_KEYWORD must not share a key")
	}
}
