// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

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
// mutex is what makes the handoff safe, and it is the pattern the Google Ads discovery and
// meta listAdIDs tests already use.
type recordedURIs struct {
	mu   sync.Mutex
	uris []string
}

// add records a request URI and returns its zero-based sequence number. Handing back the
// index under the SAME lock is what keeps the page counter safe too: consecutive requests
// are served on different goroutines, so a plain `n++` in the handler is as unsynchronized
// as the slice append.
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

// adAccountsServer serves /me/adaccounts from a list of canned page bodies, one per
// request, and records the full request URI of every call. Recording the URI is not
// incidental: it is what TestListAdAccounts_NeverPutsTheTokenInAURL asserts on.
func adAccountsServer(t *testing.T, pages ...string) (*httptest.Server, *recordedURIs) {
	t.Helper()
	rec := &recordedURIs{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := rec.add(r.URL.RequestURI())
		if r.URL.Path != "/me/adaccounts" {
			t.Errorf("unexpected path %q; discovery must ask about the TOKEN, not an account", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if n >= len(pages) {
			t.Errorf("server asked for page %d but only %d were canned", n+1, len(pages))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, pages[n])
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func newAccountsClient(srv *httptest.Server) *Client {
	// AccountConfig is deliberately ZERO: discovery must work for a connection that
	// holds credentials but has not yet been bound to an account. A client that needed
	// AccountID here could never serve the picker that chooses it.
	return NewClient(Credentials{AccessToken: "tok-secret-abc"}, AccountConfig{}, WithBaseURL(srv.URL))
}

func TestListAdAccounts_ReturnsEveryAccountWithItsStatus(t *testing.T) {
	srv, _ := adAccountsServer(t, `{"data":[
		{"id":"act_111","name":"LF Core","account_status":1},
		{"id":"act_222","name":"LF Events","account_status":3},
		{"id":"act_333"}
	]}`)

	got, err := newAccountsClient(srv).ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d accounts, want 3: %+v", len(got), got)
	}
	// The unsettled account is RETURNED, not filtered — the picker must show the user
	// the account they are looking for along with why it cannot be used.
	if got[1].ID != "act_222" || got[1].Active() {
		t.Errorf("unsettled account missing or reported active: %+v", got[1])
	}
	if lbl := got[1].StatusLabel(); lbl != "unsettled" {
		t.Errorf("StatusLabel() = %q, want %q", lbl, "unsettled")
	}
	// act_333's row OMITS account_status entirely. It must decode to 0 — and 0 is NOT a
	// claim of disabled, so it carries no label AND is not reported active. Asserting the
	// label alone would not pin this: an active account has no label either, so a fixture
	// that sent account_status:1 here would pass while testing nothing about absence.
	if got[2].Status != 0 {
		t.Errorf("absent account_status decoded to %d, want 0", got[2].Status)
	}
	if got[2].Active() {
		t.Error("absent account_status reported Active(); 0 is not a claim either way")
	}
	if lbl := got[2].StatusLabel(); lbl != "" {
		t.Errorf("absent account_status produced label %q, want empty", lbl)
	}
	if !got[0].Active() || got[0].StatusLabel() != "" {
		t.Errorf("active account misreported: %+v", got[0])
	}
	if got[2].Name != "" {
		t.Errorf("Name = %q, want empty for an account with no name", got[2].Name)
	}
}

func TestListAdAccounts_WalksEveryPage(t *testing.T) {
	srv, uris := adAccountsServer(t,
		`{"data":[{"id":"act_1","name":"one","account_status":1}],
		  "paging":{"cursors":{"after":"CUR sor/1"},"next":"https://graph.facebook.com/v21.0/me/adaccounts?after=CUR+sor%2F1&access_token=tok-secret-abc"}}`,
		`{"data":[{"id":"act_2","name":"two","account_status":1}]}`,
	)

	got, err := newAccountsClient(srv).ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	if len(got) != 2 || got[0].ID != "act_1" || got[1].ID != "act_2" {
		t.Fatalf("pages not concatenated in order: %+v", got)
	}
	if uris.count() != 2 {
		t.Fatalf("made %d requests, want 2", uris.count())
	}
	// The cursor is carried as an ESCAPED query parameter on a path we build ourselves.
	// A raw "CUR sor/1" here would mean the cursor was concatenated unescaped.
	if second := uris.all()[1]; !strings.Contains(second, "after=CUR+sor%2F1") && !strings.Contains(second, "after=CUR%20sor%2F1") {
		t.Errorf("second request did not carry the escaped cursor: %s", second)
	}
}

func TestListAdAccounts_NeverPutsTheTokenInAURL(t *testing.T) {
	// paging.next is Meta's own absolute URL and it carries access_token as a query
	// parameter. Following it would copy the credential into the request URI, and from
	// there into apiError/transportError text that the discovery handler logs.
	srv, uris := adAccountsServer(t,
		`{"data":[{"id":"act_1","name":"one","account_status":1}],
		  "paging":{"cursors":{"after":"c1"},"next":"https://graph.facebook.com/v21.0/me/adaccounts?after=c1&access_token=tok-secret-abc&appsecret_proof=deadbeef"}}`,
		`{"data":[]}`,
	)

	if _, err := newAccountsClient(srv).ListAdAccounts(context.Background()); err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	for i, u := range uris.all() {
		if strings.Contains(u, "tok-secret-abc") || strings.Contains(u, "access_token") || strings.Contains(u, "appsecret_proof") {
			t.Fatalf("request %d leaked the credential into its URL: %s", i, u)
		}
	}
}

func TestListAdAccounts_EmptyPageIsAnEmptyListNotNil(t *testing.T) {
	srv, _ := adAccountsServer(t, `{"data":[]}`)

	got, err := newAccountsClient(srv).ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	// A token that genuinely reaches zero accounts is an ANSWER, and it has to stay
	// distinguishable from "no answer" all the way up to the HTTP body, where nil
	// would serialize as null instead of [].
	if got == nil {
		t.Fatal("got nil slice for a present-but-empty data array; want empty non-nil")
	}
	if len(got) != 0 {
		t.Fatalf("got %d accounts, want 0", len(got))
	}
}

func TestListAdAccounts_FailsRatherThanTruncating(t *testing.T) {
	tests := []struct {
		name    string
		pages   []string
		wantErr string
	}{{
		name: "2xx with no data field cannot prove emptiness",
		// `{}` is not `{"data":[]}`. Decoding both to a nil slice would let a malformed
		// success read as "fully enumerated, zero accounts" — the exact false absence
		// that sends a user hunting a permissions problem that does not exist.
		pages:   []string{`{}`},
		wantErr: "no data field",
	}, {
		name:    "null data field cannot prove emptiness",
		pages:   []string{`{"data":null}`},
		wantErr: "no data field",
	}, {
		name: "more pages but no cursor",
		pages: []string{
			`{"data":[{"id":"act_1","account_status":1}],"paging":{"cursors":{"after":"  "},"next":"https://graph.facebook.com/next"}}`,
		},
		wantErr: "no cursor",
	}, {
		name: "repeated cursor would loop forever",
		pages: []string{
			`{"data":[{"id":"act_1","account_status":1}],"paging":{"cursors":{"after":"c1"},"next":"https://graph.facebook.com/next"}}`,
			`{"data":[{"id":"act_2","account_status":1}],"paging":{"cursors":{"after":"c1"},"next":"https://graph.facebook.com/next"}}`,
		},
		wantErr: "did not terminate",
	}, {
		name: "an id that is not act_DIGITS is not storable as a connection account",
		pages: []string{
			`{"data":[{"id":"act_1","account_status":1},{"id":"1234567","name":"bare id","account_status":1}]}`,
		},
		wantErr: "unusable id",
	}, {
		name: "an id from another node type is rejected too",
		pages: []string{
			`{"data":[{"id":"page_1234567","account_status":1}]}`,
		},
		wantErr: "unusable id",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := adAccountsServer(t, tc.pages...)

			got, err := newAccountsClient(srv).ListAdAccounts(context.Background())
			if err == nil {
				t.Fatalf("got %+v and no error; an incomplete walk must never return a partial list", got)
			}
			if got != nil {
				t.Errorf("returned %d accounts alongside the error; want nil", len(got))
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestListAdAccounts_PageCapIsAnErrorNotATruncation(t *testing.T) {
	// Every page advertises another one with a fresh cursor, so only the cap stops the
	// walk. Returning what was collected would be a silently short account list.
	rec := &recordedURIs{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sequence number comes back from the guarded recorder rather than a bare `n++`:
		// each request is served on its own goroutine, and the assertion below reads the
		// count from the test goroutine.
		n := rec.add(r.URL.RequestURI()) + 1
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"act_`+strconv.Itoa(n)+`","account_status":1}],`+
			`"paging":{"cursors":{"after":"cur-`+strings.Repeat("x", n)+`"},"next":"https://graph.facebook.com/next"}}`)
	}))
	defer srv.Close()

	got, err := newAccountsClient(srv).ListAdAccounts(context.Background())
	if err == nil {
		t.Fatalf("got %d accounts and no error; the page cap must be an error", len(got))
	}
	if got != nil {
		t.Errorf("returned a partial list alongside the cap error")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("error = %q, want it to mention the page cap", err)
	}
	if n := rec.count(); n != adAccountMaxPages {
		t.Errorf("made %d requests, want exactly the cap %d", n, adAccountMaxPages)
	}
}

func TestListAdAccounts_PropagatesTransportErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"insufficient permission","type":"OAuthException","code":200}}`)
	}))
	defer srv.Close()

	got, err := newAccountsClient(srv).ListAdAccounts(context.Background())
	if err == nil {
		t.Fatal("want an error for a 403 from Graph")
	}
	if got != nil {
		t.Errorf("returned accounts alongside a transport error")
	}
}

func TestListAdAccounts_RequestsTheFieldsItDecodes(t *testing.T) {
	srv, uris := adAccountsServer(t, `{"data":[]}`)

	if _, err := newAccountsClient(srv).ListAdAccounts(context.Background()); err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	// Graph returns only the fields asked for. Dropping one of these from the query
	// would silently zero the corresponding struct field for every account.
	for _, want := range []string{"id", "name", "account_status"} {
		if first := uris.all()[0]; !strings.Contains(first, want) {
			t.Errorf("request did not ask for %q: %s", want, first)
		}
	}
}
