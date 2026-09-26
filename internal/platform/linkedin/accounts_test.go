// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	],"metadata":{}}`)
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
		// No `search` criteria, on EITHER page. Omitting it is what makes the request ask
		// about the token rather than about a filtered subset — and a cursor page is the
		// easy place to reintroduce one, since it is built separately from page 1.
		if strings.Contains(u, "search=") || strings.Contains(u, "search.") {
			t.Errorf("request %q carries search criteria; discovery must omit them entirely", u)
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

// A cursor is an opaque server token and must be echoed back byte for byte. Trimming it
// requests a different page than the one offered — and the whitespace-only case is worse
// than wrong: it trims to "", reads as exhaustion, and returns page 1 alone as the
// complete account list. That is a false absence reached through pagination, and the
// caller acts on it by concluding their token reaches no such account.
func TestListAdAccounts_CursorIsEchoedVerbatim(t *testing.T) {
	t.Run("surrounding whitespace is preserved", func(t *testing.T) {
		srv, uris := adAccountsServer(t,
			`{"elements":[{"id":1}],"metadata":{"nextPageToken":"  tok  "}}`,
			`{"elements":[{"id":2}],"metadata":{"nextPageToken":""}}`,
		)
		got, err := newAccountsClient(t, srv.URL).ListAdAccounts(context.Background())
		if err != nil {
			t.Fatalf("ListAdAccounts: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d accounts, want 2", len(got))
		}
		seen := uris.all()
		// Both encodings of a space are acceptable; a trimmed "pageToken=tok" is not.
		if !strings.Contains(seen[1], "pageToken=++tok++") && !strings.Contains(seen[1], "pageToken=%20%20tok%20%20") {
			t.Errorf("cursor was not echoed verbatim: %q", seen[1])
		}
	})

	t.Run("whitespace-only cursor is not exhaustion", func(t *testing.T) {
		srv, uris := adAccountsServer(t,
			`{"elements":[{"id":1}],"metadata":{"nextPageToken":" "}}`,
			`{"elements":[{"id":2}],"metadata":{"nextPageToken":""}}`,
		)
		got, err := newAccountsClient(t, srv.URL).ListAdAccounts(context.Background())
		if err != nil {
			t.Fatalf("ListAdAccounts: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d accounts, want 2: a whitespace cursor was read as the end of the walk", len(got))
		}
		if n := len(uris.all()); n != 2 {
			t.Fatalf("made %d requests, want 2: page 2 was never fetched", n)
		}
	})
}

func TestListAdAccounts_EmptyIsAnAnswer(t *testing.T) {
	srv, _ := adAccountsServer(t, `{"elements":[],"metadata":{}}`)
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

func TestListAdAccounts_AbsentMetadataIsNotExhaustion(t *testing.T) {
	// A response carrying elements but no cursor envelope cannot say whether more accounts
	// follow. Reading the missing block as an empty nextPageToken would report a truncated
	// walk as a complete one — the same false-absence outcome the repeated-token and
	// page-bound guards below exist to prevent, and the one that would silently hide a
	// project's real ad account from the picker.
	srv, _ := adAccountsServer(t, `{"elements":[{"id":507404993,"name":"LF Core","status":"ACTIVE"}]}`)
	_, err := newAccountsClient(t, srv.URL).ListAdAccounts(context.Background())
	if err == nil {
		t.Fatal("a response with no metadata block was accepted as a fully enumerated result")
	}
	if !strings.Contains(err.Error(), "metadata") {
		t.Errorf("error %q does not say the metadata block was missing", err)
	}
}

// The per-page size is part of the contract, not a tuning knob: adAccountMaxPages bounds
// the walk in PAGES, so a smaller page lowers the account ceiling with it. At 100 the walk
// refused a legitimate 2,001-account token.
func TestListAdAccounts_RequestsTheDocumentedPageMaximum(t *testing.T) {
	if adAccountPageSize != 1000 {
		t.Fatalf("adAccountPageSize is %d, want the documented LinkedIn maximum of 1000", adAccountPageSize)
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
		srv, _ := adAccountsServer(t, `{"elements":[{"id":1},{"id":"urn:li:sponsoredAccount:5"}],"metadata":{}}`)
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

// A test account reports RUNNABLE — it is runnable in the sense servingStatuses means —
// while a campaign bound to one never serves, never bills, and has its creatives
// auto-rejected. Servable must not present it as the healthy choice.
func TestAdAccountServable_TestAccountIsNotServable(t *testing.T) {
	test := AdAccount{Test: true, ServingStatuses: []string{"RUNNABLE"}}
	if test.Servable() {
		t.Error("a RUNNABLE TEST account reported Servable(); a campaign bound to it never serves")
	}
	// The flag must not be the only thing consulted either: the same account without the
	// flag is servable, so the RUNNABLE term is still doing its work.
	real := AdAccount{ServingStatuses: []string{"RUNNABLE"}}
	if !real.Servable() {
		t.Error("a RUNNABLE non-test account did not report Servable()")
	}
	// And a held test account stays unservable for BOTH reasons, not by accident of order.
	heldTest := AdAccount{Test: true, ServingStatuses: []string{"BILLING_HOLD"}}
	if heldTest.Servable() {
		t.Error("a held TEST account reported Servable()")
	}
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

func TestReferenceOrgID(t *testing.T) {
	cases := []struct {
		name      string
		reference string
		want      string
	}{
		{"organization reference", "urn:li:organization:2414183", "2414183"},
		{"person reference is not an organization", "urn:li:person:abc123", ""},
		{"absent reference", "", ""},
		{"whitespace-only reference", "   ", ""},
		{"malformed id", "urn:li:organization:not-a-number", ""},
		{"unrelated urn shape", "urn:li:company:2414183", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := referenceOrgID(tc.reference); got != tc.want {
				t.Errorf("referenceOrgID(%q) = %q, want %q", tc.reference, got, tc.want)
			}
		})
	}
}

func TestListAdAccounts_CarriesTheReferenceOrgID(t *testing.T) {
	srv, _ := adAccountsServer(t, `{"elements":[
		{"id":507404993,"reference":"urn:li:organization:2414183"},
		{"id":507404994,"reference":"urn:li:person:555"},
		{"id":507404995}
	],"metadata":{}}`)
	got, err := newAccountsClient(t, srv.URL).ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d accounts, want 3", len(got))
	}
	if got[0].OrgID != "2414183" {
		t.Errorf("organization-referenced account OrgID = %q, want \"2414183\"", got[0].OrgID)
	}
	if got[1].OrgID != "" {
		t.Errorf("person-referenced account OrgID = %q, want empty", got[1].OrgID)
	}
	if got[2].OrgID != "" {
		t.Errorf("no-reference account OrgID = %q, want empty", got[2].OrgID)
	}
}

func TestVerifyAccountOrgReference(t *testing.T) {
	t.Run("agreement passes", func(t *testing.T) {
		srv, _ := adAccountsServer(t, `{"elements":[
			{"id":507404993,"reference":"urn:li:organization:2414183"}
		],"metadata":{}}`)
		err := newAccountsClient(t, srv.URL).VerifyAccountOrgReference(context.Background(), "507404993", "2414183")
		if err != nil {
			t.Errorf("VerifyAccountOrgReference: %v, want nil on agreement", err)
		}
	})

	t.Run("confirmed disagreement fails closed", func(t *testing.T) {
		srv, _ := adAccountsServer(t, `{"elements":[
			{"id":507404993,"reference":"urn:li:organization:2414183"}
		],"metadata":{}}`)
		err := newAccountsClient(t, srv.URL).VerifyAccountOrgReference(context.Background(), "507404993", "999")
		if err == nil {
			t.Fatal("VerifyAccountOrgReference: want an error on a confirmed org mismatch")
		}
		if !strings.Contains(err.Error(), "2414183") || !strings.Contains(err.Error(), "999") {
			t.Errorf("error = %v, want it to name both the platform's and the configured org id", err)
		}
	})

	t.Run("account absent from a complete walk is a confirmed error", func(t *testing.T) {
		srv, _ := adAccountsServer(t, `{"elements":[
			{"id":1,"reference":"urn:li:organization:2414183"}
		],"metadata":{}}`)
		err := newAccountsClient(t, srv.URL).VerifyAccountOrgReference(context.Background(), "507404993", "2414183")
		if err == nil {
			t.Fatal("VerifyAccountOrgReference: got nil, want an error when the account is absent from a walk verified complete")
		}
		if errors.Is(err, ErrOrgVerificationInconclusive) {
			t.Errorf("VerifyAccountOrgReference: %v, want a confirmed error, not ErrOrgVerificationInconclusive", err)
		}
	})

	t.Run("account with no reference is inconclusive, not an error", func(t *testing.T) {
		srv, _ := adAccountsServer(t, `{"elements":[{"id":507404993}],"metadata":{}}`)
		err := newAccountsClient(t, srv.URL).VerifyAccountOrgReference(context.Background(), "507404993", "2414183")
		if err != nil {
			t.Errorf("VerifyAccountOrgReference: %v, want nil when the account carries no reference to compare", err)
		}
	})

	// Both halves of the stored pairing are shape-checked, and the empty string is one of the
	// shapes that fails. nil means "no mismatch found" to every caller, and a pairing that is
	// half-absent or half-malformed is not one this connection can dispatch on — reporting it
	// as nil made a connection test report a healthy connection already known to be unusable,
	// which is the whole failure class this verification closes.
	t.Run("an absent or malformed account or org id is a confirmed error, refused before enumeration", func(t *testing.T) {
		cases := []struct{ name, accountID, orgID string }{
			{"absent account id", "", "2414183"},
			{"absent org id", "507404993", ""},
			// The full URN is the realistic mistyping on either field: it CONTAINS the
			// right digits, so a check looking only for the numeric id inside the string
			// would wrongly pass it.
			{"account id as a urn", "urn:li:sponsoredAccount:507404993", "2414183"},
			{"account id with a stray character", "507404993x", "2414183"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				srv, rec := adAccountsServer(t, `{"elements":[
					{"id":507404993,"reference":"urn:li:organization:2414183"}
				],"metadata":{}}`)
				err := newAccountsClient(t, srv.URL).VerifyAccountOrgReference(context.Background(), tc.accountID, tc.orgID)
				if err == nil {
					t.Fatal("VerifyAccountOrgReference: want a CONFIRMED error — targeting.go refuses the same value, so this connection cannot dispatch")
				}
				// It must NOT be the inconclusive sentinel: TestLinkedinAds reports that as
				// a platform it could not reach, which names no field the operator can fix.
				if errors.Is(err, ErrOrgVerificationInconclusive) {
					t.Errorf("VerifyAccountOrgReference: %v, want a CONFIRMED error, not ErrOrgVerificationInconclusive (which is reported as an unreachable platform to retry)", err)
				}
				// Decidable from the stored values alone. Spending a LinkedIn round trip to
				// reach a verdict already known would also make the verdict depend on that
				// call succeeding — and an id this malformed would otherwise be reported as
				// "not found among this token's own ad accounts", sending an operator after
				// a permissions problem that does not exist.
				if rec.count() != 0 {
					t.Errorf("requests = %v, want none: a malformed stored id is decidable without contacting linkedin", rec.all())
				}
			})
		}
	})

	t.Run("a non-429 4xx fails the verification, not folded into inconclusive", func(t *testing.T) {
		for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusConflict} {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			t.Cleanup(srv.Close)
			err := newAccountsClient(t, srv.URL).VerifyAccountOrgReference(context.Background(), "507404993", "2414183")
			if err == nil {
				t.Fatalf("VerifyAccountOrgReference: want an error when discovery returns %d", status)
			}
			// LinkedIn received the request and refused it; none of these clear on their
			// own, so reporting them as an interrupted walk would invite a retry of a
			// permanently broken discovery path, forever.
			if errors.Is(err, ErrOrgVerificationInconclusive) {
				t.Errorf("VerifyAccountOrgReference on %d: %v, want a definite failure — a refusal is not an incomplete walk", status, err)
			}
			// …but it is equally not a verdict about the PAIRING. The walk sends neither
			// id, so these statuses describe the request this service built, and marking
			// them confirmed would echo "account/organization verification failed" for a
			// status that checked no account and no organization.
			if !errors.Is(err, ErrAccountDiscoveryRejected) {
				t.Errorf("VerifyAccountOrgReference on %d: %v, want ErrAccountDiscoveryRejected", status, err)
			}
			if errors.Is(err, ErrOrgVerificationFailed) {
				t.Errorf("VerifyAccountOrgReference on %d: %v, want NOT ErrOrgVerificationFailed — nothing about the pairing was checked", status, err)
			}
		}
	})

	// The credential unwrap keeps THREE sentinels out of the inconclusive bucket, and only
	// one of them can come from the ad-accounts API: a 401 there yields ErrCredentialsExpired.
	// The other two are produced by the TOKEN EXCHANGE, which no ad-accounts fixture reaches,
	// so a narrowed unwrap would leave both folded into "inconclusive" — a wrong client_id or
	// a malformed refresh request reported as a healthy connection, forever.
	t.Run("token-exchange credential failures do not fold into inconclusive", func(t *testing.T) {
		cases := []struct {
			name, oauthCode string
			want            error
		}{
			// Operator fault: the stored application credentials are wrong.
			{"invalid_client", "invalid_client", ErrApplicationCredentialsInvalid},
			// Service fault: LinkedIn refused the SHAPE of the request this service built,
			// so neither stored credential was ever evaluated.
			{"invalid_request", "invalid_request", ErrTokenRequestRejected},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"error":"`+tc.oauthCode+`"}`)
				}))
				t.Cleanup(tokenSrv.Close)
				// The ad-accounts server must never be reached: the exchange fails first.
				apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					t.Error("ad-account discovery was called after the token exchange failed")
					w.WriteHeader(http.StatusInternalServerError)
				}))
				t.Cleanup(apiSrv.Close)

				c := NewClient(refreshableCreds(), RuntimeConfig{},
					WithBaseURL(apiSrv.URL), withTokenURL(tokenSrv.URL))
				err := c.VerifyAccountOrgReference(context.Background(), "507404993", "2414183")
				if err == nil {
					t.Fatalf("VerifyAccountOrgReference: want an error when the token exchange returns %s", tc.oauthCode)
				}
				if !errors.Is(err, tc.want) {
					t.Errorf("VerifyAccountOrgReference: %v, want %v to survive the walk's error handling", err, tc.want)
				}
				// The bucket that blames the platform instead of the credential. A credential
				// fault placed in it sends the operator to wait out an outage that is not
				// happening, and neither of these clears on its own.
				if errors.Is(err, ErrOrgVerificationInconclusive) {
					t.Errorf("VerifyAccountOrgReference: %v, must NOT be inconclusive — that blames an unreachable platform for a credential fault", err)
				}
				// Nor is it a verdict about the pairing: the cross-check never ran.
				if errors.Is(err, ErrOrgVerificationFailed) {
					t.Errorf("VerifyAccountOrgReference: %v, must NOT be marked a confirmed verdict — no account was compared", err)
				}
			})
		}
	})

	// The three credential sentinels come only from a 400/401 the OAuth error code classifies.
	// Every OTHER permanently-failing exchange carried no sentinel at all and fell through to
	// the inconclusive bucket, which blames the platform — so a token endpoint answering 403 or
	// 404, or a 200 with no token in it, told the operator to retry a connection that can never
	// mint a token, forever. Retryability is the axis here, NOT whether a credential is implicated.
	t.Run("a permanently failing token exchange is not inconclusive", func(t *testing.T) {
		cases := []struct {
			name        string
			status      int
			body        string
			wantRetried bool // true: a later attempt really could succeed, so inconclusive is right
		}{
			{"403 refusal", http.StatusForbidden, "", false},
			{"404 wrong endpoint", http.StatusNotFound, "", false},
			{"410 endpoint retired", http.StatusGone, "", false},
			{"200 with no access_token", http.StatusOK, `{"expires_in":86400}`, false},
			{"200 with a malformed body", http.StatusOK, `{"access_token":`, false},
			// The retryable side of the same split, asserted here so the two can never drift.
			{"429 rate limit", http.StatusTooManyRequests, "", true},
			{"503 outage", http.StatusServiceUnavailable, "", true},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				}))
				t.Cleanup(tokenSrv.Close)
				apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					t.Error("ad-account discovery was called after the token exchange failed")
					w.WriteHeader(http.StatusInternalServerError)
				}))
				t.Cleanup(apiSrv.Close)

				c := NewClient(refreshableCreds(), RuntimeConfig{},
					WithBaseURL(apiSrv.URL), withTokenURL(tokenSrv.URL))
				err := c.VerifyAccountOrgReference(context.Background(), "507404993", "2414183")
				if err == nil {
					t.Fatalf("VerifyAccountOrgReference: want an error when the token exchange fails")
				}
				if tc.wantRetried {
					if !errors.Is(err, ErrOrgVerificationInconclusive) {
						t.Errorf("VerifyAccountOrgReference: %v, want inconclusive — a later attempt really can succeed", err)
					}
					return
				}
				if errors.Is(err, ErrOrgVerificationInconclusive) {
					t.Errorf("VerifyAccountOrgReference: %v, must NOT be inconclusive — that bucket invites a retry, and this never clears on its own", err)
				}
				// Routed onto the existing "the remedy belongs to this service" reason, which
				// dispatch maps to domain.ErrServiceDefect and logs as token_request_rejected.
				if !errors.Is(err, ErrTokenRequestRejected) {
					t.Errorf("VerifyAccountOrgReference: %v, want ErrTokenRequestRejected so it reports as a service defect rather than a verdict", err)
				}
				// It is emphatically NOT a verdict about the stored pairing: no account was
				// ever fetched, let alone compared.
				if errors.Is(err, ErrOrgVerificationFailed) {
					t.Errorf("VerifyAccountOrgReference: %v, must NOT be a confirmed verdict — the walk never ran", err)
				}
			})
		}
	})

	// SafeInconclusiveDetail classifies by TYPE, and a token-exchange failure that is not one
	// of the three credential sentinels above (an unreachable endpoint, a 503, an unreadable
	// body) is a plain error — it used to reach the fallback label and tell an operator to go
	// inspect a discovery response that was never requested, on a host that was never dialled.
	t.Run("a non-credential token-exchange failure is named as one, not as a completeness guard", func(t *testing.T) {
		tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(tokenSrv.Close)
		apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Error("ad-account discovery was called after the token exchange failed")
		}))
		t.Cleanup(apiSrv.Close)

		c := NewClient(refreshableCreds(), RuntimeConfig{},
			WithBaseURL(apiSrv.URL), withTokenURL(tokenSrv.URL))
		err := c.VerifyAccountOrgReference(context.Background(), "507404993", "2414183")
		if !errors.Is(err, ErrOrgVerificationInconclusive) {
			t.Fatalf("VerifyAccountOrgReference: %v, want ErrOrgVerificationInconclusive for a token endpoint that is merely unavailable", err)
		}
		if got := SafeInconclusiveDetail(err); !strings.Contains(got, "token exchange") {
			t.Errorf("SafeInconclusiveDetail = %q, want it to name the token exchange — the discovery request was never made", got)
		}
	})

	t.Run("a 403 is a confirmed verdict on the connection, not a rejected request", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		t.Cleanup(srv.Close)
		err := newAccountsClient(t, srv.URL).VerifyAccountOrgReference(context.Background(), "507404993", "2414183")
		// LinkedIn evaluated THIS token against THIS resource and refused it, which the
		// operator resolves by re-authorizing — so unlike a 400 or 404 it belongs on the
		// failed-test path with the connection named, not on the service-defect path.
		if !errors.Is(err, ErrOrgVerificationFailed) {
			t.Errorf("VerifyAccountOrgReference on 403: %v, want ErrOrgVerificationFailed", err)
		}
		if errors.Is(err, ErrAccountDiscoveryRejected) {
			t.Errorf("VerifyAccountOrgReference on 403: %v, want NOT ErrAccountDiscoveryRejected — an authorization refusal is about this connection", err)
		}
	})

	t.Run("a 429 stays inconclusive, unlike the rest of 4xx", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		t.Cleanup(srv.Close)
		err := newAccountsClient(t, srv.URL).VerifyAccountOrgReference(context.Background(), "507404993", "2414183")
		// Rate limiting genuinely is a walk that could not complete and that a later attempt
		// can. Failing the connection over it would call a healthy connection broken.
		if !errors.Is(err, ErrOrgVerificationInconclusive) {
			t.Errorf("VerifyAccountOrgReference: %v, want ErrOrgVerificationInconclusive for a 429", err)
		}
	})

	// A configured org id that fails orgIDRE is a CONFIRMED defect, not an inconclusive one:
	// resolveOrgID (targeting.go) refuses the same value, so campaign creation on this
	// connection cannot build a valid organization URN. Reporting it as inconclusive makes
	// TestLinkedinAds blame an unreachable platform for a value stored on the operator's own row.
	t.Run("malformed configured org id fails the test, and is refused before enumeration", func(t *testing.T) {
		// The full URN is the realistic mistyping: it CONTAINS the right digits, so a check
		// that only looked for the numeric id inside the string would wrongly pass it.
		srv, rec := adAccountsServer(t, `{"elements":[
			{"id":507404993,"reference":"urn:li:organization:2414183"}
		],"metadata":{}}`)
		err := newAccountsClient(t, srv.URL).VerifyAccountOrgReference(context.Background(), "507404993", "urn:li:organization:2414183")
		if err == nil {
			t.Fatal("VerifyAccountOrgReference: want an error for a non-numeric configured org id — resolveOrgID refuses the same value, so this connection cannot dispatch")
		}
		// It must NOT be the inconclusive sentinel: TestLinkedinAds reports that as a platform
		// it could not reach, which names no field the operator can fix.
		if errors.Is(err, ErrOrgVerificationInconclusive) {
			t.Errorf("VerifyAccountOrgReference: %v, want a CONFIRMED error, not ErrOrgVerificationInconclusive (which is reported as an unreachable platform to retry)", err)
		}
		// Decidable from the stored value alone — spending a LinkedIn round trip to reach a
		// verdict already known would also make the verdict depend on that call succeeding.
		if rec.count() != 0 {
			t.Errorf("requests = %v, want none: a malformed org id is decidable without contacting linkedin", rec.all())
		}
	})

	t.Run("upstream failure is inconclusive, not a confirmed error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		err := newAccountsClient(t, srv.URL).VerifyAccountOrgReference(context.Background(), "507404993", "2414183")
		if !errors.Is(err, ErrOrgVerificationInconclusive) {
			t.Errorf("VerifyAccountOrgReference: %v, want ErrOrgVerificationInconclusive when the discovery walk itself fails", err)
		}
	})

	t.Run("expired credentials fail the verification, not folded into inconclusive", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(srv.Close)
		err := newAccountsClient(t, srv.URL).VerifyAccountOrgReference(context.Background(), "507404993", "2414183")
		if err == nil {
			t.Fatal("VerifyAccountOrgReference: want an error when the discovery walk fails on expired credentials")
		}
		if !errors.Is(err, ErrCredentialsExpired) {
			t.Errorf("VerifyAccountOrgReference: %v, want ErrCredentialsExpired", err)
		}
		if errors.Is(err, ErrOrgVerificationInconclusive) {
			t.Errorf("VerifyAccountOrgReference: %v, a credential failure must NOT be wrapped as inconclusive — TestLinkedinAds checks the inconclusive sentinel first, and folding a credential failure into it would report a broken connection as healthy", err)
		}
	})

	t.Run("a 403 fails the verification, not folded into inconclusive", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		t.Cleanup(srv.Close)
		err := newAccountsClient(t, srv.URL).VerifyAccountOrgReference(context.Background(), "507404993", "2414183")
		if err == nil {
			t.Fatal("VerifyAccountOrgReference: want an error when the discovery walk fails on a 403")
		}
		var aerr *apiError
		if !errors.As(err, &aerr) || aerr.StatusCode != http.StatusForbidden {
			t.Errorf("VerifyAccountOrgReference: %v, want an unwrapped *apiError with StatusCode 403", err)
		}
		if errors.Is(err, ErrOrgVerificationInconclusive) {
			t.Errorf("VerifyAccountOrgReference: %v, a 403 must NOT be wrapped as inconclusive — LinkedIn evaluated this credential and refused it permission, which is a definite authorization failure, not an inconclusive walk", err)
		}
	})

	// Every confirmed verdict must be POSITIVELY marked, not merely "not inconclusive".
	// internal/dispatch converts this marker into domain.ErrOrgVerificationFailed, and
	// internal/service echoes an error's own text into the operator-visible message only for
	// that domain sentinel — an allowlist, so that a class arriving here later without a
	// marker gets fixed text rather than inheriting the echo by falling through a switch.
	// Each case below therefore asserts the mark itself; asserting the absence of the
	// inconclusive sentinel, as the cases above do, no longer covers the whole contract.
	t.Run("every confirmed verdict carries ErrOrgVerificationFailed", func(t *testing.T) {
		agreeing := `{"elements":[{"id":507404993,"reference":"urn:li:organization:2414183"}],"metadata":{}}`
		cases := []struct {
			name             string
			body             string
			status           int
			accountID, orgID string
		}{
			{"org mismatch", agreeing, 0, "507404993", "999"},
			{"account absent from a complete walk", `{"elements":[{"id":1,"reference":"urn:li:organization:2414183"}],"metadata":{}}`, 0, "507404993", "2414183"},
			{"malformed stored account id", agreeing, 0, "urn:li:sponsoredAccount:507404993", "2414183"},
			{"malformed configured org id", agreeing, 0, "507404993", "urn:li:organization:2414183"},
			// 403 is the only status among the refusals that is a verdict on the stored
			// connection: LinkedIn evaluated THIS token and refused it.
			{"a 403 refusal", "", http.StatusForbidden, "507404993", "2414183"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var url string
				if tc.status != 0 {
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.WriteHeader(tc.status)
					}))
					t.Cleanup(srv.Close)
					url = srv.URL
				} else {
					srv, _ := adAccountsServer(t, tc.body)
					url = srv.URL
				}
				err := newAccountsClient(t, url).VerifyAccountOrgReference(context.Background(), tc.accountID, tc.orgID)
				if err == nil {
					t.Fatal("VerifyAccountOrgReference: got nil, want a confirmed error")
				}
				if !errors.Is(err, ErrOrgVerificationFailed) {
					t.Errorf("VerifyAccountOrgReference: %v, want ErrOrgVerificationFailed — internal/service shows an unmarked error's text to nobody", err)
				}
				// The marker carries no text of its own: dispatch attaches the domain
				// sentinel additively and the service renders this message behind its own
				// prefix, so a sentinel sentence here would be read by an operator.
				if strings.Contains(err.Error(), ErrOrgVerificationFailed.Error()) {
					t.Errorf("VerifyAccountOrgReference: %v, want the marker attached without rendering its own sentence", err)
				}
			})
		}
	})

	// The two outcomes that must NOT carry it. A credential failure is classified by its own
	// sentinels further up the chain, and an inconclusive walk is not a verdict at all — if
	// either carried the confirmed marker the service would echo a chain it did not write.
	t.Run("credential and inconclusive outcomes are not marked confirmed", func(t *testing.T) {
		expired := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		t.Cleanup(expired.Close)
		if err := newAccountsClient(t, expired.URL).VerifyAccountOrgReference(context.Background(), "507404993", "2414183"); !errors.Is(err, ErrCredentialsExpired) || errors.Is(err, ErrOrgVerificationFailed) {
			t.Errorf("VerifyAccountOrgReference: %v, want ErrCredentialsExpired and NOT ErrOrgVerificationFailed", err)
		}
		limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		t.Cleanup(limited.Close)
		if err := newAccountsClient(t, limited.URL).VerifyAccountOrgReference(context.Background(), "507404993", "2414183"); !errors.Is(err, ErrOrgVerificationInconclusive) || errors.Is(err, ErrOrgVerificationFailed) {
			t.Errorf("VerifyAccountOrgReference: %v, want ErrOrgVerificationInconclusive and NOT ErrOrgVerificationFailed", err)
		}
	})

	t.Run("confirmed mismatch found on an early page is not undone by a later page failing", func(t *testing.T) {
		rec := &recordedURIs{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := rec.add(r.URL.RequestURI())
			if n == 0 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"elements":[
					{"id":507404993,"reference":"urn:li:organization:2414183"}
				],"metadata":{"nextPageToken":"tok"}}`)
				return
			}
			// A second page must never be requested: the target account was already found
			// on page one, and its outcome must not depend on whether LinkedIn can serve a
			// page this walk no longer needs.
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		err := newAccountsClient(t, srv.URL).VerifyAccountOrgReference(context.Background(), "507404993", "999")
		if err == nil {
			t.Fatal("VerifyAccountOrgReference: want an error on a confirmed org mismatch found on the first page")
		}
		if errors.Is(err, ErrOrgVerificationInconclusive) {
			t.Errorf("VerifyAccountOrgReference: %v, a confirmed mismatch found on an earlier page must not be discarded because a LATER page then fails — the walk must stop as soon as the target account is found", err)
		}
		if !strings.Contains(err.Error(), "2414183") || !strings.Contains(err.Error(), "999") {
			t.Errorf("error = %v, want it to name both the platform's and the configured org id", err)
		}
		if n := len(rec.all()); n != 1 {
			t.Errorf("made %d requests, want 1 — the walk must stop once it finds the target account rather than requesting a page it no longer needs", n)
		}
	})

	// The mirror of the two subtests above, and the one that pins the OTHER branch: a page
	// that does not contain the target must keep the walk going. That branch is the only
	// thing that makes "was not found among this token's own ad accounts" an exhaustive
	// claim, and nothing else in the suite discriminates it — every other case finds the
	// target on page one or serves a single page, so stopping after page one passes them all
	// while turning a real account into a confirmed "not found" for any token with enough
	// accounts to paginate.
	t.Run("a page without the target does not end the walk", func(t *testing.T) {
		srv, rec := adAccountsServer(t,
			`{"elements":[{"id":111,"reference":"urn:li:organization:2414183"}],"metadata":{"nextPageToken":"tok"}}`,
			`{"elements":[{"id":507404993,"reference":"urn:li:organization:2414183"}],"metadata":{"nextPageToken":""}}`,
		)
		if err := newAccountsClient(t, srv.URL).VerifyAccountOrgReference(context.Background(), "507404993", "2414183"); err != nil {
			t.Errorf("VerifyAccountOrgReference: %v, want nil — the target agrees on page two", err)
		}
		if n := len(rec.all()); n != 2 {
			t.Errorf("made %d requests, want 2: a page that does not hold the target must not end the walk", n)
		}
	})

	t.Run("a target on a later page is still compared, not reported absent", func(t *testing.T) {
		srv, rec := adAccountsServer(t,
			`{"elements":[{"id":111,"reference":"urn:li:organization:2414183"}],"metadata":{"nextPageToken":"tok"}}`,
			`{"elements":[{"id":507404993,"reference":"urn:li:organization:999"}],"metadata":{"nextPageToken":""}}`,
		)
		err := newAccountsClient(t, srv.URL).VerifyAccountOrgReference(context.Background(), "507404993", "2414183")
		if err == nil {
			t.Fatal("VerifyAccountOrgReference: want the mismatch on page two to be reported")
		}
		// Stopping after page one would report this account as absent from the token's own
		// accounts — an operator sent after a permissions problem that does not exist,
		// instead of the org mixup that does.
		if strings.Contains(err.Error(), "not found") {
			t.Errorf("error = %v, want the page-two MISMATCH, not an absence verdict", err)
		}
		if !strings.Contains(err.Error(), "999") || !strings.Contains(err.Error(), "2414183") {
			t.Errorf("error = %v, want it to name both org ids", err)
		}
		if n := len(rec.all()); n != 2 {
			t.Errorf("made %d requests, want 2", n)
		}
	})

	t.Run("agreement found on an early page stops the walk before a later page", func(t *testing.T) {
		rec := &recordedURIs{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n := rec.add(r.URL.RequestURI())
			if n == 0 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"elements":[
					{"id":507404993,"reference":"urn:li:organization:2414183"}
				],"metadata":{"nextPageToken":"tok"}}`)
				return
			}
			t.Error("a second page was requested after the target account was already found")
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		err := newAccountsClient(t, srv.URL).VerifyAccountOrgReference(context.Background(), "507404993", "2414183")
		if err != nil {
			t.Errorf("VerifyAccountOrgReference: %v, want nil on agreement found on the first page", err)
		}
		if n := len(rec.all()); n != 1 {
			t.Errorf("made %d requests, want 1", n)
		}
	})
}

func TestSafeInconclusiveDetail(t *testing.T) {
	t.Run("transport error never surfaces the underlying request URL", func(t *testing.T) {
		leak := &url.Error{Op: "Get", URL: "https://api.linkedin.com/rest/adAccounts?pageToken=super-secret-cursor", Err: errors.New("boom")}
		err := fmt.Errorf("%w: %w", ErrOrgVerificationInconclusive, &transportError{Method: "GET", Path: "/adAccounts", Err: leak})
		detail := SafeInconclusiveDetail(err)
		if strings.Contains(detail, "super-secret-cursor") {
			t.Errorf("SafeInconclusiveDetail leaked the request URL: %q", detail)
		}
	})

	t.Run("api error reports only the status code, never the response body", func(t *testing.T) {
		err := fmt.Errorf("%w: %w", ErrOrgVerificationInconclusive, &apiError{StatusCode: 500, Method: "GET", Path: "/adAccounts", Body: "secret body text"})
		detail := SafeInconclusiveDetail(err)
		if strings.Contains(detail, "secret body text") {
			t.Errorf("SafeInconclusiveDetail leaked the response body: %q", detail)
		}
		if !strings.Contains(detail, "500") {
			t.Errorf("SafeInconclusiveDetail = %q, want it to mention the status code", detail)
		}
	})

	// The most common real cause of an inconclusive walk, and the one that was misclassified:
	// doRequest deliberately does NOT wrap a pre-send dial failure as a *transportError (that
	// type means "may have been sent"), so it matched neither branch and fell through to the
	// completeness-guard string — telling an operator to inspect a LinkedIn response that was
	// never received.
	t.Run("a pre-send dial failure is reported as a connection failure, not a response-shape guard", func(t *testing.T) {
		dial := fmt.Errorf("linkedin GET /adAccounts: %w", &url.Error{
			Op:  "Get",
			URL: "https://api.linkedin.com/rest/adAccounts?pageToken=super-secret-cursor",
			Err: &net.DNSError{Err: "no such host", Name: "api.linkedin.com", IsNotFound: true},
		})
		err := fmt.Errorf("%w: %w", ErrOrgVerificationInconclusive, dial)
		detail := SafeInconclusiveDetail(err)
		if strings.Contains(detail, "super-secret-cursor") {
			t.Errorf("SafeInconclusiveDetail leaked the request URL: %q", detail)
		}
		if strings.Contains(detail, "completeness guard") {
			t.Errorf("SafeInconclusiveDetail = %q, want a connection-failure classification for a dial error, not a response-shape one", detail)
		}
	})

	t.Run("a completeness-guard failure still gets a fixed classification", func(t *testing.T) {
		err := fmt.Errorf("%w: %w", ErrOrgVerificationInconclusive, errors.New("linkedin ad-account discovery did not terminate (repeated page cursor)"))
		if detail := SafeInconclusiveDetail(err); detail == "" {
			t.Errorf("SafeInconclusiveDetail returned empty for a guard failure")
		}
	})
}
