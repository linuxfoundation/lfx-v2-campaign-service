// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// negativeKeywordServers wires a token endpoint and an API server answering with a canned body,
// recording the last request path and body so a test can assert on the GAQL text or the mutate
// operations.
func negativeKeywordServers(t *testing.T, respond func(path string) string) (*Client, func() (string, string)) {
	t.Helper()
	var mu sync.Mutex
	var lastPath, lastBody string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastPath, lastBody = r.URL.Path, string(b)
		mu.Unlock()
		_, _ = io.WriteString(w, respond(r.URL.Path))
	}))
	t.Cleanup(apiSrv.Close)

	c := NewClient(testCreds(), testAccount(), WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))
	return c, func() (string, string) {
		mu.Lock()
		defer mu.Unlock()
		return lastPath, lastBody
	}
}

// TestListCampaignNegativeKeywords_ReadsLiveNegatives pins the happy path and the query shape.
// The absence of a status predicate is asserted, not assumed: GAQL already excludes REMOVED from
// campaign_criterion, and adding a status filter would make a removed criterion read as present
// and silently drop a keyword the campaign no longer excludes.
func TestListCampaignNegativeKeywords_ReadsLiveNegatives(t *testing.T) {
	c, last := negativeKeywordServers(t, func(string) string {
		return `{"results":[
			{"campaign":{"id":"777"},"campaignCriterion":{"criterionId":"11","negative":true,"type":"KEYWORD","keyword":{"text":"free","matchType":"BROAD"}}},
			{"campaign":{"id":"777"},"campaignCriterion":{"criterionId":"12","negative":true,"type":"KEYWORD","keyword":{"text":"cheap","matchType":"PHRASE"}}}
		]}`
	})

	got, err := c.ListCampaignNegativeKeywords(context.Background(), "777")
	if err != nil {
		t.Fatalf("ListCampaignNegativeKeywords: %v", err)
	}
	want := []CampaignNegativeKeyword{
		{CriterionID: "11", Text: "free", MatchType: "BROAD"},
		{CriterionID: "12", Text: "cheap", MatchType: "PHRASE"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d negatives, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("negative %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	_, body := last()
	for _, wantFragment := range []string{
		"FROM campaign_criterion",
		"WHERE campaign.id = 777",
		"campaign_criterion.type = 'KEYWORD'",
		"campaign_criterion.negative = TRUE",
		// campaign.id is selected so each row can be checked against the campaign that was
		// asked for rather than trusted because of the WHERE clause.
		"campaign.id,",
	} {
		if !strings.Contains(body, wantFragment) {
			t.Errorf("query missing %q: %s", wantFragment, body)
		}
	}
	if strings.Contains(body, "status") {
		t.Errorf("the query must carry NO status predicate — GAQL already excludes REMOVED: %s", body)
	}
}

// TestListCampaignNegativeKeywords_EmptyIsNotAnError pins the documented fold: a campaign with no
// negatives and a campaign id matching nothing both return zero rows, and neither is an error.
// The caller must not read this as proof the campaign exists — the dispatcher establishes that
// separately through GetCampaignSettings.
func TestListCampaignNegativeKeywords_EmptyIsNotAnError(t *testing.T) {
	c, _ := negativeKeywordServers(t, func(string) string { return `{"results":[]}` })

	got, err := c.ListCampaignNegativeKeywords(context.Background(), "777")
	if err != nil {
		t.Fatalf("zero rows must not be an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %+v, want no negatives", got)
	}
}

// TestListCampaignNegativeKeywords_RefusesUntrustworthyRows re-applies every WHERE predicate in
// code, one case per predicate. The predicate is a REQUEST; the rows are the answer. A row that
// does not satisfy it would be folded into the present-set and silently suppress a keyword the
// caller asked to add — the one failure mode this read exists to prevent, and the one that then
// fails the whole atomic batch instead.
func TestListCampaignNegativeKeywords_RefusesUntrustworthyRows(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"a positive criterion", `{"results":[{"campaign":{"id":"777"},"campaignCriterion":{"criterionId":"11","type":"KEYWORD","keyword":{"text":"free","matchType":"BROAD"}}}]}`},
		{"a non-keyword criterion", `{"results":[{"campaign":{"id":"777"},"campaignCriterion":{"criterionId":"11","negative":true,"type":"LOCATION","keyword":{"text":"free","matchType":"BROAD"}}}]}`},
		{"another campaign's row", `{"results":[{"campaign":{"id":"888"},"campaignCriterion":{"criterionId":"11","negative":true,"type":"KEYWORD","keyword":{"text":"free","matchType":"BROAD"}}}]}`},
		{"no criterion id", `{"results":[{"campaign":{"id":"777"},"campaignCriterion":{"negative":true,"type":"KEYWORD","keyword":{"text":"free","matchType":"BROAD"}}}]}`},
		// Invalid bytes fold unpredictably, so an existing exclusion could read as absent
		// (sent again, failing the whole atomic batch) or a new one as present (dropped).
		{"keyword text that is not valid UTF-8", "{\"results\":[{\"campaign\":{\"id\":\"777\"},\"campaignCriterion\":{\"criterionId\":\"11\",\"negative\":true,\"type\":\"KEYWORD\",\"keyword\":{\"text\":\"fr\xed\xa0\x80ee\",\"matchType\":\"BROAD\"}}}]}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := negativeKeywordServers(t, func(string) string { return tc.body })
			got, err := c.ListCampaignNegativeKeywords(context.Background(), "777")
			if err == nil {
				t.Fatalf("expected a refusal, got %+v", got)
			}
			if got != nil {
				t.Errorf("a refused read must return no negatives, got %+v", got)
			}
		})
	}
}

// TestListCampaignNegativeKeywords_RejectsBadCampaignID guards the interpolation: the id goes
// straight into the GAQL text, so a non-numeric value could change the SHAPE of the query rather
// than merely fail to match.
func TestListCampaignNegativeKeywords_RejectsBadCampaignID(t *testing.T) {
	var mu sync.Mutex
	var reached bool
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		reached = true
		mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer apiSrv.Close()
	c := NewClient(testCreds(), testAccount(), WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))

	for _, id := range []string{"", "  ", "777 OR 1=1", "77-7"} {
		t.Run(id, func(t *testing.T) {
			mu.Lock()
			reached = false
			mu.Unlock()
			if _, err := c.ListCampaignNegativeKeywords(context.Background(), id); err == nil {
				t.Fatal("expected a rejection")
			}
			mu.Lock()
			sawCall := reached
			mu.Unlock()
			if sawCall {
				t.Error("no API call should be made — the guard runs before interpolation")
			}
		})
	}
}

// TestAddCampaignNegativeKeywords_SendsOneAtomicBatch pins the operations: every keyword in ONE
// campaignCriteria:mutate, each carrying negative:true against the campaign resource, and the
// criterion ids returned in request order.
func TestAddCampaignNegativeKeywords_SendsOneAtomicBatch(t *testing.T) {
	c, last := negativeKeywordServers(t, func(string) string {
		return `{"results":[{"resourceName":"customers/1234567890/campaignCriteria/777~11"},{"resourceName":"customers/1234567890/campaignCriteria/777~12"}]}`
	})

	ids, err := c.AddCampaignNegativeKeywords(context.Background(), "777", []Keyword{
		{Text: "free", MatchType: MatchTypeBroad},
		{Text: "cheap", MatchType: MatchTypePhrase},
	})
	if err != nil {
		t.Fatalf("AddCampaignNegativeKeywords: %v", err)
	}
	if len(ids) != 2 || ids[0] != "11" || ids[1] != "12" {
		t.Errorf("ids = %v, want [11 12] in request order", ids)
	}

	path, body := last()
	if !strings.HasSuffix(path, "campaignCriteria:mutate") {
		t.Errorf("path = %s, want campaignCriteria:mutate", path)
	}
	for _, want := range []string{`"negative":true`, `customers/1234567890/campaigns/777`, `"text":"free"`, `"matchType":"BROAD"`, `"text":"cheap"`, `"matchType":"PHRASE"`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s: %s", want, body)
		}
	}
	// partialFailure is never set in this client: a half-applied exclusion list is worse than
	// none, because an operator sees exclusions in place and has no reason to suspect the rest
	// were dropped.
	if strings.Contains(body, "partialFailure") {
		t.Errorf("the batch must stay atomic — no partialFailure: %s", body)
	}
	if strings.Count(body, `"create"`) != 2 {
		t.Errorf("want both keywords in ONE batch of two create operations: %s", body)
	}
}

// TestAddCampaignNegativeKeywords_RevalidatesAtTheBoundary pins the package-boundary re-check.
// This is an exported method, and the text and match-type rules are the ones the CREATE path
// enforces: a keyword that could not have been created must not become reachable by editing.
func TestAddCampaignNegativeKeywords_RevalidatesAtTheBoundary(t *testing.T) {
	var mu sync.Mutex
	var reached bool
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer tokenSrv.Close()
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		reached = true
		mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer apiSrv.Close()
	c := NewClient(testCreds(), testAccount(), WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()))

	for _, tc := range []struct {
		name      string
		negatives []Keyword
	}{
		{"empty text", []Keyword{{Text: "   ", MatchType: MatchTypeBroad}}},
		{"text past the rune limit", []Keyword{{Text: strings.Repeat("k", maxKeywordTextRunes+1), MatchType: MatchTypeBroad}}},
		{"unsupported match type", []Keyword{{Text: "free", MatchType: "NEAR"}}},
		{"more than the batch maximum", manyNegatives(maxNegativeKeywords + 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			reached = false
			mu.Unlock()
			if _, err := c.AddCampaignNegativeKeywords(context.Background(), "777", tc.negatives); err == nil {
				t.Fatal("expected a rejection")
			}
			mu.Lock()
			sawCall := reached
			mu.Unlock()
			if sawCall {
				t.Error("no API call should be made — validation runs before the batch is built")
			}
		})
	}

	// An empty list is not an error and sends nothing: there is no batch to send.
	mu.Lock()
	reached = false
	mu.Unlock()
	ids, err := c.AddCampaignNegativeKeywords(context.Background(), "777", nil)
	if err != nil || ids != nil {
		t.Errorf("empty list = (%v, %v), want (nil, nil)", ids, err)
	}
	mu.Lock()
	sawCall := reached
	mu.Unlock()
	if sawCall {
		t.Error("an empty list must send no request at all")
	}
}

func manyNegatives(n int) []Keyword {
	out := make([]Keyword, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Keyword{Text: "kw" + strings.Repeat("x", i), MatchType: MatchTypeBroad})
	}
	return out
}

// TestAddCampaignNegativeKeywords_UnusableAcknowledgementIsUnconfirmed pins the rule that past a
// 2xx nothing may be reported as "not applied". Google accepted the batch, so the criteria may
// exist; a definite failure here would tell the caller the campaign's exclusions are untouched
// when they may not be.
func TestAddCampaignNegativeKeywords_UnusableAcknowledgementIsUnconfirmed(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"malformed response", `{"results":`},
		{"short response", `{"results":[{"resourceName":"customers/1234567890/campaignCriteria/777~11"}]}`},
		{"malformed criterion resource name", `{"results":[{"resourceName":"customers/1234567890/campaignCriteria/777-11"},{"resourceName":"customers/1234567890/campaignCriteria/777~12"}]}`},
		{"a different account", `{"results":[{"resourceName":"customers/9999999999/campaignCriteria/777~11"},{"resourceName":"customers/1234567890/campaignCriteria/777~12"}]}`},
		{"a different resource kind", `{"results":[{"resourceName":"customers/1234567890/adGroupCriteria/777~11"},{"resourceName":"customers/1234567890/campaignCriteria/777~12"}]}`},
		{"a different campaign id", `{"results":[{"resourceName":"customers/1234567890/campaignCriteria/888~11"},{"resourceName":"customers/1234567890/campaignCriteria/777~12"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := negativeKeywordServers(t, func(string) string { return tc.body })
			ids, err := c.AddCampaignNegativeKeywords(context.Background(), "777", []Keyword{
				{Text: "free", MatchType: MatchTypeBroad},
				{Text: "cheap", MatchType: MatchTypePhrase},
			})
			if err == nil {
				t.Fatalf("expected a refusal, got ids %v", ids)
			}
			if ids != nil {
				t.Errorf("ids must not be persisted from an untrustworthy acknowledgement: %v", ids)
			}
			if !IsOutcomeUnconfirmed(err) {
				t.Fatalf("error is not classified unconfirmed: %v — Google accepted the batch, so the criteria may exist", err)
			}
		})
	}
}

// TestAddCampaignNegativeKeywords_CancelledContextIsNotAttempted pins the one failure that
// happens BEFORE any request is built. IsOutcomeUnconfirmed gives NotAttempted precedence over
// every shape-based inference precisely for this case: a context error otherwise looks exactly
// like a request cut off in flight, and would be reported as "the criteria may exist" when
// nothing was ever sent.
func TestAddCampaignNegativeKeywords_CancelledContextIsNotAttempted(t *testing.T) {
	c, _ := negativeKeywordServers(t, func(string) string {
		return `{"results":[{"resourceName":"customers/1234567890/campaignCriteria/777~11"}]}`
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ids, err := c.AddCampaignNegativeKeywords(ctx, "777", []Keyword{{Text: "free", MatchType: MatchTypeBroad}})
	if err == nil {
		t.Fatalf("expected a refusal, got ids %v", ids)
	}
	var notAttempted interface{ NotAttempted() bool }
	if !errors.As(err, &notAttempted) || !notAttempted.NotAttempted() {
		t.Fatalf("error does not report NotAttempted: %v — nothing was sent, so this must not read as ambiguous", err)
	}
	if IsOutcomeUnconfirmed(err) {
		t.Errorf("a not-attempted batch must not classify as unconfirmed: %v", err)
	}
}
