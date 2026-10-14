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

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// redditExcludedBody is redditTargetingBody with an `excluded_keywords` member holding excluded
// (raw JSON), or without the member when excluded is "".
func redditExcludedBody(excluded string) string {
	obj := redditTargetingObject(`["kubernetes"]`)
	if excluded != "" {
		obj = strings.Replace(obj, `"keywords":`, `"excluded_keywords":`+excluded+`,"keywords":`, 1)
	}
	return `{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":` + obj + `}}`
}

// redditRevisionOf reads body once through the dispatcher, for the revision a caller holds.
func redditRevisionOf(t *testing.T, body string) string {
	t.Helper()
	s := newRedditKWTStub(t, kwtReply{http.StatusOK, `{}`}, kwtReply{http.StatusOK, body})
	kt, err := s.d.ReadKeywordTargeting(context.Background(), "proj", model.ProviderRedditAds, redditBudgetCampaign())
	if err != nil {
		t.Fatalf("ReadKeywordTargeting: %v", err)
	}
	return kt.Revision
}

func addRedditNegatives(s *redditKWTStub, c *model.Campaign, rev string, keywords ...model.NegativeKeyword) ([]model.NegativeKeywordOutcome, error) {
	return s.d.AddNegativeKeywords(context.Background(), "proj", model.ProviderRedditAds, c, keywords, rev)
}

func broad(text string) model.NegativeKeyword {
	return model.NegativeKeyword{Text: text, MatchType: "Broad"}
}

func sentTargeting(t *testing.T, body string) map[string]json.RawMessage {
	t.Helper()
	var sent struct {
		Data struct {
			Targeting map[string]json.RawMessage `json:"targeting"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("PATCH body: %v", err)
	}
	return sent.Data.Targeting
}

func TestReddit_AddNegativeKeywords_OffByDefault(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "TRUE")
	s := newRedditKWTStub(t, kwtReply{http.StatusOK, `{}`}, kwtReply{http.StatusOK, redditExcludedBody("")})
	_, err := addRedditNegatives(s, redditBudgetCampaign(), "sha256:x", broad("free"))
	if !errors.Is(err, domain.ErrNegativeKeywordsUnsupported) || s.count() != 0 {
		t.Fatalf("anything but exactly \"true\" must refuse with zero requests, got %v after %d", err, s.count())
	}
}

// The new keyword is appended to excluded_keywords, an already-excluded one is ALREADY_PRESENT
// and not resent, every other member goes back exactly as read, and the re-read confirms.
func TestReddit_AddNegativeKeywords_AppendsToExcludedKeywordsThenConfirms(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "true")
	before := redditExcludedBody(`["jobs",null]`)
	rev := redditRevisionOf(t, before)
	s := newRedditKWTStub(t, kwtReply{http.StatusOK, `{"data":{"id":"t5_ag"}}`},
		kwtReply{http.StatusOK, before},
		kwtReply{http.StatusOK, redditExcludedBody(`["jobs",null,"free download"]`)},
	)
	out, err := addRedditNegatives(s, redditBudgetCampaign(), rev, broad("  free   download "), model.NegativeKeyword{Text: "jobs", MatchType: "broad"})
	if err != nil {
		t.Fatalf("AddNegativeKeywords: %v", err)
	}
	if len(out) != 2 || out[0].Text != "free download" || out[0].MatchType != "Broad" || out[0].Outcome != model.KeywordOutcomeApplied ||
		out[1].Outcome != model.KeywordOutcomeAlreadyPresent || out[0].NegativeKeywordID != "" {
		t.Fatalf("outcomes = %+v", out)
	}
	p := s.patches()
	if len(p) != 1 || p[0].Path != redditBidAdGroupPath {
		t.Fatalf("want one PATCH of the ad group, got %+v", p)
	}
	tg := sentTargeting(t, p[0].Body)
	if string(tg["excluded_keywords"]) != `["jobs",null,"free download"]` {
		t.Errorf("excluded_keywords sent = %s", tg["excluded_keywords"])
	}
	var read map[string]json.RawMessage
	if err := json.Unmarshal([]byte(redditTargetingObject(`["kubernetes"]`)), &read); err != nil {
		t.Fatal(err)
	}
	for k, v := range read {
		if g, w := compactRaw(t, tg[k]), compactRaw(t, v); g != w {
			t.Errorf("%s sent = %s, read %s", k, g, w)
		}
	}
	if len(tg) != len(read)+1 {
		t.Errorf("sent %d targeting members, want the %d read", len(tg), len(read)+1)
	}
}

func TestReddit_AddNegativeKeywords_CreatesAnAbsentMember(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "true")
	rev := redditRevisionOf(t, redditExcludedBody(""))
	s := newRedditKWTStub(t, kwtReply{http.StatusOK, `{}`},
		kwtReply{http.StatusOK, redditExcludedBody("")},
		kwtReply{http.StatusOK, redditExcludedBody(`["free"]`)},
	)
	out, err := addRedditNegatives(s, redditBudgetCampaign(), rev, broad("free"))
	if err != nil || len(out) != 1 || out[0].Outcome != model.KeywordOutcomeApplied {
		t.Fatalf("AddNegativeKeywords = %+v, %v", out, err)
	}
	if got := string(sentTargeting(t, s.patches()[0].Body)["excluded_keywords"]); got != `["free"]` {
		t.Errorf("excluded_keywords sent = %s", got)
	}
}

// Nothing new to add is a success with no write at all.
func TestReddit_AddNegativeKeywords_AllAlreadyPresentWritesNothing(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "true")
	body := redditExcludedBody(`["free","jobs"]`)
	rev := redditRevisionOf(t, body)
	s := newRedditKWTStub(t, kwtReply{http.StatusOK, `{}`}, kwtReply{http.StatusOK, body})
	out, err := addRedditNegatives(s, redditBudgetCampaign(), rev, broad("jobs"), broad("free"))
	if err != nil || len(out) != 2 || out[0].Outcome != model.KeywordOutcomeAlreadyPresent || out[1].Outcome != model.KeywordOutcomeAlreadyPresent {
		t.Fatalf("AddNegativeKeywords = %+v, %v", out, err)
	}
	if len(s.patches()) != 0 {
		t.Fatal("nothing to add, but a PATCH was sent")
	}
}

func TestReddit_AddNegativeKeywords_RefusalsWriteNothing(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "true")
	body := redditExcludedBody(`["jobs"]`)
	rev := redditRevisionOf(t, body)
	many := make([]string, 2000)
	for i := range many {
		many[i] = fmt.Sprintf("%q", fmt.Sprintf("kw%d", i))
	}
	full := redditExcludedBody("[" + strings.Join(many, ",") + "]")
	fullRev := redditRevisionOf(t, full)
	// 1,999 keywords and one null: 2,000 elements already, so one more keyword is 2,001 on the
	// wire even though only 1,999 keywords are excluded (PR #321 review).
	nullPadded := redditExcludedBody("[" + strings.Join(many[:1999], ",") + ",null]")
	nullPaddedRev := redditRevisionOf(t, nullPadded)
	cases := []struct {
		name     string
		campaign *model.Campaign
		get      string
		rev      string
		keywords []model.NegativeKeyword
		want     error
		zeroReqs bool
	}{
		{"exact is not a reddit match type", redditBudgetCampaign(), body, rev, []model.NegativeKeyword{{Text: "free", MatchType: "Exact"}}, domain.ErrNegativeKeywordInvalid, true},
		{"phrase is not a reddit match type", redditBudgetCampaign(), body, rev, []model.NegativeKeyword{{Text: "free", MatchType: "Phrase"}}, domain.ErrNegativeKeywordInvalid, true},
		{"disallowed character", redditBudgetCampaign(), body, rev, []model.NegativeKeyword{broad("a@b")}, domain.ErrNegativeKeywordInvalid, true},
		{"duplicate ignoring case", redditBudgetCampaign(), body, rev, []model.NegativeKeyword{broad("Free"), broad("free")}, domain.ErrNegativeKeywordInvalid, true},
		{"duplicate under unicode simple folding", redditBudgetCampaign(), body, rev, []model.NegativeKeyword{broad("ΟΣ"), broad("ος")}, domain.ErrNegativeKeywordInvalid, true},
		{"revision missing", redditBudgetCampaign(), body, "", []model.NegativeKeyword{broad("free")}, domain.ErrNegativeKeywordInvalid, true},
		{"revision blank", redditBudgetCampaign(), body, "   ", []model.NegativeKeyword{broad("free")}, domain.ErrNegativeKeywordInvalid, true},
		{"provenance unknown", &model.Campaign{ID: "camp-1", Platform: model.ProviderRedditAds, PlatformCampaignID: "t3_c",
			Result: json.RawMessage(`{"adGroupId":"t5_ag"}`)}, body, rev, []model.NegativeKeyword{broad("free")}, domain.ErrCampaignProvenanceUnknown, true},
		{"unprovisioned", &model.Campaign{ID: "camp-1", Platform: model.ProviderRedditAds}, body, rev, []model.NegativeKeyword{broad("free")}, domain.ErrCampaignNotProvisioned, true},
		{"ad group of another campaign", redditBudgetCampaign(), `{"data":{"id":"t5_ag","campaign_id":"t3_other","targeting":{"keywords":["a"]}}}`, rev,
			[]model.NegativeKeyword{broad("free")}, domain.ErrKeywordTargetingUnaddressable, false},
		{"stale revision", redditBudgetCampaign(), body, "sha256:stale", []model.NegativeKeyword{broad("free")}, domain.ErrKeywordTargetingChanged, false},
		{"excluded keywords not a string list", redditBudgetCampaign(), redditExcludedBody(`{"a":1}`), redditRevisionOf(t, redditExcludedBody(`{"a":1}`)),
			[]model.NegativeKeyword{broad("free")}, domain.ErrKeywordTargetingUnaddressable, false},
		{"over reddit's 2000", redditBudgetCampaign(), full, fullRev, []model.NegativeKeyword{broad("free")}, domain.ErrNegativeKeywordLimit, false},
		{"null elements count toward reddit's 2000", redditBudgetCampaign(), nullPadded, nullPaddedRev, []model.NegativeKeyword{broad("free")}, domain.ErrNegativeKeywordLimit, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRedditKWTStub(t, kwtReply{http.StatusOK, `{}`}, kwtReply{http.StatusOK, tc.get})
			_, err := addRedditNegatives(s, tc.campaign, tc.rev, tc.keywords...)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if len(s.patches()) != 0 {
				t.Fatal("a refusal issued a PATCH")
			}
			if tc.zeroReqs && s.count() != 0 {
				t.Fatalf("a local refusal reached Reddit (%d requests)", s.count())
			}
		})
	}
}

func TestReddit_AddNegativeKeywords_AmbiguityIsUnconfirmed(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "true")
	body := redditExcludedBody(`["jobs"]`)
	rev := redditRevisionOf(t, body)
	read := kwtReply{http.StatusOK, body}
	cases := []struct {
		name  string
		patch kwtReply
		after kwtReply
	}{
		{"5xx on the PATCH", kwtReply{http.StatusBadGateway, `{}`}, read},
		{"429 on the PATCH", kwtReply{http.StatusTooManyRequests, `{}`}, read},
		{"re-read shows the keyword missing", kwtReply{http.StatusOK, `{}`}, read},
		{"re-read shows another dimension moved", kwtReply{http.StatusOK, `{}`},
			kwtReply{http.StatusOK, `{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":{"geolocations":["GB"],"keywords":["kubernetes"],"excluded_keywords":["jobs","free"]}}}`}},
		{"echo with another excluded list", kwtReply{http.StatusOK, `{"data":{"id":"t5_ag","targeting":{"keywords":["kubernetes"],"excluded_keywords":["jobs"]}}}`}, read},
		{"echo naming another ad group", kwtReply{http.StatusOK, `{"data":{"id":"t5_other"}}`}, read},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRedditKWTStub(t, tc.patch, read, tc.after)
			_, err := addRedditNegatives(s, redditBudgetCampaign(), rev, broad("free"))
			var u interface{ Unconfirmed() bool }
			if !errors.As(err, &u) || !u.Unconfirmed() {
				t.Fatalf("want UNCONFIRMED, got %T: %v", err, err)
			}
			if n := len(s.patches()); n != 1 {
				t.Fatalf("the PATCH must be sent exactly once, got %d", n)
			}
		})
	}
	// A definite refusal is a KNOWN outcome, so it is answered per item (as X answers it) rather
	// than as a batch-wide error; only an all-UNCONFIRMED batch is one (PR #321 review).
	t.Run("a definite 400 is answered per item as REJECTED", func(t *testing.T) {
		s := newRedditKWTStub(t, kwtReply{http.StatusBadRequest, `{"error":{}}`}, read)
		out, err := addRedditNegatives(s, redditBudgetCampaign(), rev, broad("free"))
		if err != nil || len(out) != 1 || out[0].Outcome != model.KeywordOutcomeFailed || out[0].ErrorCode != model.KeywordTargetingErrRejected {
			t.Fatalf("want [FAILED/REJECTED], got %+v, %v", out, err)
		}
	})
}

// A batch mixing an already-excluded keyword with a new one answers PER ITEM whatever happens to
// the write: the ALREADY_PRESENT result is known before the PATCH is sent and is never discarded,
// and the new keyword carries the write's outcome (PR #321 review).
func TestReddit_AddNegativeKeywords_MixedBatchKeepsAlreadyPresentWhenTheWriteFails(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "true")
	body := redditExcludedBody(`["jobs"]`)
	rev := redditRevisionOf(t, body)
	read := kwtReply{http.StatusOK, body}
	cases := []struct {
		name    string
		patch   kwtReply
		after   kwtReply
		outcome string
		code    string
	}{
		{"definite 400 on the PATCH", kwtReply{http.StatusBadRequest, `{"error":{"message":"secret upstream text"}}`}, read, model.KeywordOutcomeFailed, model.KeywordTargetingErrRejected},
		{"5xx on the PATCH", kwtReply{http.StatusBadGateway, `{}`}, read, model.KeywordOutcomeUnconfirmed, ""},
		{"429 on the PATCH", kwtReply{http.StatusTooManyRequests, `{}`}, read, model.KeywordOutcomeUnconfirmed, ""},
		{"echo with another excluded list", kwtReply{http.StatusOK, `{"data":{"id":"t5_ag","targeting":{"keywords":["kubernetes"],"excluded_keywords":["jobs"]}}}`}, read, model.KeywordOutcomeUnconfirmed, ""},
		{"re-read finds no ad group", kwtReply{http.StatusOK, `{}`}, kwtReply{http.StatusNotFound, `{}`}, model.KeywordOutcomeUnconfirmed, ""},
		{"re-read fails", kwtReply{http.StatusOK, `{}`}, kwtReply{http.StatusBadRequest, `{}`}, model.KeywordOutcomeUnconfirmed, ""},
		{"re-read shows the keyword missing", kwtReply{http.StatusOK, `{}`}, read, model.KeywordOutcomeUnconfirmed, ""},
		{"re-read shows another dimension moved", kwtReply{http.StatusOK, `{}`},
			kwtReply{http.StatusOK, `{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":{"geolocations":["GB"],"keywords":["kubernetes"],"excluded_keywords":["jobs","free"]}}}`},
			model.KeywordOutcomeUnconfirmed, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRedditKWTStub(t, tc.patch, read, tc.after)
			out, err := addRedditNegatives(s, redditBudgetCampaign(), rev, broad("jobs"), broad("free"))
			if err != nil {
				t.Fatalf("a batch with a known ALREADY_PRESENT item must answer per item, got %v", err)
			}
			if len(out) != 2 || out[0].Outcome != model.KeywordOutcomeAlreadyPresent ||
				out[1].Outcome != tc.outcome || out[1].ErrorCode != tc.code || out[1].Text != "free" {
				t.Fatalf("outcomes = %+v, want [ALREADY_PRESENT, %s/%q]", out, tc.outcome, tc.code)
			}
			if strings.Contains(out[1].ErrorCode, "secret") {
				t.Errorf("upstream text leaked: %+v", out[1])
			}
			if n := len(s.patches()); n != 1 {
				t.Fatalf("the PATCH must be sent exactly once, got %d", n)
			}
		})
	}
}
