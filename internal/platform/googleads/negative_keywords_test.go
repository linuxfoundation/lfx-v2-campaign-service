// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
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
	// Each case names the SENTENCE the guard it was written for produces, not merely that SOME
	// guard tripped. Several rows are degenerate in more than one way at once — a row with no
	// `keyword` object has neither a text nor a match type, and the guards run in a fixed order
	// — so an err-is-not-nil assertion alone would stay green if a guard were deleted and its
	// rows quietly absorbed by the next one down, which is how a row ends up proving nothing
	// about the check it was added for.
	for _, tc := range []struct{ name, body, want string }{
		{"a positive criterion", `{"results":[{"campaign":{"id":"777"},"campaignCriterion":{"criterionId":"11","type":"KEYWORD","keyword":{"text":"free","matchType":"BROAD"}}}]}`,
			"is not a negative keyword criterion"},
		{"a non-keyword criterion", `{"results":[{"campaign":{"id":"777"},"campaignCriterion":{"criterionId":"11","negative":true,"type":"LOCATION","keyword":{"text":"free","matchType":"BROAD"}}}]}`,
			"is not a negative keyword criterion"},
		{"another campaign's row", `{"results":[{"campaign":{"id":"888"},"campaignCriterion":{"criterionId":"11","negative":true,"type":"KEYWORD","keyword":{"text":"free","matchType":"BROAD"}}}]}`,
			"reports a different campaign id"},
		{"no criterion id", `{"results":[{"campaign":{"id":"777"},"campaignCriterion":{"negative":true,"type":"KEYWORD","keyword":{"text":"free","matchType":"BROAD"}}}]}`,
			"is not the base-10 spelling of an integer"},
		// The criterion id is reported back as an ALREADY_PRESENT outcome's NegativeKeywordID,
		// so a non-numeric one is an identifier no later lever can address — and the mutate
		// path on this same resource already refuses that spelling.
		{"a non-numeric criterion id", `{"results":[{"campaign":{"id":"777"},"campaignCriterion":{"criterionId":"abc","negative":true,"type":"KEYWORD","keyword":{"text":"free","matchType":"BROAD"}}}]}`,
			"is not the base-10 spelling of an integer"},
		{"a criterion id with a sign", `{"results":[{"campaign":{"id":"777"},"campaignCriterion":{"criterionId":"-11","negative":true,"type":"KEYWORD","keyword":{"text":"free","matchType":"BROAD"}}}]}`,
			"is not the base-10 spelling of an integer"},
		{"a criterion id that is only whitespace", `{"results":[{"campaign":{"id":"777"},"campaignCriterion":{"criterionId":"  ","negative":true,"type":"KEYWORD","keyword":{"text":"free","matchType":"BROAD"}}}]}`,
			"is not the base-10 spelling of an integer"},
		// Invalid bytes fold unpredictably, so an existing exclusion could read as absent
		// (sent again, failing the whole atomic batch) or a new one as present (dropped).
		//
		// The sentence this row must produce is the ENVELOPE's, not the row loop's: gaqlSearch
		// runs `!utf8.Valid(raw) || hasUnpairedSurrogateEscape(raw)` over the whole response
		// body before decoding it, and those bytes include every row, so the per-row
		// utf8.ValidString check never sees a page the envelope would admit. That matches
		// TestGetCampaignSettings_UnpairedSurrogateRefused, which records the same shadowing on
		// its own sibling: the row check is kept because it names the campaign in its
		// diagnostics, and this row is NOT revert-binding on it. Asserting the row sentence here
		// would have been a false claim about which code the test exercises.
		{"keyword text that is not valid UTF-8", "{\"results\":[{\"campaign\":{\"id\":\"777\"},\"campaignCriterion\":{\"criterionId\":\"11\",\"negative\":true,\"type\":\"KEYWORD\",\"keyword\":{\"text\":\"fr\xed\xa0\x80ee\",\"matchType\":\"BROAD\"}}}]}",
			"cannot survive JSON decoding intact"},
		// utf8.ValidString("") is TRUE, so empty text needs its own check or it passes every
		// guard above and is indexed under an empty key — the keyword the row actually names
		// then reads as absent, is sent again, and fails the whole atomic batch.
		{"no keyword text at all", `{"results":[{"campaign":{"id":"777"},"campaignCriterion":{"criterionId":"11","negative":true,"type":"KEYWORD","keyword":{"matchType":"BROAD"}}}]}`,
			"carries no keyword text"},
		{"null keyword text", `{"results":[{"campaign":{"id":"777"},"campaignCriterion":{"criterionId":"11","negative":true,"type":"KEYWORD","keyword":{"text":null,"matchType":"BROAD"}}}]}`,
			"carries no keyword text"},
		{"empty keyword text", `{"results":[{"campaign":{"id":"777"},"campaignCriterion":{"criterionId":"11","negative":true,"type":"KEYWORD","keyword":{"text":"","matchType":"BROAD"}}}]}`,
			"carries no keyword text"},
		{"whitespace-only keyword text", `{"results":[{"campaign":{"id":"777"},"campaignCriterion":{"criterionId":"11","negative":true,"type":"KEYWORD","keyword":{"text":"   ","matchType":"BROAD"}}}]}`,
			"carries no keyword text"},
		// No `keyword` object at all is degenerate twice over — no text AND no match type — and
		// the text guard runs first, so the text sentence is the one it must produce. Pinning
		// that is the point: if the text guard were removed, this row would still refuse, on the
		// match type, and an err-is-not-nil assertion would never notice.
		{"no keyword object at all", `{"results":[{"campaign":{"id":"777"},"campaignCriterion":{"criterionId":"11","negative":true,"type":"KEYWORD"}}]}`,
			"carries no keyword text"},
		// The match type is half the comparison key, so an unrecognised or absent one cannot
		// match any key the caller builds: the exclusion would read as ABSENT, be sent again,
		// and fail the entire atomic batch — presenting as a platform rejection of keywords
		// that were perfectly acceptable.
		{"an unrecognised match type", `{"results":[{"campaign":{"id":"777"},"campaignCriterion":{"criterionId":"11","negative":true,"type":"KEYWORD","keyword":{"text":"free","matchType":"UNKNOWN"}}}]}`,
			"which is not one of"},
		{"no match type at all", `{"results":[{"campaign":{"id":"777"},"campaignCriterion":{"criterionId":"11","negative":true,"type":"KEYWORD","keyword":{"text":"free"}}}]}`,
			"which is not one of"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := negativeKeywordServers(t, func(string) string { return tc.body })
			got, err := c.ListCampaignNegativeKeywords(context.Background(), "777")
			if err == nil {
				t.Fatalf("expected a refusal, got %+v", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the wrong guard refused this row:\n got: %v\nwant a message containing: %q", err, tc.want)
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
		{"text past the rune limit", []Keyword{{Text: strings.Repeat("k", MaxKeywordTextRunes+1), MatchType: MatchTypeBroad}}},
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

// dialFailingOnMutate routes the pre-read to a real server and fails the campaignCriteria:mutate
// with a genuine connect-time refusal, the shape ProbeNotSent recognises. It reproduces the one
// ordering that matters: the pre-read SUCCEEDS, so the batch is built and the caller is past
// every guard, and only then does the mutate fail to leave this process.
type dialFailingOnMutate struct{ inner http.RoundTripper }

func (d dialFailingOnMutate) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.URL.Path, "campaignCriteria:mutate") {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
	return d.inner.RoundTrip(r)
}

// TestAddCampaignNegativeKeywords_PreSendDialFailureIsNotAttempted pins the classification of a
// mutate that never reached Google after a successful pre-read.
//
// Unmarked, this error carried no outcome at all, so the dispatcher's DEFINITE arm reported every
// keyword in the batch as a platform REJECTION over HTTP 200 — Google refusing keywords it was
// never asked about. It must read as NOT ATTEMPTED: the criteria cannot exist, so the operator
// must not be sent to verify a campaign nothing was sent to, and the batch is safe to retry
// whole.
func TestAddCampaignNegativeKeywords_PreSendDialFailureIsNotAttempted(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	// The pre-read answers with no existing negatives, so the whole requested batch is new and
	// the mutate is genuinely attempted.
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	t.Cleanup(apiSrv.Close)

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL), WithClock(fixedClock()),
		WithHTTPClient(&http.Client{Transport: dialFailingOnMutate{inner: http.DefaultTransport}}))

	ids, err := c.AddCampaignNegativeKeywords(context.Background(), "777", []Keyword{{Text: "free", MatchType: MatchTypeBroad}})
	if err == nil {
		t.Fatalf("expected a refusal, got ids %v", ids)
	}
	var notAttempted interface{ NotAttempted() bool }
	if !errors.As(err, &notAttempted) || !notAttempted.NotAttempted() {
		t.Fatalf("error does not report NotAttempted: %v — the batch never left this process, so it must not read as a platform rejection", err)
	}
	if IsOutcomeUnconfirmed(err) {
		t.Errorf("a batch that was never sent must not classify as unconfirmed: %v", err)
	}
}

// TestValidateNegativeKeywords_FoldsLikeTheDispatcher pins the client's dedupe key to
// textfold.Key, the key internal/dispatch uses for every negative-keyword comparison.
//
// strings.ToLower is NOT that key, and the two disagree in both directions. Under ToLower the
// dispatcher could approve a batch of two that collapsed to one operation here: Google would
// acknowledge one id, the dispatcher would still expect two, and the caller would be told
// UNCONFIRMED while one exclusion was silently never applied — identically on every retry.
//
// Cardinality is what is asserted, because cardinality is what the dispatcher pairs on.
func TestValidateNegativeKeywords_FoldsLikeTheDispatcher(t *testing.T) {
	tests := []struct {
		name string
		in   []Keyword
		want int
		why  string
	}{
		{
			name: "simple folding keeps a dotted capital I distinct from i",
			in:   []Keyword{{Text: "İ", MatchType: MatchTypeExact}, {Text: "i", MatchType: MatchTypeExact}},
			want: 2,
			why:  "strings.ToLower collapses these two; textfold.Key does not, and the dispatcher counts two",
		},
		{
			name: "simple folding joins the two Greek final sigmas",
			in:   []Keyword{{Text: "ΟΣ", MatchType: MatchTypeExact}, {Text: "ος", MatchType: MatchTypeExact}},
			want: 1,
			why:  "strings.ToLower keeps these apart; textfold.Key joins them, as the dispatcher does",
		},
		{
			name: "plain ASCII case still dedupes",
			in:   []Keyword{{Text: "Kubernetes", MatchType: MatchTypeBroad}, {Text: "kubernetes", MatchType: MatchTypeBroad}},
			want: 1,
			why:  "the original reason the key folds at all must not regress",
		},
		{
			name: "match type is half the key",
			in:   []Keyword{{Text: "free", MatchType: MatchTypeExact}, {Text: "free", MatchType: MatchTypeBroad}},
			want: 2,
			why:  "the same text under two match types is two distinct exclusions",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateNegativeKeywords(tc.in)
			if err != nil {
				t.Fatalf("validateNegativeKeywords(%v): %v", tc.in, err)
			}
			if len(got) != tc.want {
				t.Errorf("validateNegativeKeywords(%v) kept %d keywords, want %d — %s", tc.in, len(got), tc.want, tc.why)
			}
		})
	}
}

// TestValidateKeywords_FoldsLikeTheNegativePath pins the POSITIVE create path to the same key.
//
// validateKeywords and validateNegativeKeywords share validateKeywordList, so the fold is not
// forkable per caller — one helper with two folds would be a worse defect than either fold. No
// dispatcher compares positive keywords, so the dispatcher-agreement argument does not reach this
// path; what must hold here is simply that both entry points fold identically, which is what these
// rows assert.
func TestValidateKeywords_FoldsLikeTheNegativePath(t *testing.T) {
	tests := []struct {
		name string
		in   []Keyword
		want int
		why  string
	}{
		{
			name: "simple folding joins the two Greek final sigmas",
			in:   []Keyword{{Text: "ΟΣ", MatchType: MatchTypeExact}, {Text: "ος", MatchType: MatchTypeExact}},
			want: 1,
			why:  "ToLower kept these apart and sent two operations Google counts as one keyword; the fold removes that duplicate",
		},
		{
			name: "plain ASCII case still dedupes",
			in:   []Keyword{{Text: "Kubernetes", MatchType: MatchTypeBroad}, {Text: "kubernetes", MatchType: MatchTypeBroad}},
			want: 1,
			why:  "the original reason this preflight dedupes at all — a duplicate fails the whole adGroupCriteria:mutate",
		},
		{
			name: "match type is half the key",
			in:   []Keyword{{Text: "free", MatchType: MatchTypeExact}, {Text: "free", MatchType: MatchTypeBroad}},
			want: 2,
			why:  "the same text under two match types is two distinct criteria",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateKeywords(tc.in)
			if err != nil {
				t.Fatalf("validateKeywords(%v): %v", tc.in, err)
			}
			if len(got) != tc.want {
				t.Errorf("validateKeywords(%v) kept %d keywords, want %d — %s", tc.in, len(got), tc.want, tc.why)
			}
		})
	}
}

// TestAddCampaignNegativeKeywords_BoundaryRefusalsAreNotAttempted pins the method's own guards.
//
// These refuse before a request is built, so the batch provably does not exist. Returned plain,
// each would reach the dispatcher's DEFINITE arm and stamp every keyword REJECTED over HTTP 200 —
// Google refusing keywords it was never sent. validateAccountIDs is the reachable one: nothing
// upstream validates the stored customer id.
func TestAddCampaignNegativeKeywords_BoundaryRefusalsAreNotAttempted(t *testing.T) {
	valid := []Keyword{{Text: "free", MatchType: MatchTypeBroad}}
	tests := []struct {
		name       string
		account    AccountConfig
		campaignID string
		negatives  []Keyword
	}{
		{name: "malformed stored customer id", account: AccountConfig{CustomerID: "not-numeric"}, campaignID: "777", negatives: valid},
		{name: "malformed campaign id", account: testAccount(), campaignID: "77x", negatives: valid},
		{name: "keyword the create path would refuse", account: testAccount(), campaignID: "777", negatives: []Keyword{{Text: "", MatchType: MatchTypeBroad}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// No server: reaching one would itself be the failure under test.
			c := NewClient(testCreds(), tc.account, WithClock(fixedClock()))
			ids, err := c.AddCampaignNegativeKeywords(context.Background(), tc.campaignID, tc.negatives)
			if err == nil {
				t.Fatalf("expected a refusal, got ids %v", ids)
			}
			var notAttempted interface{ NotAttempted() bool }
			if !errors.As(err, &notAttempted) || !notAttempted.NotAttempted() {
				t.Fatalf("error does not report NotAttempted: %v — nothing was sent, so this must not read as a platform rejection", err)
			}
		})
	}
}

// tokenFailingAfterPreRead fails every token acquisition once the test arms it. It reproduces
// the exact ordering Copilot raised on PR #346: the dispatcher's pre-read
// (ListCampaignNegativeKeywords) succeeds on a valid cached token, that token then enters its
// refresh buffer, and the refresh ahead of campaignCriteria:mutate fails — so the mutate is
// never sent.
type tokenFailingAfterPreRead struct {
	inner    http.RoundTripper
	tokenURL string
	armed    atomic.Bool
}

func (t *tokenFailingAfterPreRead) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasPrefix(r.URL.String(), t.tokenURL) && t.armed.Load() {
		// 503: the token endpoint could not answer. Nothing about the keywords reached Google.
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader(`{"error":"temporarily_unavailable"}`)),
			Header:     make(http.Header),
			Request:    r,
		}, nil
	}
	return t.inner.RoundTrip(r)
}

// TestAddCampaignNegativeKeywords_TokenFailureAfterPreReadIsNotAttempted pins the classification
// of a token acquisition that fails AFTER the dispatcher's pre-read has already succeeded.
//
// doRequestValidated fetches the token inside the retry loop and returned that failure bare.
// ProbeNotSent does not recognise it (it is neither a dial failure nor the already-done context
// marker) and createOutcomeAmbiguous does not either (it is neither a transportError nor an
// apiError), so it reached the dispatcher's DEFINITE arm and every keyword was reported
// REJECTED over HTTP 200 — for a batch campaignCriteria:mutate never carried.
//
// The mutate is sent NON-idempotent, so doRequestValidated's loop runs exactly once and this
// failure is provably before any transmission.
func TestAddCampaignNegativeKeywords_TokenFailureAfterPreReadIsNotAttempted(t *testing.T) {
	// expires_in is inside tokenExpiryBuffer, so the token cached by the pre-read is treated as
	// needing a refresh when the mutate asks for one — the ordering the defect requires.
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":1,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	t.Cleanup(apiSrv.Close)

	rt := &tokenFailingAfterPreRead{inner: http.DefaultTransport, tokenURL: tokenSrv.URL}
	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL),
		WithHTTPClient(&http.Client{Transport: rt}))

	// The pre-read the dispatcher performs before deciding which keywords are new. It must
	// SUCCEED: the defect only appears once the caller is past every guard with a built batch.
	if _, err := c.ListCampaignNegativeKeywords(context.Background(), "777"); err != nil {
		t.Fatalf("pre-read failed, so this is not the ordering under test: %v", err)
	}
	// From here every token refresh fails.
	rt.armed.Store(true)

	ids, err := c.AddCampaignNegativeKeywords(context.Background(), "777", []Keyword{{Text: "free", MatchType: MatchTypeBroad}})
	if err == nil {
		t.Fatalf("expected a refusal, got ids %v", ids)
	}
	var notAttempted interface{ NotAttempted() bool }
	if !errors.As(err, &notAttempted) || !notAttempted.NotAttempted() {
		t.Fatalf("error does not report NotAttempted: %v — the mutate was never sent, so it must not read as a platform rejection", err)
	}
	if IsOutcomeUnconfirmed(err) {
		t.Errorf("a mutate that was never sent must not classify as unconfirmed: %v", err)
	}
}

// TestRequestNotTransmitted_OnlyClaimsTheFirstAttempt pins the guard that keeps this marker
// honest: it is attached only on attempt 0.
//
// A token failure on a LATER attempt follows a 429 whose request was already transmitted, so
// "never reached the transport" would be false — the outcome is genuinely ambiguous, which is
// the retry-history gap tracked in #347 and must not be converted into a confident claim.
func TestRequestNotTransmitted_OnlyClaimsTheFirstAttempt(t *testing.T) {
	var tokenHits atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// The SECOND token request — the one the retry makes after the 429 — fails.
		if tokenHits.Add(1) > 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"temporarily_unavailable"}`)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":1,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	// A 429 with a short Retry-After, so the idempotent call retries and re-acquires the token.
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate limited"}`)
	}))
	t.Cleanup(apiSrv.Close)

	c := NewClient(testCreds(), testAccount(),
		WithTokenURL(tokenSrv.URL), WithBaseURL(apiSrv.URL))

	// idempotent=true, so the 429 is retried and the failing token fetch happens on attempt 1.
	_, err := c.doRequest(context.Background(), http.MethodGet, c.customerPath("campaigns:search"), nil, true)
	if err == nil {
		t.Fatal("expected the retried call to fail")
	}
	if tokenHits.Load() < 2 {
		t.Fatalf("the retry never re-acquired the token (%d hits), so this is not the path under test: %v", tokenHits.Load(), err)
	}
	if RequestNotTransmitted(err) {
		t.Fatalf("a token failure AFTER a transmitted attempt must not be claimed as never-transmitted: %v", err)
	}
}

// TestRequestNotTransmitted_IsNotReadByProbeNotSent pins the separation that keeps the
// connection-test arm correct.
//
// Both predicates answer the same question — did this request leave the process — but they serve
// callers that need opposite readings of a refused credential. If ProbeNotSent absorbed this
// marker, a token refresh Google answers 401 would be reported to the metrics arm as "not sent"
// and would shadow ProbeCredentialRejected, so the operator would never be told to replace a
// credential that no longer works.
func TestRequestNotTransmitted_IsNotReadByProbeNotSent(t *testing.T) {
	err := fmt.Errorf("%w: %w", errRequestNotTransmitted, ErrCredentialRejected)

	if !RequestNotTransmitted(err) {
		t.Fatal("RequestNotTransmitted must claim its own marker")
	}
	if ProbeNotSent(err) {
		t.Error("ProbeNotSent must NOT absorb errRequestNotTransmitted — it would shadow a credential verdict on the metrics arm")
	}
	if !ProbeCredentialRejected(err) {
		t.Error("the wrap must preserve the chain so ProbeCredentialRejected still answers")
	}
}
