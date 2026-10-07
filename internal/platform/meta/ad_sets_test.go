// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// adSetReply is one canned answer.
type adSetReply struct {
	status int
	body   string
}

// adSetCall is one recorded request.
type adSetCall struct {
	method, uri, body string
}

// adSetServer answers by "METHOD path", serving each key's replies in order (the last one repeats).
// Every request is recorded, and the per-key index taken, under one mutex; the test goroutine reads
// the record only through calls(), after the call under test has returned. The handler never calls
// t.Fatal.
type adSetServer struct {
	srv     *httptest.Server
	mu      sync.Mutex
	replies map[string][]adSetReply
	served  map[string]int
	seen    []adSetCall
}

func newAdSetServer(t *testing.T, replies map[string][]adSetReply) *adSetServer {
	t.Helper()
	s := &adSetServer{replies: replies, served: map[string]int{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		s.mu.Lock()
		s.seen = append(s.seen, adSetCall{method: r.Method, uri: r.URL.RequestURI(), body: string(body)})
		canned := s.replies[key]
		n := s.served[key]
		s.served[key] = n + 1
		s.mu.Unlock()
		if len(canned) == 0 {
			w.WriteHeader(http.StatusTeapot)
			_, _ = io.WriteString(w, `{}`)
			return
		}
		if n >= len(canned) {
			n = len(canned) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(canned[n].status)
		_, _ = io.WriteString(w, canned[n].body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *adSetServer) calls() []adSetCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]adSetCall(nil), s.seen...)
}

func (s *adSetServer) client(opts ...Option) *Client {
	all := append([]Option{WithBaseURL(s.srv.URL), WithClock(fixedMetaClock()),
		withSleepFn(func(context.Context, time.Duration) error { return nil })}, opts...)
	return NewClient(Credentials{AccessToken: "tok-secret-abc"}, AccountConfig{AccountID: "act_777"}, all...)
}

const (
	adSetListKey = "GET /555/adsets"
	adSetAcctKey = "GET /act_777"
	adSetInsKey  = "GET /act_777/insights"
)

func ok(body string) []adSetReply { return []adSetReply{{status: 200, body: body}} }

func adSetRow(id, campaign, account, extra string) string {
	return fmt.Sprintf(`{"id":%q,"campaign_id":%q,"account_id":%q%s}`, id, campaign, account, extra)
}

func page(next string, rows ...string) string {
	paging := `{}`
	if next != "" {
		paging = fmt.Sprintf(`{"cursors":{"after":%q},"next":"https://graph.facebook.com/v25.0/x?access_token=SHOULD-NOT-BE-FOLLOWED&after=%s"}`, next, next)
	}
	return `{"data":[` + strings.Join(rows, ",") + `],"paging":` + paging + `}`
}

func insRow(adSet, campaign, extra string) string {
	return fmt.Sprintf(`{"adset_id":%q,"campaign_id":%q,"account_currency":"USD"%s}`, adSet, campaign, extra)
}

func happyAdSetReplies() map[string][]adSetReply {
	return map[string][]adSetReply{
		adSetListKey: {
			{200, page("c1", adSetRow("888", "555", "777", `,"name":"Leads US","status":"ACTIVE","effective_status":"ACTIVE","daily_budget":"5000","bid_strategy":"LOWEST_COST_WITHOUT_CAP"`))},
			{200, page("", adSetRow("889", "555", "act_777", `,"name":"Leads EU","status":"PAUSED","effective_status":"PAUSED","lifetime_budget":"100000"`))},
		},
		adSetAcctKey: ok(`{"currency":"USD","id":"act_777"}`),
		adSetInsKey: ok(page("",
			insRow("888", "555", `,"impressions":"1000","clicks":"25","spend":"12.34"`),
			insRow("890", "555", `,"impressions":"10","clicks":"1","spend":"0.50"`),
		)),
	}
}

func TestListCampaignAdSets_HappyPathPagesAndMerges(t *testing.T) {
	s := newAdSetServer(t, happyAdSetReplies())
	got, err := s.client().ListCampaignAdSets(context.Background(), "555", "act_777", WindowLast7Days)
	if err != nil {
		t.Fatalf("ListCampaignAdSets: %v", err)
	}
	if got.Currency != "USD" || !got.CurrencyKnown || got.CurrencyOffset != 100 || got.Window != WindowLast7Days {
		t.Fatalf("header = %+v", got)
	}
	if len(got.AdSets) != 3 {
		t.Fatalf("ad sets = %+v", got.AdSets)
	}
	a, b, c := got.AdSets[0], got.AdSets[1], got.AdSets[2]
	if a.ID != "888" || !a.Listed || *a.Name != "Leads US" || *a.DailyMinor != 5000 || a.LifetimeMinor != nil ||
		a.Impressions != 1000 || a.Clicks != 25 || a.CostMicros != 12_340_000 || a.Ctr != 0.025 {
		t.Errorf("888 = %+v", a)
	}
	// Listed but no Insights row: no delivery, so zero counters — not an error.
	if b.ID != "889" || !b.Listed || *b.LifetimeMinor != 100000 || b.Impressions != 0 || b.Ctr != 0 {
		t.Errorf("889 = %+v", b)
	}
	// Delivered but not listed (deleted since): counters only.
	if c.ID != "890" || c.Listed || c.Name != nil || c.Status != nil || c.Impressions != 10 || c.CostMicros != 500_000 {
		t.Errorf("890 = %+v", c)
	}
	calls := s.calls()
	if len(calls) != 4 {
		t.Fatalf("want 2 listing pages, the currency and one insights page, got %+v", calls)
	}
	for _, c := range calls {
		if c.method != http.MethodGet {
			t.Fatalf("a read issued %s %s", c.method, c.uri)
		}
		if strings.Contains(c.uri, "SHOULD-NOT-BE-FOLLOWED") || strings.Contains(c.uri, "access_token") {
			t.Fatalf("the paging.next URL (and its token) was followed: %s", c.uri)
		}
	}
	if !strings.Contains(calls[0].uri, "fields="+adSetListFields) || !strings.Contains(calls[1].uri, "after=c1") {
		t.Errorf("listing requests = %q / %q", calls[0].uri, calls[1].uri)
	}
	ins := calls[3].uri
	for _, want := range []string{"level=adset", "date_preset=last_7d", "fields=adset_id,campaign_id,impressions,clicks,spend,account_currency",
		"filtering=%5B%7B%22field%22%3A%22campaign.id%22%2C%22operator%22%3A%22EQUAL%22%2C%22value%22%3A%22555%22%7D%5D"} {
		if !strings.Contains(ins, want) {
			t.Errorf("insights request %q lacks %q", ins, want)
		}
	}
}

func TestListCampaignAdSets_ListingPagingIsBounded(t *testing.T) {
	r := happyAdSetReplies()
	var pages []adSetReply
	for i := 0; i < adSetListMaxPages+5; i++ {
		pages = append(pages, adSetReply{200, page(fmt.Sprintf("c%d", i), adSetRow(fmt.Sprintf("%d", 1000+i), "555", "777", ""))})
	}
	r[adSetListKey] = pages
	s := newAdSetServer(t, r)
	_, err := s.client().ListCampaignAdSets(context.Background(), "555", "act_777", "")
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("err = %v, want the page bound", err)
	}
	if n := len(s.calls()); n != adSetListMaxPages {
		t.Fatalf("sent %d listing requests, want exactly %d", n, adSetListMaxPages)
	}
}

func TestListCampaignAdSets_InsightsPagingIsBounded(t *testing.T) {
	r := happyAdSetReplies()
	var pages []adSetReply
	for i := 0; i < adSetInsightsMaxPages+5; i++ {
		pages = append(pages, adSetReply{200, page(fmt.Sprintf("i%d", i), insRow(fmt.Sprintf("%d", 2000+i), "555", ""))})
	}
	r[adSetInsKey] = pages
	s := newAdSetServer(t, r)
	_, err := s.client().ListCampaignAdSets(context.Background(), "555", "act_777", "")
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("err = %v, want the page bound", err)
	}
	n := 0
	for _, c := range s.calls() {
		if strings.HasPrefix(c.uri, "/act_777/insights") {
			n++
		}
	}
	if n != adSetInsightsMaxPages {
		t.Fatalf("sent %d insights requests, want exactly %d", n, adSetInsightsMaxPages)
	}
}

func TestListCampaignAdSets_RefusesUntrustworthyAnswers(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  string
		body string
	}{
		{"repeated cursor", adSetListKey, page("c1", adSetRow("888", "555", "777", ""))},
		{"next without cursor", adSetListKey, `{"data":[],"paging":{"next":"https://x"}}`},
		{"no data field", adSetListKey, `{"paging":{}}`},
		{"duplicated key on a listing page", adSetListKey, `{"data":[{"id":"888","campaign_id":"556","campaign_id":"555","account_id":"777"}],"paging":{}}`},
		{"case-folded duplicate key", adSetListKey, `{"data":[{"id":"888","Campaign_ID":"556","campaign_id":"555","account_id":"777"}],"paging":{}}`},
		{"malformed UTF-8", adSetListKey, "{\"data\":[{\"id\":\"888\",\"name\":\"a\xffb\",\"campaign_id\":\"555\",\"account_id\":\"777\"}],\"paging\":{}}"},
		{"unpaired surrogate", adSetListKey, `{"data":[{"id":"888","name":"\ud800","campaign_id":"555","account_id":"777"}],"paging":{}}`},
		{"an ad set of another campaign", adSetListKey, page("", adSetRow("888", "556", "777", ""))},
		{"an ad set listed twice", adSetListKey, page("", adSetRow("888", "555", "777", ""), adSetRow("888", "555", "777", ""))},
		{"a non-canonical ad set id", adSetListKey, page("", adSetRow("0888", "555", "777", ""))},
		{"both budgets", adSetListKey, page("", adSetRow("888", "555", "777", `,"daily_budget":"1","lifetime_budget":"2"`))},
		{"a fractional budget", adSetListKey, page("", adSetRow("888", "555", "777", `,"daily_budget":"1.5"`))},
		{"a status outside the charset", adSetListKey, page("", adSetRow("888", "555", "777", `,"status":"<b>"`))},
		{"no account currency", adSetAcctKey, `{"id":"act_777"}`},
		{"duplicated key on an insights page", adSetInsKey, `{"data":[{"adset_id":"888","campaign_id":"555","account_currency":"USD","spend":"1","spend":"2"}],"paging":{}}`},
		{"an insights row of another campaign", adSetInsKey, page("", insRow("888", "556", ""))},
		{"a duplicated insights row", adSetInsKey, page("", insRow("888", "555", ""), insRow("888", "555", ""))},
		{"a different currency", adSetInsKey, page("", `{"adset_id":"888","campaign_id":"555","account_currency":"EUR"}`)},
		{"an explicit null impressions", adSetInsKey, page("", insRow("888", "555", `,"impressions":null`))},
		{"an explicit null spend", adSetInsKey, page("", insRow("888", "555", `,"spend":null`))},
		{"a numeric (non-string) clicks", adSetInsKey, page("", insRow("888", "555", `,"clicks":5`))},
		{"a negative impressions", adSetInsKey, page("", insRow("888", "555", `,"impressions":"-1"`))},
		{"a malformed spend", adSetInsKey, page("", insRow("888", "555", `,"spend":"lots"`))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := happyAdSetReplies()
			r[tc.key] = ok(tc.body)
			if tc.key == adSetListKey {
				// The repeated-cursor case needs the same page served twice; others end at page 1.
				r[tc.key] = []adSetReply{{200, tc.body}, {200, tc.body}}
			}
			s := newAdSetServer(t, r)
			got, err := s.client().ListCampaignAdSets(context.Background(), "555", "act_777", "")
			if err == nil || got != nil {
				t.Fatalf("got %+v, %v; want a refusal", got, err)
			}
			if errors.Is(err, ErrAdSetAccountMismatch) {
				t.Fatalf("an untrustworthy answer was classified as an account mismatch: %v", err)
			}
			for _, leak := range []string{"<b>", "lots", "\xff"} {
				if strings.Contains(err.Error(), leak) {
					t.Fatalf("error echoes upstream content %q: %v", leak, err)
				}
			}
		})
	}
}

func TestListCampaignAdSets_AnotherAccountIsAMismatch(t *testing.T) {
	r := happyAdSetReplies()
	r[adSetListKey] = ok(page("", adSetRow("888", "555", "999", "")))
	s := newAdSetServer(t, r)
	_, err := s.client().ListCampaignAdSets(context.Background(), "555", "act_777", "")
	if !errors.Is(err, ErrAdSetAccountMismatch) {
		t.Fatalf("err = %v, want ErrAdSetAccountMismatch", err)
	}
}

// Graph 100/33 on the campaign edge cannot tell a deleted campaign from one this token cannot load:
// an error, never an empty (absent) answer.
func TestListCampaignAdSets_ObjectMissingIsAnError(t *testing.T) {
	for _, status := range []int{400, 403, 404} {
		r := happyAdSetReplies()
		r[adSetListKey] = []adSetReply{{status, `{"error":{"message":"Unsupported get request","type":"GraphMethodException","code":100,"error_subcode":33}}`}}
		s := newAdSetServer(t, r)
		got, err := s.client().ListCampaignAdSets(context.Background(), "555", "act_777", "")
		var ae *APIError
		if got != nil || !errors.As(err, &ae) || ae.Code != 100 {
			t.Fatalf("status %d: got %+v, %v; want the Graph error", status, got, err)
		}
	}
}

func TestListCampaignAdSets_RefusesBadInputBeforeAnyRequest(t *testing.T) {
	s := newAdSetServer(t, happyAdSetReplies())
	c := s.client()
	for _, args := range [][3]string{{"0555", "act_777", ""}, {"555", "777", ""}, {"555", "act_777", "LAST_90_DAYS"}} {
		if _, err := c.ListCampaignAdSets(context.Background(), args[0], args[1], MetricsWindow(args[2])); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if n := len(s.calls()); n != 0 {
		t.Fatalf("sent %d requests for refused input", n)
	}
}

func TestGetAdSetState(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		s := newAdSetServer(t, map[string][]adSetReply{"GET /888": ok(`{"id":"888","campaign_id":"555","account_id":"777","status":"ACTIVE"}`)})
		st, err := s.client().GetAdSetState(context.Background(), "888")
		if err != nil || st.CampaignID != "555" || st.AccountID != "act_777" || st.Status != "ACTIVE" {
			t.Fatalf("got %+v, %v", st, err)
		}
		if calls := s.calls(); len(calls) != 1 || calls[0].uri != "/888?fields=id,campaign_id,account_id,status" {
			t.Fatalf("calls = %+v", calls)
		}
	})
	for _, tc := range []struct{ name, body string }{
		{"another ad set echoed", `{"id":"889","campaign_id":"555","account_id":"777","status":"ACTIVE"}`},
		{"no campaign", `{"id":"888","account_id":"777","status":"ACTIVE"}`},
		{"no account", `{"id":"888","campaign_id":"555","status":"ACTIVE"}`},
		{"no status", `{"id":"888","campaign_id":"555","account_id":"777"}`},
		{"duplicated campaign_id", `{"id":"888","campaign_id":"556","campaign_id":"555","account_id":"777","status":"ACTIVE"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newAdSetServer(t, map[string][]adSetReply{"GET /888": ok(tc.body)})
			if st, err := s.client().GetAdSetState(context.Background(), "888"); err == nil {
				t.Fatalf("accepted %+v", st)
			}
		})
	}
	t.Run("invalid id sends nothing", func(t *testing.T) {
		s := newAdSetServer(t, nil)
		if _, err := s.client().GetAdSetState(context.Background(), "88/8"); !errors.Is(err, ErrInvalidAdSetID) {
			t.Fatalf("err = %v", err)
		}
		if len(s.calls()) != 0 {
			t.Fatal("a request was sent")
		}
	})
}

func TestUpdateAdSetStatusOnce_Classification(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply adSetReply
		want  AdSetWriteOutcome
	}{
		{"confirmed", adSetReply{200, `{"success":true}`}, AdSetWriteApplied},
		{"2xx without success", adSetReply{200, `{}`}, AdSetWriteUnconfirmed},
		{"2xx success false", adSetReply{200, `{"success":false}`}, AdSetWriteUnconfirmed},
		{"2xx undecodable", adSetReply{200, `not json`}, AdSetWriteUnconfirmed},
		{"429", adSetReply{429, `{"error":{"message":"slow down","code":4}}`}, AdSetWriteUnconfirmed},
		{"400 with a rate-limit code", adSetReply{400, `{"error":{"message":"limit","code":17}}`}, AdSetWriteUnconfirmed},
		{"500", adSetReply{500, `{"error":{"message":"boom","code":1}}`}, AdSetWriteUnconfirmed},
		{"502 unreadable envelope", adSetReply{502, `<html>`}, AdSetWriteUnconfirmed},
		{"400 definite refusal", adSetReply{400, `{"error":{"message":"Invalid parameter","code":100}}`}, AdSetWriteRejected},
		{"403 definite refusal", adSetReply{403, `{"error":{"message":"denied","code":200}}`}, AdSetWriteRejected},
		{"400 code 1 (unknown error)", adSetReply{400, `{"error":{"message":"An unknown error occurred","code":1}}`}, AdSetWriteUnconfirmed},
		{"400 code 2 (service unavailable)", adSetReply{400, `{"error":{"message":"Service temporarily unavailable","code":2}}`}, AdSetWriteUnconfirmed},
		{"400 is_transient", adSetReply{400, `{"error":{"message":"try again","code":100,"is_transient":true}}`}, AdSetWriteUnconfirmed},
		{"400 HTML body", adSetReply{400, `<html><body>Bad Request</body></html>`}, AdSetWriteUnconfirmed},
		{"408", adSetReply{408, `{"error":{"message":"timeout","code":100}}`}, AdSetWriteUnconfirmed},
		{"400 code missing", adSetReply{400, `{"error":{"message":"rate limited"}}`}, AdSetWriteUnconfirmed},
		{"400 code 0", adSetReply{400, `{"error":{"message":"refused","code":0}}`}, AdSetWriteUnconfirmed},
		{"400 code non-numeric", adSetReply{400, `{"error":{"message":"refused","code":"100"}}`}, AdSetWriteUnconfirmed},
		{"400 duplicate code (throttle first)", adSetReply{400, `{"error":{"message":"x","code":17,"code":100}}`}, AdSetWriteUnconfirmed},
		{"400 case-folded Code", adSetReply{400, `{"error":{"message":"x","Code":17,"code":100}}`}, AdSetWriteUnconfirmed},
		{"400 duplicate is_transient", adSetReply{400, `{"error":{"message":"x","code":100,"is_transient":true,"is_transient":false}}`}, AdSetWriteUnconfirmed},
		{"2xx duplicate success", adSetReply{200, `{"success":false,"success":true}`}, AdSetWriteUnconfirmed},
		{"2xx case-folded Success", adSetReply{200, `{"success":false,"Success":true}`}, AdSetWriteUnconfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newAdSetServer(t, map[string][]adSetReply{"POST /888": {tc.reply, {200, `{"success":true}`}}})
			err := s.client().UpdateAdSetStatusOnce(context.Background(), "888", StatusPaused)
			if got := ClassifyAdSetWrite(err); got != tc.want {
				t.Fatalf("outcome = %v (err %v), want %v", got, err, tc.want)
			}
			calls := s.calls()
			// EXACTLY ONE request — a throttle is not retried, because the throttled write may
			// already have been applied.
			if len(calls) != 1 || calls[0].method != http.MethodPost || calls[0].uri != "/888" || calls[0].body != `{"status":"PAUSED"}` {
				t.Fatalf("calls = %+v, want exactly one POST /888 {\"status\":\"PAUSED\"}", calls)
			}
		})
	}
}

func TestUpdateAdSetStatusOnce_PreSendIsNotSent(t *testing.T) {
	t.Run("context already done", func(t *testing.T) {
		s := newAdSetServer(t, map[string][]adSetReply{"POST /888": ok(`{"success":true}`)})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := s.client().UpdateAdSetStatusOnce(ctx, "888", StatusActive)
		if ClassifyAdSetWrite(err) != AdSetWriteNotSent || len(s.calls()) != 0 {
			t.Fatalf("err = %v, calls = %d", err, len(s.calls()))
		}
	})
	t.Run("connection refused", func(t *testing.T) {
		s := newAdSetServer(t, nil)
		url := s.srv.URL
		s.srv.Close()
		c := NewClient(Credentials{AccessToken: "tok"}, AccountConfig{}, WithBaseURL(url))
		if got := ClassifyAdSetWrite(c.UpdateAdSetStatusOnce(context.Background(), "888", StatusActive)); got != AdSetWriteNotSent {
			t.Fatalf("outcome = %v, want NOT_SENT", got)
		}
	})
	t.Run("invalid input", func(t *testing.T) {
		s := newAdSetServer(t, nil)
		for _, args := range [][2]string{{"0", StatusActive}, {"888", "DELETED"}} {
			if got := ClassifyAdSetWrite(s.client().UpdateAdSetStatusOnce(context.Background(), args[0], args[1])); got != AdSetWriteNotSent {
				t.Errorf("%v: outcome = %v", args, got)
			}
		}
		if len(s.calls()) != 0 {
			t.Fatal("a request was sent")
		}
	})
}

// A timeout AFTER the request reached Meta is UNCONFIRMED. The handler signals receipt and then
// blocks until the test releases it — after the client has given up — so the ordering is fixed by
// the handoff, not by a sleep.
func TestUpdateAdSetStatusOnce_TimeoutAfterSendIsUnconfirmed(t *testing.T) {
	received := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(received)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		c := NewClient(Credentials{AccessToken: "tok"}, AccountConfig{}, WithBaseURL(srv.URL))
		done <- c.UpdateAdSetStatusOnce(ctx, "888", StatusPaused)
	}()
	select {
	case <-received:
	case err := <-done:
		t.Fatalf("returned before the request reached the server: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the request never reached the server")
	}
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the write did not return after its context was cancelled")
	}
	if got := ClassifyAdSetWrite(err); got != AdSetWriteUnconfirmed {
		t.Fatalf("outcome = %v (err %v), want UNCONFIRMED", got, err)
	}
}

func TestValidateAdSetID(t *testing.T) {
	for id, want := range map[string]bool{"1": true, "120210000000000888": true, strings.Repeat("9", 32): true,
		"": false, "0": false, "01": false, "1a": false, " 1": false, strings.Repeat("9", 33): false} {
		if got := ValidateAdSetID(id) == nil; got != want {
			t.Errorf("ValidateAdSetID(%q) ok = %v, want %v", id, got, want)
		}
	}
}
