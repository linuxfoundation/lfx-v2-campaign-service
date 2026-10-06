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
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// redditTargetingAdGroup is an ad group of campaign t3_c whose targeting carries the dimensions
// the create path sets, plus keywords.
func redditTargetingBody(keywords string) string {
	return `{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":{"geolocations":["US"],"locations":["FEED","COMMENTS_PAGE"],"platforms":["ALL"],"expand_targeting":true,"communities":["kubernetes"],"keywords":` + keywords + `}}}`
}

type kwtReply struct {
	status int
	body   string
}

// redditKWTStub answers GETs from a queue (the last entry repeats) and every PATCH with patch.
type redditKWTStub struct {
	d     *RedditDispatcher
	mu    sync.Mutex
	gets  []kwtReply
	patch kwtReply
	seen  []budgetRequest
}

func newRedditKWTStub(t *testing.T, patch kwtReply, gets ...kwtReply) *redditKWTStub {
	t.Helper()
	s := &redditKWTStub{gets: gets, patch: patch}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.seen = append(s.seen, budgetRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)})
		reply := s.patch
		if r.Method == http.MethodGet {
			reply = s.gets[0]
			if len(s.gets) > 1 {
				s.gets = s.gets[1:]
			}
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_, _ = io.WriteString(w, reply.body)
	}))
	t.Cleanup(api.Close)
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tok.Close)
	s.d = NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
	return s
}

func (s *redditKWTStub) patches() []budgetRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []budgetRequest
	for _, r := range s.seen {
		if r.Method == http.MethodPatch {
			out = append(out, r)
		}
	}
	return out
}

func (s *redditKWTStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// readRevision reads the targeting once through the dispatcher, for the revision a caller holds.
func readRedditRevision(t *testing.T, keywords string) string {
	t.Helper()
	s := newRedditKWTStub(t, kwtReply{http.StatusOK, `{}`}, kwtReply{http.StatusOK, redditTargetingBody(keywords)})
	kt, err := s.d.ReadKeywordTargeting(context.Background(), "proj", model.ProviderRedditAds, redditBudgetCampaign())
	if err != nil {
		t.Fatalf("ReadKeywordTargeting: %v", err)
	}
	return kt.Revision
}

func removeReddit(s *redditKWTStub, c *model.Campaign, rev string, keywords ...string) ([]model.KeywordTargetingOutcome, error) {
	rm := make([]model.KeywordTargetingRemoval, 0, len(keywords))
	for _, k := range keywords {
		rm = append(rm, model.KeywordTargetingRemoval{Keyword: k})
	}
	return s.d.RemoveKeywordTargeting(context.Background(), "proj", model.ProviderRedditAds, c, rm, rev)
}

func TestReddit_ReadKeywordTargeting_ReportsTheAdGroupsKeywordsAndRevision(t *testing.T) {
	s := newRedditKWTStub(t, kwtReply{http.StatusOK, `{}`}, kwtReply{http.StatusOK, redditTargetingBody(`["kubernetes","cloud native"]`)})
	kt, err := s.d.ReadKeywordTargeting(context.Background(), "proj", model.ProviderRedditAds, redditBudgetCampaign())
	if err != nil {
		t.Fatalf("ReadKeywordTargeting: %v", err)
	}
	if kt.EntityID != "t5_ag" || len(kt.Keywords) != 2 || kt.Keywords[0].Keyword != "kubernetes" || kt.Keywords[1].Keyword != "cloud native" {
		t.Fatalf("unexpected targeting: %+v", kt)
	}
	if kt.Keywords[0].CriterionID != "" || kt.Keywords[0].MatchType != "" {
		t.Errorf("a Reddit keyword has no id or match type: %+v", kt.Keywords[0])
	}
	if !strings.HasPrefix(kt.Revision, "sha256:") {
		t.Errorf("revision = %q", kt.Revision)
	}
	if len(s.patches()) != 0 {
		t.Fatal("a read must not write")
	}
}

func TestReddit_ReadKeywordTargeting_RefusesWhatItCannotProve(t *testing.T) {
	cases := []struct {
		name string
		get  kwtReply
	}{
		{"ad group of another campaign", kwtReply{http.StatusOK, `{"data":{"id":"t5_ag","campaign_id":"t3_other","targeting":{"keywords":["a"]}}}`}},
		{"campaign not reported", kwtReply{http.StatusOK, `{"data":{"id":"t5_ag","targeting":{"keywords":["a"]}}}`}},
		{"ad group gone", kwtReply{http.StatusNotFound, `{}`}},
		{"no targeting", kwtReply{http.StatusOK, `{"data":{"id":"t5_ag","campaign_id":"t3_c"}}`}},
		{"keywords not a string list", kwtReply{http.StatusOK, `{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":{"keywords":[1,2]}}}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRedditKWTStub(t, kwtReply{http.StatusOK, `{}`}, tc.get)
			_, err := s.d.ReadKeywordTargeting(context.Background(), "proj", model.ProviderRedditAds, redditBudgetCampaign())
			if !errors.Is(err, domain.ErrKeywordTargetingUnaddressable) {
				t.Fatalf("want ErrKeywordTargetingUnaddressable, got %v", err)
			}
		})
	}
	t.Run("no recorded ad group never reaches Reddit", func(t *testing.T) {
		s := newRedditKWTStub(t, kwtReply{http.StatusOK, `{}`}, kwtReply{http.StatusOK, redditTargetingBody(`["a"]`)})
		c := redditBudgetCampaign()
		c.Result = json.RawMessage(`{"accountId":"t2_acct"}`)
		_, err := s.d.ReadKeywordTargeting(context.Background(), "proj", model.ProviderRedditAds, c)
		if !errors.Is(err, domain.ErrKeywordTargetingUnaddressable) || s.count() != 0 {
			t.Fatalf("want a refusal with zero requests, got %v after %d", err, s.count())
		}
	})
}

func TestReddit_RemoveKeywordTargeting_OffByDefault(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "TRUE")
	s := newRedditKWTStub(t, kwtReply{http.StatusOK, `{}`}, kwtReply{http.StatusOK, redditTargetingBody(`["a","b"]`)})
	_, err := removeReddit(s, redditBudgetCampaign(), "sha256:x", "a")
	if !errors.Is(err, domain.ErrKeywordTargetingUnsupported) || s.count() != 0 {
		t.Fatalf("anything but exactly \"true\" must refuse with zero requests, got %v after %d", err, s.count())
	}
}

func TestReddit_RemoveKeywordTargeting_WritesBackEveryOtherDimensionThenConfirms(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "true")
	rev := readRedditRevision(t, `["kubernetes","cloud native","ebpf"]`)
	s := newRedditKWTStub(t, kwtReply{http.StatusOK, `{"data":{"id":"t5_ag"}}`},
		kwtReply{http.StatusOK, redditTargetingBody(`["kubernetes","cloud native","ebpf"]`)},
		kwtReply{http.StatusOK, redditTargetingBody(`["cloud native"]`)},
	)
	out, err := removeReddit(s, redditBudgetCampaign(), rev, "kubernetes", "ebpf")
	if err != nil {
		t.Fatalf("RemoveKeywordTargeting: %v", err)
	}
	if len(out) != 2 || out[0].Keyword != "kubernetes" || out[1].Keyword != "ebpf" ||
		out[0].Outcome != model.KeywordOutcomeApplied || out[1].Outcome != model.KeywordOutcomeApplied {
		t.Fatalf("outcomes = %+v", out)
	}
	p := s.patches()
	if len(p) != 1 || p[0].Path != redditBidAdGroupPath {
		t.Fatalf("want one PATCH of the ad group, got %+v", p)
	}
	var sent struct {
		Data struct {
			Targeting map[string]json.RawMessage `json:"targeting"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(p[0].Body), &sent); err != nil {
		t.Fatalf("PATCH body: %v", err)
	}
	tg := sent.Data.Targeting
	if string(tg["keywords"]) != `["cloud native"]` {
		t.Errorf("keywords sent = %s", tg["keywords"])
	}
	// Every other member goes back exactly as read — the write replaces the whole object.
	for k, want := range map[string]string{
		"geolocations": `["US"]`, "locations": `["FEED","COMMENTS_PAGE"]`, "platforms": `["ALL"]`,
		"expand_targeting": `true`, "communities": `["kubernetes"]`,
	} {
		if string(tg[k]) != want {
			t.Errorf("%s sent = %s, want %s", k, tg[k], want)
		}
	}
	if len(tg) != 6 {
		t.Errorf("sent %d targeting members, want the 6 read", len(tg))
	}
}

func TestReddit_RemoveKeywordTargeting_RefusalsWriteNothing(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "true")
	rev := readRedditRevision(t, `["a","b"]`)
	cases := []struct {
		name     string
		campaign *model.Campaign
		rev      string
		keywords []string
		want     error
		zeroReqs bool
	}{
		{"revision missing", redditBudgetCampaign(), "", []string{"a"}, domain.ErrKeywordTargetingInvalid, true},
		{"duplicate keyword", redditBudgetCampaign(), rev, []string{"a", "a"}, domain.ErrKeywordTargetingInvalid, true},
		{"provenance unknown", &model.Campaign{ID: "camp-1", Platform: model.ProviderRedditAds, PlatformCampaignID: "t3_c",
			Result: json.RawMessage(`{"adGroupId":"t5_ag"}`)}, rev, []string{"a"}, domain.ErrCampaignProvenanceUnknown, true},
		{"targeting changed since read", redditBudgetCampaign(), "sha256:stale", []string{"a"}, domain.ErrKeywordTargetingChanged, false},
		{"keyword not targeted", redditBudgetCampaign(), rev, []string{"c"}, domain.ErrKeywordTargetingInvalid, false},
		{"would remove every keyword", redditBudgetCampaign(), rev, []string{"a", "b"}, domain.ErrKeywordTargetingWouldEmpty, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRedditKWTStub(t, kwtReply{http.StatusOK, `{}`}, kwtReply{http.StatusOK, redditTargetingBody(`["a","b"]`)})
			_, err := removeReddit(s, tc.campaign, tc.rev, tc.keywords...)
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

func TestReddit_RemoveKeywordTargeting_AmbiguityIsUnconfirmed(t *testing.T) {
	t.Setenv(constants.EnvRedditKeywordTargetingWritesEnabled, "true")
	rev := readRedditRevision(t, `["a","b"]`)
	read := kwtReply{http.StatusOK, redditTargetingBody(`["a","b"]`)}
	cases := []struct {
		name  string
		patch kwtReply
		after kwtReply
	}{
		{"5xx on the PATCH", kwtReply{http.StatusBadGateway, `{}`}, read},
		{"re-read shows the keyword still there", kwtReply{http.StatusOK, `{}`}, read},
		{"re-read shows another dimension moved", kwtReply{http.StatusOK, `{}`},
			kwtReply{http.StatusOK, `{"data":{"id":"t5_ag","campaign_id":"t3_c","targeting":{"geolocations":["GB"],"keywords":["b"]}}}`}},
		{"echo with another keyword list", kwtReply{http.StatusOK, `{"data":{"id":"t5_ag","targeting":{"keywords":["a"]}}}`}, read},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newRedditKWTStub(t, tc.patch, read, tc.after)
			_, err := removeReddit(s, redditBudgetCampaign(), rev, "a")
			var u interface{ Unconfirmed() bool }
			if !errors.As(err, &u) || !u.Unconfirmed() {
				t.Fatalf("want UNCONFIRMED, got %T: %v", err, err)
			}
		})
	}
	t.Run("a definite 400 is not unconfirmed", func(t *testing.T) {
		s := newRedditKWTStub(t, kwtReply{http.StatusBadRequest, `{"error":{}}`}, read)
		_, err := removeReddit(s, redditBudgetCampaign(), rev, "a")
		var u interface{ Unconfirmed() bool }
		if err == nil || errors.As(err, &u) {
			t.Fatalf("want a definite failure, got %T: %v", err, err)
		}
	})
}
