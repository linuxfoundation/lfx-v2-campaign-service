// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

func enableXKeywordAdd(t *testing.T) {
	t.Helper()
	t.Setenv(constants.EnvTwitterKeywordAddEnabled, "true")
}

func addXKeywords(s *xNegStub, c *model.Campaign, keywords ...string) ([]model.KeywordTargetingOutcome, error) {
	return s.d.AddKeywordTargeting(context.Background(), "proj", model.ProviderTwitterAds, c, keywords)
}

// xCriteriaWith returns a criteria list of n live LOCATION criteria plus extra raw entries.
func xCriteriaWith(n int, extra ...string) string {
	entries := make([]string, 0, n+len(extra))
	for i := 0; i < n; i++ {
		entries = append(entries, fmt.Sprintf(`{"id":"g%d","line_item_id":"li1","targeting_type":"LOCATION","targeting_value":"loc%d","operator_type":"EQ","deleted":false}`, i, i))
	}
	entries = append(entries, extra...)
	return `{"data":[` + strings.Join(entries, ",") + `],"next_cursor":null}`
}

// Off unless TWITTER_KEYWORD_ADD_ENABLED is exactly "true": refused before X is contacted.
func TestTwitter_AddKeywordTargeting_GatedOffByDefault(t *testing.T) {
	for _, v := range []string{"", "TRUE", "1"} {
		t.Setenv(constants.EnvTwitterKeywordAddEnabled, v)
		s := newXNegStub(t, xKWTLineItem, xCriteria, nil)
		if _, err := addXKeywords(s, xBidCampaign(), "kubernetes"); !errors.Is(err, domain.ErrKeywordTargetingUnsupported) {
			t.Errorf("flag %q: want ErrKeywordTargetingUnsupported, got %v", v, err)
		}
		if n := s.count(); n != 0 {
			t.Errorf("flag %q: %d requests sent with the gate off", v, n)
		}
	}
}

// One BROAD_KEYWORD / EQ POST per keyword not already targeted as a broad keyword; a live broad
// keyword with the same text (ignoring case) is ALREADY_PRESENT with its id and not sent; a PHRASE
// keyword with the same text is a different criterion and does not count.
func TestTwitter_AddKeywordTargeting_OnePostPerNewKeywordIdempotent(t *testing.T) {
	enableXKeywordAdd(t)
	s := newXNegStub(t, xKWTLineItem, xCriteria, nil)
	out, err := addXKeywords(s, xBidCampaign(), "Kubernetes", "cloud native", "service mesh")
	if err != nil {
		t.Fatalf("AddKeywordTargeting: %v", err)
	}
	want := []model.KeywordTargetingOutcome{
		{Keyword: "Kubernetes", CriterionID: "k1", Outcome: model.KeywordOutcomeAlreadyPresent},
		{Keyword: "cloud native", CriterionID: "neg1", Outcome: model.KeywordOutcomeApplied},
		{Keyword: "service mesh", CriterionID: "neg2", Outcome: model.KeywordOutcomeApplied},
	}
	if len(out) != len(want) {
		t.Fatalf("outcomes = %+v", out)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Errorf("outcome %d = %+v, want %+v", i, out[i], want[i])
		}
	}
	posts := s.sent()
	if len(posts) != 2 {
		t.Fatalf("POSTs = %v", posts)
	}
	for i, kw := range []string{"cloud native", "service mesh"} {
		q := posts[i]
		if q.Get("targeting_type") != "BROAD_KEYWORD" || q.Get("operator_type") != "EQ" || q.Get("targeting_value") != kw || q.Get("line_item_id") != "li1" {
			t.Errorf("POST %d = %v, want BROAD_KEYWORD %q EQ on li1", i, q, kw)
		}
	}

	// A repeat of a fully present request sends nothing.
	s2 := newXNegStub(t, xKWTLineItem, xCriteria, nil)
	out, err = addXKeywords(s2, xBidCampaign(), "kubernetes")
	if err != nil || len(out) != 1 || out[0].Outcome != model.KeywordOutcomeAlreadyPresent || len(s2.sent()) != 0 {
		t.Errorf("repeat add = %+v, %v, %d POSTs; want ALREADY_PRESENT and nothing sent", out, err, len(s2.sent()))
	}
}

// The line item is capped at 60 criteria, counting every live criterion it holds now (deleted ones
// do not count) plus the keywords this request would send; a keyword already present costs nothing.
func TestTwitter_AddKeywordTargeting_CountsExistingCriteriaAgainstTheCap(t *testing.T) {
	enableXKeywordAdd(t)
	present := `{"id":"k1","line_item_id":"li1","targeting_type":"BROAD_KEYWORD","targeting_value":"kubernetes","operator_type":"EQ","deleted":false}`
	gone := `{"id":"d1","line_item_id":"li1","targeting_type":"LOCATION","targeting_value":"old","operator_type":"EQ","deleted":true}`

	// 58 locations + 1 keyword = 59 live; two new keywords make 61.
	s := newXNegStub(t, xKWTLineItem, xCriteriaWith(58, present, gone), nil)
	if _, err := addXKeywords(s, xBidCampaign(), "ebpf", "wasm"); !errors.Is(err, domain.ErrKeywordTargetingLimit) {
		t.Fatalf("want ErrKeywordTargetingLimit, got %v", err)
	}
	if n := len(s.sent()); n != 0 {
		t.Errorf("%d POSTs sent over the cap", n)
	}

	// One new keyword (60) plus one already present is accepted.
	s = newXNegStub(t, xKWTLineItem, xCriteriaWith(58, present, gone), nil)
	out, err := addXKeywords(s, xBidCampaign(), "ebpf", "kubernetes")
	if err != nil || len(out) != 2 || out[0].Outcome != model.KeywordOutcomeApplied || out[1].Outcome != model.KeywordOutcomeAlreadyPresent {
		t.Fatalf("at the cap: %+v, %v", out, err)
	}
	if n := len(s.sent()); n != 1 {
		t.Errorf("%d POSTs, want 1", n)
	}
}

// A malformed batch is refused before X is contacted.
func TestTwitter_AddKeywordTargeting_InvalidBatchSendsNothing(t *testing.T) {
	enableXKeywordAdd(t)
	for name, kws := range map[string][]string{
		"empty":    nil,
		"blank":    {" "},
		"too long": {strings.Repeat("k", 81)},
		"control":  {"a\nb"},
	} {
		s := newXNegStub(t, xKWTLineItem, xCriteria, nil)
		if _, err := addXKeywords(s, xBidCampaign(), kws...); !errors.Is(err, domain.ErrKeywordTargetingInvalid) {
			t.Errorf("%s: want ErrKeywordTargetingInvalid, got %v", name, err)
		}
		if n := s.count(); n != 0 {
			t.Errorf("%s: %d requests sent", name, n)
		}
	}
}

// Provenance fails closed, and a line item reported under another campaign is refused, both before
// any POST.
func TestTwitter_AddKeywordTargeting_OwnershipBeforeAnyWrite(t *testing.T) {
	enableXKeywordAdd(t)
	s := newXNegStub(t, xKWTLineItem, xCriteria, nil)
	c := xBidCampaign()
	c.Result = json.RawMessage(`{"CampaignID":"c1","LineItemID":"li1"}`)
	if _, err := addXKeywords(s, c, "ebpf"); !errors.Is(err, domain.ErrCampaignProvenanceUnknown) {
		t.Errorf("no recorded account: want ErrCampaignProvenanceUnknown, got %v", err)
	}
	s = newXNegStub(t, `{"data":{"id":"li1","campaign_id":"other","deleted":false}}`, xCriteria, nil)
	if _, err := addXKeywords(s, xBidCampaign(), "ebpf"); !errors.Is(err, domain.ErrKeywordTargetingUnaddressable) {
		t.Errorf("foreign line item: want ErrKeywordTargetingUnaddressable, got %v", err)
	}
	if n := len(s.sent()); n != 0 {
		t.Errorf("%d POSTs sent", n)
	}
}

// Per-item outcomes: a refused POST is FAILED/REJECTED and the rest still go; when every POST is
// UNCONFIRMED the call is one UNCONFIRMED error.
func TestTwitter_AddKeywordTargeting_PerItemOutcomes(t *testing.T) {
	enableXKeywordAdd(t)
	s := newXNegStub(t, xKWTLineItem, xCriteria, map[string]kwtReply{
		"ebpf": {http.StatusBadRequest, `{"errors":[{"code":"INVALID_PARAMETER"}]}`},
		"wasm": {http.StatusBadGateway, `{}`},
	})
	out, err := addXKeywords(s, xBidCampaign(), "ebpf", "wasm", "otel")
	if err != nil || len(out) != 3 {
		t.Fatalf("AddKeywordTargeting = %+v, %v", out, err)
	}
	if out[0].Outcome != model.KeywordOutcomeFailed || out[0].ErrorCode != model.KeywordTargetingErrRejected ||
		out[1].Outcome != model.KeywordOutcomeUnconfirmed || out[2].Outcome != model.KeywordOutcomeApplied {
		t.Errorf("outcomes = %+v", out)
	}

	s = newXNegStub(t, xKWTLineItem, xCriteria, map[string]kwtReply{"wasm": {http.StatusBadGateway, `{}`}})
	_, err = addXKeywords(s, xBidCampaign(), "wasm")
	var u interface{ Unconfirmed() bool }
	if !errors.As(err, &u) || !u.Unconfirmed() {
		t.Errorf("every item UNCONFIRMED: want an UNCONFIRMED error, got %v", err)
	}
}

// THE LOCK. Another holder of the line item's lock refuses the add with nothing read or sent; a
// settled add releases at once; an UNCONFIRMED add keeps the lock for the cooldown, so the next
// add cannot re-send the keyword X may still be applying.
func TestTwitter_AddKeywordTargeting_HoldsThePerLineItemLock(t *testing.T) {
	enableXKeywordAdd(t)
	short := func(s *xNegStub) {
		s.d.targetingLock = newLocalTargetingLock(50 * time.Millisecond)
		s.d.targetingCooldown = 2 * time.Second
	}

	s := newXNegStub(t, xKWTLineItem, xCriteria, nil)
	short(s)
	hold, err := s.d.targetingLock.Lock(context.Background(), twitterTargetingLockScope("acc1", "li1"))
	if err != nil {
		t.Fatalf("pre-lock: %v", err)
	}
	before := s.count()
	if _, err := addXKeywords(s, xBidCampaign(), "ebpf"); !errors.Is(err, domain.ErrKeywordTargetingWriteInProgress) {
		t.Errorf("lock held elsewhere: want ErrKeywordTargetingWriteInProgress, got %v", err)
	}
	s.mu.Lock()
	after := append([]xBidRequest(nil), s.all[before:]...)
	s.mu.Unlock()
	for _, r := range after {
		if strings.HasSuffix(r.Path, "/targeting_criteria") {
			t.Errorf("lock held elsewhere, yet %s %s was sent", r.Method, r.Path)
		}
	}
	hold.Release()

	// Settled: released at once, so a second add proceeds.
	if _, err := addXKeywords(s, xBidCampaign(), "ebpf"); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if _, err := addXKeywords(s, xBidCampaign(), "wasm"); err != nil {
		t.Errorf("an add after a settled add must not wait on the lock: %v", err)
	}

	// Unsettled: kept for the cooldown.
	s = newXNegStub(t, xKWTLineItem, xCriteria, map[string]kwtReply{"wasm": {http.StatusBadGateway, `{}`}})
	short(s)
	if _, err := addXKeywords(s, xBidCampaign(), "wasm"); err == nil {
		t.Fatal("want the UNCONFIRMED error")
	}
	if _, err := addXKeywords(s, xBidCampaign(), "wasm"); !errors.Is(err, domain.ErrKeywordTargetingWriteInProgress) {
		t.Errorf("an add inside the cooldown of an UNCONFIRMED add: want ErrKeywordTargetingWriteInProgress, got %v", err)
	}
}
