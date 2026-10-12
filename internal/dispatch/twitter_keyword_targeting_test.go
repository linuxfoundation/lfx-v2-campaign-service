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

const xKWTLineItem = `{"data":{"id":"li1","campaign_id":"c1","bid_strategy":"AUTO","deleted":false}}`

// xCriteria is the line item's criteria: three positive keywords, one negated keyword and one
// non-keyword criterion — only the first three are keyword TARGETING.
const xCriteria = `{"data":[
 {"id":"k1","line_item_id":"li1","targeting_type":"BROAD_KEYWORD","targeting_value":"kubernetes","operator_type":"EQ","deleted":false},
 {"id":"k2","line_item_id":"li1","targeting_type":"PHRASE_KEYWORD","targeting_value":"cloud native","operator_type":"EQ","deleted":false},
 {"id":"k3","line_item_id":"li1","targeting_type":"EXACT_KEYWORD","targeting_value":"ebpf","operator_type":"EQ","deleted":false},
 {"id":"n1","line_item_id":"li1","targeting_type":"BROAD_KEYWORD","targeting_value":"jobs","operator_type":"NE","deleted":false},
 {"id":"g1","line_item_id":"li1","targeting_type":"LOCATION","targeting_value":"96683cc9126741d1","operator_type":"EQ","deleted":false}
],"next_cursor":null}`

type xKWTStub struct {
	d        *TwitterDispatcher
	mu       sync.Mutex
	lineItem string
	// criteria answers the targeting_criteria lists in order; the last entry repeats.
	criteria []string
	deletes  map[string]kwtReply
	seen     []xBidRequest
}

func newXKWTStub(t *testing.T, lineItem, criteria string, deletes map[string]kwtReply) *xKWTStub {
	t.Helper()
	return newXKWTStubSeq(t, lineItem, []string{criteria}, deletes)
}

// newXKWTStubSeq answers successive targeting_criteria lists from criteria, so a test can make
// the targeting change between the guard's read and a later re-read.
func newXKWTStubSeq(t *testing.T, lineItem string, criteria []string, deletes map[string]kwtReply) *xKWTStub {
	t.Helper()
	s := &xKWTStub{lineItem: lineItem, criteria: criteria, deletes: deletes}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		s.mu.Lock()
		s.seen = append(s.seen, xBidRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery})
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodDelete:
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			reply, ok := deletes[id]
			if !ok {
				reply = kwtReply{http.StatusOK, `{"data":{"id":"` + id + `","deleted":true}}`}
			}
			w.WriteHeader(reply.status)
			_, _ = io.WriteString(w, reply.body)
		case strings.HasSuffix(r.URL.Path, "/targeting_criteria"):
			s.mu.Lock()
			body := s.criteria[0]
			if len(s.criteria) > 1 {
				s.criteria = s.criteria[1:]
			}
			s.mu.Unlock()
			_, _ = io.WriteString(w, body)
		default:
			_, _ = io.WriteString(w, s.lineItem)
		}
	}))
	t.Cleanup(srv.Close)
	s.d = NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0))
	return s
}

// requests is a locked snapshot of every request the handler recorded.
func (s *xKWTStub) requests() []xBidRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]xBidRequest(nil), s.seen...)
}

func (s *xKWTStub) deleted() []string {
	var out []string
	for _, r := range s.requests() {
		if r.Method == http.MethodDelete {
			out = append(out, r.Path)
		}
	}
	return out
}

func (s *xKWTStub) count() int {
	return len(s.requests())
}

func removeX(s *xKWTStub, c *model.Campaign, ids ...string) ([]model.KeywordTargetingOutcome, error) {
	rm := make([]model.KeywordTargetingRemoval, 0, len(ids))
	for _, id := range ids {
		rm = append(rm, model.KeywordTargetingRemoval{CriterionID: id})
	}
	return s.d.RemoveKeywordTargeting(context.Background(), "proj", model.ProviderTwitterAds, c, rm, "")
}

func TestTwitter_ReadKeywordTargeting_ListsOnlyPositiveKeywordCriteriaOfTheLineItem(t *testing.T) {
	s := newXKWTStub(t, xKWTLineItem, xCriteria, nil)
	kt, err := s.d.ReadKeywordTargeting(context.Background(), "proj", model.ProviderTwitterAds, xBidCampaign())
	if err != nil {
		t.Fatalf("ReadKeywordTargeting: %v", err)
	}
	if kt.EntityID != "li1" || kt.Revision != "" || len(kt.Keywords) != 3 {
		t.Fatalf("unexpected targeting: %+v", kt)
	}
	if k := kt.Keywords[1]; k.Keyword != "cloud native" || k.CriterionID != "k2" || k.MatchType != twitter.TargetingPhraseKeyword {
		t.Errorf("keyword[1] = %+v", k)
	}
	var listed bool
	for _, r := range s.requests() {
		if strings.HasSuffix(r.Path, "/12/accounts/acc1/targeting_criteria") {
			listed = strings.Contains(r.Query, "line_item_ids=li1") && strings.Contains(r.Query, "with_deleted=false")
		}
	}
	if !listed {
		t.Errorf("the list was not scoped to the line item: %+v", s.requests())
	}
	if len(s.deleted()) != 0 {
		t.Fatal("a read must not write")
	}
}

// What every X campaign this service creates looks like: no targeting criteria at all.
func TestTwitter_ReadKeywordTargeting_CreatedCampaignHasNone(t *testing.T) {
	s := newXKWTStub(t, xKWTLineItem, `{"data":[],"next_cursor":null}`, nil)
	kt, err := s.d.ReadKeywordTargeting(context.Background(), "proj", model.ProviderTwitterAds, xBidCampaign())
	if err != nil || kt.Keywords == nil || len(kt.Keywords) != 0 {
		t.Fatalf("want an empty, non-nil list, got %+v, %v", kt, err)
	}
}

func TestTwitter_KeywordTargeting_RefusesWhatItCannotProve(t *testing.T) {
	cases := []struct {
		name, lineItem, criteria string
	}{
		{"line item of another campaign", `{"data":{"id":"li1","campaign_id":"c9"}}`, xCriteria},
		{"line item deleted", `{"data":{"id":"li1","campaign_id":"c1","deleted":true}}`, xCriteria},
		{"criteria of another line item in the answer", xKWTLineItem, `{"data":[{"id":"k1","line_item_id":"li9","targeting_type":"BROAD_KEYWORD","targeting_value":"a","operator_type":"EQ"}],"next_cursor":null}`},
		{"no result set", xKWTLineItem, `{"data":null,"next_cursor":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newXKWTStub(t, tc.lineItem, tc.criteria, nil)
			_, err := s.d.ReadKeywordTargeting(context.Background(), "proj", model.ProviderTwitterAds, xBidCampaign())
			if !errors.Is(err, domain.ErrKeywordTargetingUnaddressable) {
				t.Fatalf("read: want ErrKeywordTargetingUnaddressable, got %v", err)
			}
			if _, err := removeX(s, xBidCampaign(), "k1"); !errors.Is(err, domain.ErrKeywordTargetingUnaddressable) {
				t.Fatalf("remove: want ErrKeywordTargetingUnaddressable, got %v", err)
			}
			if len(s.deleted()) != 0 {
				t.Fatal("a refusal issued a DELETE")
			}
		})
	}
}

func TestTwitter_RemoveKeywordTargeting_PerItemOutcomesInRequestOrder(t *testing.T) {
	s := newXKWTStub(t, xKWTLineItem, xCriteria, map[string]kwtReply{
		"k2": {http.StatusNotFound, `{"errors":[{"code":"NOT_FOUND"}]}`},
	})
	// Two of the three positive keywords (n1 is negated and does not count, so removing all three
	// would be refused): k2 FAILED/NOT_FOUND, then k1 APPLIED — reported in request order.
	out, err := removeX(s, xBidCampaign(), "k2", "k1")
	if err != nil {
		t.Fatalf("RemoveKeywordTargeting: %v", err)
	}
	if len(out) != 2 || out[0].CriterionID != "k2" || out[0].Outcome != model.KeywordOutcomeFailed || out[0].ErrorCode != model.KeywordTargetingErrNotFound ||
		out[1].CriterionID != "k1" || out[1].Outcome != model.KeywordOutcomeApplied {
		t.Fatalf("outcomes = %+v", out)
	}
	if d := s.deleted(); len(d) != 2 || d[0] != "/12/accounts/acc1/targeting_criteria/k2" || d[1] != "/12/accounts/acc1/targeting_criteria/k1" {
		t.Errorf("DELETEs = %v", d)
	}

	s = newXKWTStub(t, xKWTLineItem, xCriteria, map[string]kwtReply{"k3": {http.StatusServiceUnavailable, `{}`}})
	out, err = removeX(s, xBidCampaign(), "k1", "k3")
	if err != nil {
		t.Fatalf("RemoveKeywordTargeting: %v", err)
	}
	if out[0].Outcome != model.KeywordOutcomeApplied || out[1].Outcome != model.KeywordOutcomeUnconfirmed {
		t.Fatalf("outcomes = %+v", out)
	}
}

func TestTwitter_RemoveKeywordTargeting_NoDefiniteAnswerIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply kwtReply
	}{
		{"429 (never retried, so unconfirmed)", kwtReply{http.StatusTooManyRequests, `{}`}},
		{"2xx not reporting the delete", kwtReply{http.StatusOK, `{"data":{"id":"k1","deleted":false}}`}},
		{"2xx naming another criterion", kwtReply{http.StatusOK, `{"data":{"id":"k9","deleted":true}}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newXKWTStub(t, xKWTLineItem, xCriteria, map[string]kwtReply{"k1": tc.reply})
			_, err := removeX(s, xBidCampaign(), "k1")
			var u interface{ Unconfirmed() bool }
			if !errors.As(err, &u) || !u.Unconfirmed() {
				t.Fatalf("want UNCONFIRMED, got %T: %v", err, err)
			}
		})
	}
}

func TestTwitter_RemoveKeywordTargeting_RefusalsDeleteNothing(t *testing.T) {
	noProvenance := xBidCampaign()
	noProvenance.Result = json.RawMessage(`{"CampaignID":"c1","LineItemID":"li1"}`)
	cases := []struct {
		name     string
		campaign *model.Campaign
		ids      []string
		rev      string
		want     error
		zeroReqs bool
	}{
		{"malformed id", xBidCampaign(), []string{"k/../1"}, "", domain.ErrKeywordTargetingInvalid, true},
		{"duplicate", xBidCampaign(), []string{"k1", "k1"}, "", domain.ErrKeywordTargetingInvalid, true},
		{"revision is reddit-only", xBidCampaign(), []string{"k1"}, "sha256:x", domain.ErrKeywordTargetingInvalid, true},
		{"provenance unknown", noProvenance, []string{"k1"}, "", domain.ErrCampaignProvenanceUnknown, true},
		{"criterion of no keyword of this line item", xBidCampaign(), []string{"zz9"}, "", domain.ErrKeywordTargetingInvalid, false},
		{"negated keyword is not targeting", xBidCampaign(), []string{"n1"}, "", domain.ErrKeywordTargetingInvalid, false},
		{"would remove every keyword", xBidCampaign(), []string{"k1", "k2", "k3"}, "", domain.ErrKeywordTargetingWouldEmpty, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newXKWTStub(t, xKWTLineItem, xCriteria, nil)
			rm := make([]model.KeywordTargetingRemoval, 0, len(tc.ids))
			for _, id := range tc.ids {
				rm = append(rm, model.KeywordTargetingRemoval{CriterionID: id})
			}
			_, err := s.d.RemoveKeywordTargeting(context.Background(), "proj", model.ProviderTwitterAds, tc.campaign, rm, tc.rev)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if len(s.deleted()) != 0 {
				t.Fatal("a refusal issued a DELETE")
			}
			if tc.zeroReqs && s.count() != 0 {
				t.Fatalf("a local refusal reached X (%d requests)", s.count())
			}
		})
	}
}

const xTwoKeywords = `{"data":[
 {"id":"c1","line_item_id":"li1","targeting_type":"BROAD_KEYWORD","targeting_value":"kubernetes","operator_type":"EQ"},
 {"id":"c2","line_item_id":"li1","targeting_type":"BROAD_KEYWORD","targeting_value":"ebpf","operator_type":"EQ"}
],"next_cursor":null}`

const xOnlyC1 = `{"data":[
 {"id":"c1","line_item_id":"li1","targeting_type":"BROAD_KEYWORD","targeting_value":"kubernetes","operator_type":"EQ"}
],"next_cursor":null}`

// The race the per-DELETE re-list closes: live = {c1, c2}; this request removes c1 while a
// concurrent removal of c2 lands between the guard's list and this DELETE. Both requests passed
// the one-shot guard; without the re-list they would together empty the line item.
func TestTwitter_RemoveKeywordTargeting_ConcurrentRemovalCannotEmptyTheLineItem(t *testing.T) {
	s := newXKWTStubSeq(t, xKWTLineItem, []string{xTwoKeywords, xOnlyC1}, nil)
	out, err := removeX(s, xBidCampaign(), "c1")
	if err != nil {
		t.Fatalf("RemoveKeywordTargeting: %v", err)
	}
	if len(out) != 1 || out[0].Outcome != model.KeywordOutcomeFailed || out[0].ErrorCode != model.KeywordTargetingErrWouldEmpty {
		t.Fatalf("outcomes = %+v, want FAILED/WOULD_EMPTY", out)
	}
	if d := s.deleted(); len(d) != 0 {
		t.Fatalf("the DELETE that would empty the line item was sent: %v", d)
	}
}

// A criterion removed concurrently is reported NOT_FOUND without a DELETE being sent.
func TestTwitter_RemoveKeywordTargeting_CriterionGoneBeforeItsDeleteIsNotSent(t *testing.T) {
	s := newXKWTStubSeq(t, xKWTLineItem, []string{xCriteria, `{"data":[
 {"id":"k1","line_item_id":"li1","targeting_type":"BROAD_KEYWORD","targeting_value":"kubernetes","operator_type":"EQ"},
 {"id":"k3","line_item_id":"li1","targeting_type":"EXACT_KEYWORD","targeting_value":"ebpf","operator_type":"EQ"}
],"next_cursor":null}`}, nil)
	out, err := removeX(s, xBidCampaign(), "k2")
	if err != nil {
		t.Fatalf("RemoveKeywordTargeting: %v", err)
	}
	if out[0].Outcome != model.KeywordOutcomeFailed || out[0].ErrorCode != model.KeywordTargetingErrNotFound || len(s.deleted()) != 0 {
		t.Fatalf("outcomes = %+v, deletes %v", out, s.deleted())
	}
}
