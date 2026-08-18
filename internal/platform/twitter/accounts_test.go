// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// mustJSONString renders s as a JSON string literal, so a fixture can embed an id
// containing quotes or slashes without hand-escaping it.
func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// unmarshalEnvelope decodes a body through the SAME path doRequestAbs uses, so the
// presence-bit assertions below exercise apiResponse.UnmarshalJSON rather than a
// test-local reimplementation of it.
func unmarshalEnvelope(body string, r *apiResponse) error {
	return json.Unmarshal([]byte(body), r)
}

// discoveryClient builds a client pointed at srv with a configured account id, so every
// test that asserts the OUTBOUND REQUEST is asserting against a client that HAS an account
// to leak. A client built with a zero AccountConfig could not send the account id even if
// ListAdAccounts tried to, so it would pass against a scoped implementation.
func discoveryClient(t *testing.T, baseURL, accountID string) *Client {
	t.Helper()
	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{AccountID: accountID, FundingInstrumentID: "fund1"},
		WithBaseURL(baseURL),
		WithAPIVersion("12"),
		WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime
	return c
}

// TestListAdAccounts_RequestsTheCollectionNotTheConfiguredAccount is the test that binds
// the endpoint choice. A discovery client scoped to one account still returns a plausible
// non-empty list, so asserting only on the decoded result cannot catch it — the request
// path is the observable difference. The configured account id ("18ce54d4x5t") must appear
// NOWHERE in the request: not in the path (which would make this
// /12/accounts/18ce54d4x5t, the single-resource form every other call in this client uses)
// and not in the query (which `account_ids` would do, scoping the answer to a subset).
func TestListAdAccounts_RequestsTheCollectionNotTheConfiguredAccount(t *testing.T) {
	var (
		mu      sync.Mutex
		paths   []string
		queries []string
		methods []string
		auths   []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		queries = append(queries, r.URL.RawQuery)
		methods = append(methods, r.Method)
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"18ce54d4x5t","name":"Linux Foundation","approval_status":"ACCEPTED","timezone":"America/Los_Angeles"}],"next_cursor":null}`))
	}))
	defer srv.Close()

	c := discoveryClient(t, srv.URL, "18ce54d4x5t")
	accounts, err := c.ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}

	if len(paths) != 1 {
		t.Fatalf("expected exactly 1 request, got %d: %v", len(paths), paths)
	}
	if paths[0] != "/12/accounts" {
		t.Errorf("path = %q, want /12/accounts (the COLLECTION; a trailing id would scope the answer to one account)", paths[0])
	}
	if methods[0] != http.MethodGet {
		t.Errorf("method = %q, want GET", methods[0])
	}
	// The account id must not reach the query either — `account_ids` is the parameter that
	// would scope it there, and the path assertion above cannot see it.
	if strings.Contains(queries[0], "18ce54d4x5t") {
		t.Errorf("query %q must not carry the configured account id; discovery asks what the CREDENTIAL reaches", queries[0])
	}
	if strings.Contains(queries[0], "account_ids") {
		t.Errorf("query %q must not send account_ids; it scopes the answer to a caller-supplied subset", queries[0])
	}
	// `q` prefix-matches on name and `with_deleted` changes the set; neither is ours to guess.
	if strings.Contains(queries[0], "q=") || strings.Contains(queries[0], "with_deleted") || strings.Contains(queries[0], "sort_by") {
		t.Errorf("query %q must not narrow or reorder the picker", queries[0])
	}
	if !strings.Contains(queries[0], "count=1000") {
		t.Errorf("query %q must request count=1000, X's documented maximum page size", queries[0])
	}
	// The call is still OAuth1-signed: doRequestAbs applies the same signing as every
	// other call, and an unsigned discovery request would 401 in production while every
	// httptest-based assertion above still passed.
	if !strings.HasPrefix(auths[0], "OAuth ") {
		t.Errorf("Authorization = %q, want an OAuth 1.0a signature", auths[0])
	}

	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}
	got := accounts[0]
	if got.ID != "18ce54d4x5t" || got.Name != "Linux Foundation" || got.Status != "ACCEPTED" || got.Timezone != "America/Los_Angeles" {
		t.Errorf("decoded account = %+v, want the id/name/status/timezone from the body", got)
	}
	if got.Deleted {
		t.Errorf("Deleted = true for a row with no deleted flag")
	}
	if !got.Approved() {
		t.Errorf("Approved() = false for approval_status ACCEPTED")
	}
	if l := got.ApprovalLabel(); l != "" {
		t.Errorf("ApprovalLabel() = %q for an ACCEPTED account, want \"\"", l)
	}
}

// TestListAdAccounts_WorksWithoutAConfiguredAccount pins the property the endpoint exists
// for: the account-less connection is exactly the one discovery must serve, so a client
// built with a zero AccountConfig must still enumerate. accountIDRe guards the STORED id on
// every account-scoped path; applying it here would refuse the connection this rescues.
func TestListAdAccounts_WorksWithoutAConfiguredAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/12/accounts" {
			t.Errorf("path = %q, want /12/accounts", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"8r7gb","name":"CNCF"}],"next_cursor":null}`))
	}))
	defer srv.Close()

	c := NewClient(
		Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
		AccountConfig{}, // zero: no account chosen yet
		WithBaseURL(srv.URL), WithAPIVersion("12"), WithWriteDelay(0),
	)
	c.nonceFn = func() string { return "n" }
	c.timeFn = staticTime

	accounts, err := c.ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts with no configured account: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ID != "8r7gb" {
		t.Fatalf("accounts = %+v, want the single 8r7gb row", accounts)
	}
}

// TestListAdAccounts_ZeroAccountsIsAnAnswerNotAFailure covers the case the whole
// empty-vs-nil discipline exists for. An empty result must be a NON-NIL zero-length slice:
// nil would serialize as `null` on the wire and the service layer treats a nil result as a
// contract violation, so the distinction has to survive in Go as well as in JSON.
func TestListAdAccounts_ZeroAccountsIsAnAnswerNotAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		// An affirmative empty set: `data` present and empty, `next_cursor` present.
		_, _ = w.Write([]byte(`{"data":[],"next_cursor":null}`))
	}))
	defer srv.Close()

	accounts, err := discoveryClient(t, srv.URL, "18ce54d4x5t").ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("a credential reaching zero accounts is an ANSWER, got error: %v", err)
	}
	if accounts == nil {
		t.Fatal("accounts is nil; empty must stay distinguishable from \"no answer\", including on the wire where nil serializes as null")
	}
	if len(accounts) != 0 {
		t.Fatalf("accounts = %+v, want empty", accounts)
	}
}

// TestListAdAccounts_AbsentDataIsNotAnEmptyAnswer is the counterpart to the test above and
// the one that makes it meaningful. `{"data":[]}` PROVES the credential reaches no
// accounts; a body with no `data` field proves nothing, and reporting it as zero accounts
// would be failure-as-measurement — the caller concludes their credential reaches nothing
// and goes hunting a permissions problem that does not exist.
func TestListAdAccounts_AbsentDataIsNotAnEmptyAnswer(t *testing.T) {
	for _, body := range []string{`{"next_cursor":null}`, `{"data":null,"next_cursor":null}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
		}))
		accounts, err := discoveryClient(t, srv.URL, "18ce54d4x5t").ListAdAccounts(context.Background())
		srv.Close()
		if err == nil {
			t.Fatalf("body %s: expected an error, got accounts=%+v", body, accounts)
		}
		if accounts != nil {
			t.Errorf("body %s: accounts must be nil on failure, got %+v", body, accounts)
		}
		if !strings.Contains(err.Error(), "no data field") {
			t.Errorf("body %s: error %q should name the missing data field", body, err)
		}
	}
}

// TestListAdAccounts_AbsentCursorIsNotExhaustion is the pagination-door version of the
// guard above. X documents exhaustion as an explicit null ("If less than count entities are
// returned in the current page of the result set, the next_cursor value will be null"), so
// a body carrying no such key never said the walk was finished. Without the presence bit,
// the zero value reads as "no more pages" and a malformed FIRST page returns a partial list
// that looks complete.
func TestListAdAccounts_AbsentCursorIsNotExhaustion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		// A full, well-formed page of accounts — but no next_cursor key at all.
		_, _ = w.Write([]byte(`{"data":[{"id":"18ce54d4x5t","name":"LF"}]}`))
	}))
	defer srv.Close()

	accounts, err := discoveryClient(t, srv.URL, "18ce54d4x5t").ListAdAccounts(context.Background())
	if err == nil {
		t.Fatalf("expected an error for an absent next_cursor, got accounts=%+v", accounts)
	}
	if accounts != nil {
		t.Errorf("accounts must be nil on failure, got %+v", accounts)
	}
	if !strings.Contains(err.Error(), "next_cursor") {
		t.Errorf("error %q should name the missing next_cursor", err)
	}
}

// TestListAdAccounts_ExplicitNullCursorTerminates is the other half of the pair: a PRESENT
// null cursor is X's documented exhaustion signal and must terminate cleanly. Without this
// test, making the guard reject every falsy cursor would pass every other test here while
// failing every real single-page response.
func TestListAdAccounts_ExplicitNullCursorTerminates(t *testing.T) {
	for _, body := range []string{
		`{"data":[{"id":"abc123"}],"next_cursor":null}`,
		`{"data":[{"id":"abc123"}],"next_cursor":""}`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
		}))
		accounts, err := discoveryClient(t, srv.URL, "abc123").ListAdAccounts(context.Background())
		srv.Close()
		if err != nil {
			t.Fatalf("body %s: a present null/empty cursor is exhaustion, got error: %v", body, err)
		}
		if len(accounts) != 1 || accounts[0].ID != "abc123" {
			t.Errorf("body %s: accounts = %+v, want the single row", body, accounts)
		}
	}
}

// TestListAdAccounts_FollowsTheCursorAcrossPages asserts the walk actually pages AND that
// the cursor is echoed back verbatim in the query. Asserting only the merged result would
// pass against a client that sent no cursor at all and simply received both pages by luck
// of the fixture.
func TestListAdAccounts_FollowsTheCursorAcrossPages(t *testing.T) {
	var (
		mu      sync.Mutex
		queries []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		n := len(queries)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			// The cursor is deliberately SURROUNDED by whitespace. A trim-then-send
			// implementation is only observable against a cursor whose trimmed form
			// DIFFERS from the original — an inner space alone ("c1 c2") trims to
			// itself, so the fixture would share the code's assumption and the mutation
			// would survive.
			_, _ = w.Write([]byte(`{"data":[{"id":"acct1","name":"One"}],"next_cursor":" c1 c2 "}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"acct2","name":"Two"}],"next_cursor":null}`))
	}))
	defer srv.Close()

	accounts, err := discoveryClient(t, srv.URL, "18ce54d4x5t").ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	if len(queries) != 2 {
		t.Fatalf("expected 2 requests, got %d: %v", len(queries), queries)
	}
	if strings.Contains(queries[0], "cursor=") {
		t.Errorf("first request must send no cursor, got %q", queries[0])
	}
	// The cursor is an opaque server token: it must be sent back EXACTLY, url-escaped but
	// not trimmed or otherwise rewritten. The fixture uses a value containing a space so a
	// trim-then-send implementation is observable here rather than silently equivalent.
	// url.QueryEscape renders a space as '+', so the verbatim " c1 c2 " becomes
	// "+c1+c2+". A trimmed send would produce "c1+c2" and fail here — which is the whole
	// point of the surrounding whitespace in the fixture.
	if !strings.Contains(queries[1], "cursor=+c1+c2+") && !strings.Contains(queries[1], "cursor=%20c1%20c2%20") {
		t.Errorf("second request query = %q, want the cursor echoed VERBATIM including its surrounding whitespace (escaped, never trimmed)", queries[1])
	}
	if len(accounts) != 2 || accounts[0].ID != "acct1" || accounts[1].ID != "acct2" {
		t.Fatalf("accounts = %+v, want both pages merged in order", accounts)
	}
}

// TestListAdAccounts_RepeatedCursorIsAnError pins the non-termination guard. A server that
// keeps handing back the same cursor would otherwise spin to the page cap and return a
// duplicate-laden list.
func TestListAdAccounts_RepeatedCursorIsAnError(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"acct1"}],"next_cursor":"same"}`))
	}))
	defer srv.Close()

	accounts, err := discoveryClient(t, srv.URL, "18ce54d4x5t").ListAdAccounts(context.Background())
	if err == nil {
		t.Fatalf("expected a non-termination error, got accounts=%+v", accounts)
	}
	if accounts != nil {
		t.Errorf("accounts must be nil on failure, got %+v", accounts)
	}
	if !strings.Contains(err.Error(), "did not terminate") {
		t.Errorf("error %q should name the repeated cursor", err)
	}
	// It must stop at the repeat, not grind through the whole page cap.
	if calls != 2 {
		t.Errorf("expected the walk to stop on the 2nd page (repeat detected), got %d calls", calls)
	}
}

// TestListAdAccounts_UnusableIDFailsTheWholeWalk pins that a row whose id cannot be stored
// as a connection's account_id fails the walk rather than being skipped. Skipping would
// hand back a list short by an unknown amount that looks complete, and offering the row
// would hand the user a choice that fails at bind time — accountIDRe is the SAME regexp
// every account-scoped path validates the stored id against.
func TestListAdAccounts_UnusableIDFailsTheWholeWalk(t *testing.T) {
	for _, id := range []string{"", "acct/../other", "acct?x=1", "acct id"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			// A GOOD row precedes the bad one, so a "skip the row" implementation would
			// return a plausible one-element list rather than an obvious empty.
			_, _ = w.Write([]byte(`{"data":[{"id":"good1"},{"id":` + mustJSONString(id) + `}],"next_cursor":null}`))
		}))
		accounts, err := discoveryClient(t, srv.URL, "18ce54d4x5t").ListAdAccounts(context.Background())
		srv.Close()
		if err == nil {
			t.Fatalf("id %q: expected an error, got accounts=%+v", id, accounts)
		}
		if accounts != nil {
			t.Errorf("id %q: accounts must be nil, got %+v (a partial list looks complete)", id, accounts)
		}
		if !strings.Contains(err.Error(), "unusable id") {
			t.Errorf("id %q: error %q should name the unusable id", id, err)
		}
	}
}

// TestListAdAccounts_UnusableAccountsAreReturnedNotFiltered pins the picker discipline.
// Dropping an under-review or deleted account answers "your credential reaches no ad
// accounts" about an account sitting right there; the reason travels with the row instead.
func TestListAdAccounts_UnusableAccountsAreReturnedNotFiltered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[
			{"id":"acct1","name":"Under Review","approval_status":"UNDER_REVIEW"},
			{"id":"acct2","name":"Rejected","approval_status":"REJECTED"},
			{"id":"acct3","name":"Gone","approval_status":"ACCEPTED","deleted":true},
			{"id":"acct4","name":"Odd","approval_status":"SOMETHING_NEW"}
		],"next_cursor":null}`))
	}))
	defer srv.Close()

	accounts, err := discoveryClient(t, srv.URL, "18ce54d4x5t").ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	if len(accounts) != 4 {
		t.Fatalf("expected all 4 accounts returned, got %d: %+v", len(accounts), accounts)
	}
	if l := accounts[0].ApprovalLabel(); l != "under review" {
		t.Errorf("UNDER_REVIEW label = %q, want \"under review\"", l)
	}
	if l := accounts[1].ApprovalLabel(); l != "rejected" {
		t.Errorf("REJECTED label = %q, want \"rejected\"", l)
	}
	if !accounts[2].Deleted {
		t.Errorf("the deleted flag must survive onto the row")
	}
	// An UNRECOGNISED status must yield no label and must not be claimed as approved. X
	// publishes no complete approval_status enum, so an unknown value is "nothing to say",
	// never "this account is broken" and never "this account is fine".
	if l := accounts[3].ApprovalLabel(); l != "" {
		t.Errorf("unrecognised status label = %q, want \"\" (not a claim either way)", l)
	}
	if accounts[3].Approved() {
		t.Errorf("Approved() must be false for an unrecognised approval_status")
	}
	if accounts[3].Status != "SOMETHING_NEW" {
		t.Errorf("the raw status must travel to the caller untouched, got %q", accounts[3].Status)
	}
}

// TestListAdAccounts_NonOKStatusIsAnError pins that an upstream failure is never reported
// as an empty list — the failure-as-measurement case this whole file guards against. It
// also pins that the error text carries neither the credential nor a query string.
func TestListAdAccounts_NonOKStatusIsAnError(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED_ACCESS","message":"bad token ck/cs"}]}`))
		}))
		accounts, err := discoveryClient(t, srv.URL, "18ce54d4x5t").ListAdAccounts(context.Background())
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: expected an error, got accounts=%+v", code, accounts)
		}
		if accounts != nil {
			t.Errorf("status %d: accounts must be nil, never an empty list", code)
		}
		msg := err.Error()
		for _, secret := range []string{"ck", "cs", "at", "ats"} {
			// Whole-token check: the short fixture secrets appear as substrings of
			// ordinary words, so only a standalone occurrence is evidence of a leak.
			for _, field := range strings.FieldsFunc(msg, func(r rune) bool { return r < 'a' || r > 'z' }) {
				if field == secret {
					t.Errorf("status %d: error %q leaks credential material %q", code, msg, secret)
				}
			}
		}
		if strings.Contains(msg, "count=") || strings.Contains(msg, "?") {
			t.Errorf("status %d: error %q must not carry a query string", code, msg)
		}
	}
}

// TestListAdAccounts_PageCapIsAnErrorNotATruncatedList pins the runaway bound. Returning
// what was collected would be a short list indistinguishable from a complete one.
func TestListAdAccounts_PageCapIsAnErrorNotATruncatedList(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		// A fresh cursor every time, so the repeat guard never fires and the page cap is
		// the only thing that can stop the walk.
		_, _ = w.Write([]byte(`{"data":[{"id":"acct1"}],"next_cursor":"c` + strconv.Itoa(n) + `"}`))
	}))
	defer srv.Close()

	accounts, err := discoveryClient(t, srv.URL, "18ce54d4x5t").ListAdAccounts(context.Background())
	if err == nil {
		t.Fatalf("expected a page-cap error, got accounts=%+v", accounts)
	}
	if accounts != nil {
		t.Errorf("accounts must be nil at the cap, got %+v", accounts)
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("error %q should name the page cap", err)
	}
	if calls != adAccountMaxPages {
		t.Errorf("expected exactly %d requests before the cap fired, got %d", adAccountMaxPages, calls)
	}
}

// TestListAdAccounts_MalformedDataFieldIsAnError pins that a `data` present but not an
// array fails rather than decoding to an empty answer.
func TestListAdAccounts_MalformedDataFieldIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"id":"acct1"},"next_cursor":null}`))
	}))
	defer srv.Close()

	accounts, err := discoveryClient(t, srv.URL, "18ce54d4x5t").ListAdAccounts(context.Background())
	if err == nil {
		t.Fatalf("expected an error, got accounts=%+v", accounts)
	}
	if accounts != nil {
		t.Errorf("accounts must be nil, got %+v", accounts)
	}
	if !strings.Contains(err.Error(), "not an account list") {
		t.Errorf("error %q should name the malformed data field", err)
	}
}

// TestNextCursorPresentDoesNotChangeFindByName pins that adding the presence bit to the
// shared envelope left the other cursor walk's decoded values untouched. apiResponse gained
// a custom UnmarshalJSON, which is exactly the kind of change that can silently alter every
// other decode in the package.
func TestNextCursorPresentDoesNotChangeFindByName(t *testing.T) {
	cases := []struct {
		body        string
		wantCursor  string
		wantPresent bool
		wantData    bool
	}{
		{`{"data":[{"id":"x"}],"next_cursor":"c1"}`, "c1", true, true},
		{`{"data":[{"id":"x"}],"next_cursor":null}`, "", true, true},
		{`{"data":[{"id":"x"}]}`, "", false, true},
		{`{"next_cursor":"c1"}`, "c1", true, false},
		{`{}`, "", false, false},
	}
	for _, tc := range cases {
		var r apiResponse
		if err := unmarshalEnvelope(tc.body, &r); err != nil {
			t.Fatalf("body %s: %v", tc.body, err)
		}
		if r.NextCursor != tc.wantCursor {
			t.Errorf("body %s: NextCursor = %q, want %q", tc.body, r.NextCursor, tc.wantCursor)
		}
		if r.NextCursorPresent != tc.wantPresent {
			t.Errorf("body %s: NextCursorPresent = %v, want %v", tc.body, r.NextCursorPresent, tc.wantPresent)
		}
		if (r.Data != nil) != tc.wantData {
			t.Errorf("body %s: Data non-nil = %v, want %v", tc.body, r.Data != nil, tc.wantData)
		}
	}
}
