// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// recordedURIs collects the request URIs an httptest handler saw. The handler runs on the
// server's own goroutine and the assertions run on the test goroutine, with only a TCP
// socket between them — which is not a happens-before edge the race detector can see. The
// mutex is what makes the handoff safe.
type recordedURIs struct {
	mu   sync.Mutex
	uris []string
}

// add records a request URI and returns its zero-based sequence number. Handing back the
// index under the SAME lock is what keeps the page counter safe too: consecutive requests
// are served on different goroutines, so a plain `n++` in the handler would be as
// unsynchronized as the slice append.
func (r *recordedURIs) add(uri string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.uris = append(r.uris, uri)
	return len(r.uris) - 1
}

func (r *recordedURIs) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.uris)
}

func (r *recordedURIs) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.uris...)
}

// newAccountsClient builds a client whose RuntimeConfig is deliberately ZERO — no
// DefaultAccountID, no Accounts. Discovery asks about the TOKEN, so it must not consult
// any configured account; a future edit that starts reading one fails here, in a test,
// rather than in production where a credentials-only connection has nothing to read.
func newAccountsClient(t *testing.T, url string) *Client {
	t.Helper()
	return NewClient(Credentials{AccessToken: "t"}, RuntimeConfig{}, WithBaseURL(url), WithClock(fixedClock()))
}

// adAccountsServer serves the given JSON bodies in order, one per request, and records
// every request URI.
func adAccountsServer(t *testing.T, pages ...string) (*httptest.Server, *recordedURIs) {
	t.Helper()
	rec := &recordedURIs{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := rec.add(r.URL.RequestURI())
		if n >= len(pages) {
			t.Errorf("unexpected request %d beyond the %d configured pages: %s", n+1, len(pages), r.URL.RequestURI())
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, pages[n])
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestListAdAccounts_ReturnsEveryAccountWithItsHealth(t *testing.T) {
	srv, _ := adAccountsServer(t, `{"elements":[
		{"id":507404993,"name":"LF Core","status":"ACTIVE","type":"BUSINESS","currency":"USD","servingStatuses":["RUNNABLE"]},
		{"id":507404994,"name":"LF Events","status":"ACTIVE","type":"BUSINESS","currency":"EUR","servingStatuses":["BILLING_HOLD"]},
		{"id":507404995,"name":"LF Draft","status":"DRAFT","type":"ENTERPRISE","currency":"USD","test":true},
		{"id":507404996}
	]}`)
	got, err := newAccountsClient(t, srv.URL).ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d accounts, want 4 — known-bad accounts must be RETURNED, not filtered", len(got))
	}

	// A numeric JSON id decodes to the bare-digits string the connection config stores.
	if got[0].ID != "507404993" {
		t.Errorf("numeric id decoded to %q, want \"507404993\"", got[0].ID)
	}
	if !got[0].Active() || !got[0].Servable() || got[0].StatusLabel() != "" || len(got[0].ServingHolds()) != 0 {
		t.Errorf("healthy account reported unhealthy: %+v", got[0])
	}
	if got[0].Currency != "USD" || got[0].Type != "BUSINESS" {
		t.Errorf("currency/type not carried: %+v", got[0])
	}

	// The two axes disagree here, which is the whole reason they are separate fields: an
	// ACTIVE account on BILLING_HOLD is bindable but will not spend. Collapsing them would
	// either hide this account or promise it can serve.
	if !got[1].Active() {
		t.Error("ACTIVE account on billing hold reported not Active(); its LIFECYCLE is fine")
	}
	if got[1].Servable() {
		t.Error("account on BILLING_HOLD reported Servable()")
	}
	if got[1].StatusLabel() != "" {
		t.Errorf("ACTIVE account produced lifecycle label %q, want empty", got[1].StatusLabel())
	}
	if holds := got[1].ServingHolds(); len(holds) != 1 || holds[0] != "on billing hold" {
		t.Errorf("serving holds = %v, want [\"on billing hold\"]", holds)
	}

	if got[2].Active() {
		t.Error("DRAFT account reported Active()")
	}
	if got[2].StatusLabel() != "not finished being set up" {
		t.Errorf("DRAFT label = %q", got[2].StatusLabel())
	}
	if !got[2].Test {
		t.Error("test:true was not carried; a test account never serves and must be surfaced, not dropped")
	}

	// The last row OMITS status, type, currency and servingStatuses entirely. An absent
	// status must NOT be reported active and must carry no label — 0 information is not a
	// claim either way. Asserting the label alone would not pin this: an ACTIVE account has
	// no label either, so a fixture sending "ACTIVE" here would pass while testing nothing
	// about absence.
	if got[3].Status != "" {
		t.Errorf("absent status decoded to %q, want empty", got[3].Status)
	}
	if got[3].Active() {
		t.Error("absent status reported Active(); absence is not a claim either way")
	}
	if got[3].StatusLabel() != "" {
		t.Errorf("absent status produced label %q, want empty", got[3].StatusLabel())
	}
	// Servable is an ALLOW-LIST: absent servingStatuses is "not confirmed servable", and
	// ServingHolds stays empty so the caller can tell "unknown" from "held".
	if got[3].Servable() {
		t.Error("absent servingStatuses reported Servable(); only [RUNNABLE] confirms it")
	}
	if holds := got[3].ServingHolds(); len(holds) != 0 {
		t.Errorf("absent servingStatuses produced holds %v, want none", holds)
	}
}

func TestListAdAccounts_AsksAboutTheTokenAndPaginatesByCursor(t *testing.T) {
	srv, uris := adAccountsServer(t,
		`{"elements":[{"id":1}],"metadata":{"nextPageToken":"tok/2 +x"}}`,
		`{"elements":[{"id":2}],"metadata":{"nextPageToken":""}}`,
	)
	got, err := newAccountsClient(t, srv.URL).ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	if len(got) != 2 || got[0].ID != "1" || got[1].ID != "2" {
		t.Fatalf("accounts across pages = %+v", got)
	}
	seen := uris.all()
	if len(seen) != 2 {
		t.Fatalf("made %d requests, want 2: %v", len(seen), seen)
	}
	for _, u := range seen {
		// The request must carry NO account id anywhere. That is what makes this callable
		// by a connection that has not chosen an account yet.
		if strings.Contains(u, "adAccounts/") {
			t.Errorf("request %q is account-scoped; discovery must ask about the token", u)
		}
		if !strings.Contains(u, "q=search") {
			t.Errorf("request %q is missing q=search", u)
		}
		if !strings.Contains(u, "pageSize="+strconv.Itoa(adAccountPageSize)) {
			t.Errorf("request %q is missing pageSize=%d", u, adAccountPageSize)
		}
	}
	// The first request must NOT send an empty pageToken: LinkedIn treats a present-but-
	// blank cursor as a malformed one on some finders.
	if strings.Contains(seen[0], "pageToken") {
		t.Errorf("first request sent a pageToken: %q", seen[0])
	}
	// The second must carry the cursor from page 1, percent-encoded — an opaque token can
	// contain characters ("/", "+", " ") that would otherwise corrupt the query string.
	if !strings.Contains(seen[1], "pageToken=tok%2F2+%2Bx") && !strings.Contains(seen[1], "pageToken=tok%2F2%20%2Bx") {
		t.Errorf("second request did not carry the encoded cursor: %q", seen[1])
	}
}

func TestListAdAccounts_EmptyIsAnAnswer(t *testing.T) {
	srv, _ := adAccountsServer(t, `{"elements":[]}`)
	got, err := newAccountsClient(t, srv.URL).ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	// Non-nil so the answer "your token reaches zero ad accounts" stays distinguishable
	// from "no answer" — including on the wire, where nil serializes as null.
	if got == nil {
		t.Fatal("empty result returned a nil slice; empty must stay distinguishable from absent")
	}
	if len(got) != 0 {
		t.Fatalf("got %d accounts, want 0", len(got))
	}
}

// Each of these is a mode in which the walk CANNOT be completed. Every one must return an
// error rather than the accounts collected so far: a short list is indistinguishable from
// a complete one at the boundary, and the caller acts on the absence — concluding their
// token cannot reach an account that is sitting right there.
func TestListAdAccounts_FailsRatherThanTruncating(t *testing.T) {
	t.Run("absent elements field", func(t *testing.T) {
		// Caught one layer down by doRequest's search-presence guard: a 2xx body with no
		// `elements` cannot prove a result set, so it must not read as "zero accounts".
		srv, _ := adAccountsServer(t, `{}`)
		if _, err := newAccountsClient(t, srv.URL).ListAdAccounts(context.Background()); err == nil {
			t.Fatal("a 2xx body with no elements field was accepted as zero accounts")
		}
	})

	t.Run("unusable id", func(t *testing.T) {
		// "urn:li:sponsoredAccount:5" is not the bare-digits form accountIDRE accepts, so
		// it could never be stored on the connection. The whole walk fails rather than the
		// row being skipped: a shape this far from the documented one means the response is
		// not what we think it is, and the rest of it is not trustworthy either.
		srv, _ := adAccountsServer(t, `{"elements":[{"id":1},{"id":"urn:li:sponsoredAccount:5"}]}`)
		_, err := newAccountsClient(t, srv.URL).ListAdAccounts(context.Background())
		if err == nil {
			t.Fatal("an account with a non-numeric id was offered to the picker")
		}
		if !strings.Contains(err.Error(), "unusable id") {
			t.Errorf("error = %v, want it to name the unusable id", err)
		}
	})

	t.Run("repeated cursor", func(t *testing.T) {
		srv, _ := adAccountsServer(t,
			`{"elements":[{"id":1}],"metadata":{"nextPageToken":"same"}}`,
			`{"elements":[{"id":2}],"metadata":{"nextPageToken":"same"}}`,
		)
		_, err := newAccountsClient(t, srv.URL).ListAdAccounts(context.Background())
		if err == nil {
			t.Fatal("a non-terminating walk returned success")
		}
		if !strings.Contains(err.Error(), "did not terminate") {
			t.Errorf("error = %v, want it to name the repeated cursor", err)
		}
	})

	t.Run("page cap", func(t *testing.T) {
		// A server that always hands back a fresh cursor. The walk must stop at the cap
		// AND report failure — stopping quietly would be the truncation this guards.
		rec := &recordedURIs{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := rec.add(r.URL.RequestURI()) + 1
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"elements":[{"id":`+strconv.Itoa(n)+`}],"metadata":{"nextPageToken":"tok`+strconv.Itoa(n)+`"}}`)
		}))
		t.Cleanup(srv.Close)

		_, err := newAccountsClient(t, srv.URL).ListAdAccounts(context.Background())
		if err == nil {
			t.Fatal("an unbounded walk returned success")
		}
		if !strings.Contains(err.Error(), "exceeded") {
			t.Errorf("error = %v, want it to name the page cap", err)
		}
		if n := rec.count(); n != adAccountMaxPages {
			t.Errorf("made %d requests, want exactly the cap of %d", n, adAccountMaxPages)
		}
	})
}

func TestAdAccountServingHolds_ReportsEveryRecognizedHold(t *testing.T) {
	a := AdAccount{ServingStatuses: []string{"STOPPED", "SOMETHING_NEW", "RESTRICTED_HOLD"}}
	if a.Servable() {
		t.Error("an account with holds reported Servable()")
	}
	holds := a.ServingHolds()
	// The unrecognized value is dropped from the LABELS (there is nothing honest to say
	// about it) but it still keeps the account out of Servable, which is an allow-list.
	if len(holds) != 2 || holds[0] != "stopped" || holds[1] != "restricted" {
		t.Errorf("holds = %v, want [stopped restricted] in report order", holds)
	}
}
