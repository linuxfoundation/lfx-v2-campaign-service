// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// These dispatchers must SATISFY the AccountLister interface, or Orchestrator.ReadAccounts
// type-asserts, misses, and answers ErrAccountsUnsupported — the endpoint would exist and
// always fail. A compile-time assertion catches that at build rather than at runtime.
//
// This block is NOT the discovery roster. It pins the three providers whose behavioural tests
// live in this file, and a provider can satisfy AccountLister without appearing here — Google
// Ads and Meta both do today. Reading it as the full set is what the comment it replaces got
// wrong, and an enumeration in prose is exactly the thing that goes stale as providers land.
//
// The authoritative roster is derived, not written down: accountListerProviders in
// accountlister_prose_parity_test.go type-asserts every candidate dispatcher against
// service.AccountLister, so it moves with the code and cannot silently omit an implementation.
// Consult that when the question is "which providers support discovery?"; these assertions
// only guarantee that the three exercised below still compile against the interface.
var (
	_ service.AccountLister = (*LinkedInDispatcher)(nil)
	_ service.AccountLister = (*MicrosoftDispatcher)(nil)
	_ service.AccountLister = (*TwitterDispatcher)(nil)
)

// requestRecorder captures what the dispatcher actually put on the wire. Asserting on the
// REQUEST is the point: a discovery client accidentally scoped to one account still returns
// a plausible non-empty list, so only the outbound request distinguishes "which accounts
// does this credential reach?" from a narrower question that looks identical in the result.
type requestRecorder struct {
	mu      sync.Mutex
	paths   []string
	queries []string
	headers []http.Header
}

func (r *requestRecorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, req.URL.Path)
	r.queries = append(r.queries, req.URL.RawQuery)
	r.headers = append(r.headers, req.Header.Clone())
}

func (r *requestRecorder) all() (paths, queries []string, headers []http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...), append([]string(nil), r.queries...), append([]http.Header(nil), r.headers...)
}

// linkedInAccountsServer answers the adAccounts finder with a fixed element set.
func linkedInAccountsServer(t *testing.T, rec *requestRecorder, elements string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		// metadata is REQUIRED: the client refuses a response without it rather than
		// reading the zero value as an exhausted cursor, because that would return a
		// truncated account list as a complete one. An empty nextPageToken means "fully
		// enumerated".
		_, _ = io.WriteString(w, `{"elements":[`+elements+`],"metadata":{"nextPageToken":""}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The account-less connection is the state discovery exists to rescue, and it must reach the
// platform rather than being refused before the call.
//
// Both dispatchers' credential resolvers hard-fail on a missing account id for DISPATCH —
// correctly, since a client built without one cannot reach the platform. Discovery exists to
// FIND that id, so demanding one makes the endpoint reachable only by connections that no
// longer need it. Asserted through a real (fake) server so the test proves the listing
// COMPLETED, not merely that some other error came back: an earlier version asserted only
// that the error was not ErrAccountNotSelected, which an unrelated preflight failure also
// satisfies, and which reached the live internet to do it.
func TestLinkedInListAccountsWorksWithoutASelectedAccount(t *testing.T) {
	rec := &requestRecorder{}
	srv := linkedInAccountsServer(t, rec,
		`{"id":507404993,"name":"LF Events","status":"ACTIVE","currency":"USD","test":false,"servingStatuses":["RUNNABLE"]}`)

	conn := activeLinkedInConn(goodLinkedInCreds)
	conn.AccountID = "" // the state under test
	d := NewLinkedInDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))

	accounts, err := d.ListAccounts(context.Background(), "cncf", model.ProviderLinkedInAds)
	if err != nil {
		t.Fatalf("discovery must work on a connection with no account id — that is the connection it exists to serve: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ID != "507404993" {
		t.Fatalf("accounts = %+v, want the one the platform reported", accounts)
	}
}

// The request must ask about the TOKEN, not about the stored account.
//
// LinkedIn's ListAdAccounts builds its request from q/pageSize/pageToken only and never
// reads RuntimeConfig.AccountID, so it cannot be account-scoped the way Microsoft's
// per-customer path can. What this test pins is that the stored id never LEAKS into the
// request — a future edit that threaded it through (as the Microsoft path legitimately
// does for its customer id) would narrow the question silently, and the result would still
// be a plausible one-account list. Only the outbound request shows the difference.
func TestLinkedInListAccountsAsksAboutTheTokenNotTheAccount(t *testing.T) {
	rec := &requestRecorder{}
	srv := linkedInAccountsServer(t, rec, `{"id":507404993,"name":"LF Events","status":"ACTIVE"}`)

	// The fixture DOES carry an account id, so there is something that could leak.
	conn := activeLinkedInConn(goodLinkedInCreds)
	conn.AccountID = "123456789"
	d := NewLinkedInDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))

	if _, err := d.ListAccounts(context.Background(), "cncf", model.ProviderLinkedInAds); err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	paths, queries, _ := rec.all()
	if len(paths) == 0 {
		t.Fatal("no request was made")
	}
	for i := range paths {
		if strings.Contains(paths[i], "123456789") || strings.Contains(queries[i], "123456789") {
			t.Errorf("the stored account id leaked into the discovery request %q?%q — the client is scoped to one account and answers a narrower question than was asked",
				paths[i], queries[i])
		}
	}
}

// microsoftAccountsServer answers the SOAP-ish Accounts/Query envelopes the client issues.
// Both shapes are needed: the client discovers customers first, then enumerates per customer.
func microsoftAccountsServer(t *testing.T, rec *requestRecorder) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/token"):
			_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
		case strings.Contains(r.URL.Path, "User/Query"), strings.Contains(r.URL.Path, "User"):
			// CustomerRoles is TOP-LEVEL, matching userQueryResponse
			// (internal/platform/microsoft/accounts.go) and the platform package's own
			// withCustomerRoles helper. An earlier version of this fixture nested it under
			// "User", which never decoded — so customer discovery always failed and this
			// test only ever proved that a request was attempted.
			_, _ = io.WriteString(w, `{"CustomerRoles":[{"CustomerId":9999999,"RoleId":41}]}`)
		default:
			_, _ = io.WriteString(w, `{"AccountsInfo":[{"Id":1234567,"Name":"LF Events","Number":"X1234567","AccountLifeCycleStatus":"Active","PauseReason":0}]}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMicrosoftListAccountsWorksWithoutASelectedAccount(t *testing.T) {
	rec := &requestRecorder{}
	srv := microsoftAccountsServer(t, rec)

	conn := activeMicrosoftConn(goodMicrosoftCreds)
	conn.AccountID = ""
	d := NewMicrosoftDispatcher(fakeConnReader{conn: conn}, identityEncryptor{},
		microsoft.WithBaseURL(srv.URL), microsoft.WithCustomerBaseURL(srv.URL), microsoft.WithTokenURL(srv.URL+"/token"))

	accounts, err := d.ListAccounts(context.Background(), "cncf", model.ProviderMicrosoftAds)
	if errors.Is(err, domain.ErrAccountNotSelected) {
		t.Fatal("discovery refused a connection with no account id — that is the connection it exists to serve")
	}
	// Require the ACCOUNT, not merely an attempt. An earlier version of this test accepted
	// any "transport-shaped failure against the fake", which — combined with a fixture the
	// parser could not decode — meant it passed while customer discovery failed on every
	// run. A test that tolerates the failure it is meant to detect proves nothing: this is
	// the whole point of the capability, so it must reach AccountsInfo and return the row.
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("want exactly the one discovered account, got %d: %+v", len(accounts), accounts)
	}
	if accounts[0].ID != "1234567" {
		t.Errorf("account id = %q, want 1234567", accounts[0].ID)
	}
	// The client composes the label from the account NAME plus its account NUMBER, so a
	// marketer picking from the list can tell two similarly-named accounts apart.
	if accounts[0].Label != "LF Events (X1234567)" {
		t.Errorf("account label = %q, want %q", accounts[0].Label, "LF Events (X1234567)")
	}
	paths, _, _ := rec.all()
	if len(paths) == 0 {
		t.Fatal("no upstream request was made — the resolver refused before contacting the platform")
	}
}

// The configured customer_id must NOT scope discovery. discoveryCustomerIDs treats a
// configured customer as the COMPLETE answer and returns early without enumerating any
// other, so passing it would have made an ordinary configured connection list only that one
// customer's accounts — while this endpoint's own description promises every customer the
// credential reaches. The endpoint would have contradicted itself for exactly the
// connections most likely to use it.
func TestMicrosoftListAccountsDoesNotScopeToTheStoredCustomer(t *testing.T) {
	rec := &requestRecorder{}
	srv := microsoftAccountsServer(t, rec)

	// The fixture carries customer_id 9999999, so a scoping regression is observable.
	conn := activeMicrosoftConn(goodMicrosoftCreds)
	d := NewMicrosoftDispatcher(fakeConnReader{conn: conn}, identityEncryptor{},
		microsoft.WithBaseURL(srv.URL), microsoft.WithCustomerBaseURL(srv.URL), microsoft.WithTokenURL(srv.URL+"/token"))

	_, _ = d.ListAccounts(context.Background(), "cncf", model.ProviderMicrosoftAds)

	paths, _, _ := rec.all()
	if len(paths) == 0 {
		t.Fatal("no upstream request was made")
	}
	// User/Query is the tell, and it is the ONLY reliable one. A configured CustomerID makes
	// discoveryCustomerIDs return early with that single customer and NEVER call User/Query
	// -- so the presence of that call is what distinguishes "asked which customers this
	// credential reaches" from "assumed the stored one is the whole answer". Asserting on a
	// request HEADER does not work: the scoped path skips the call entirely rather than
	// sending a differently-headed one, so a header check passes against the bug.
	var askedWhichCustomers bool
	for _, p := range paths {
		if strings.Contains(p, "User") {
			askedWhichCustomers = true
		}
	}
	if !askedWhichCustomers {
		t.Errorf("discovery never called User/Query; a configured customer_id was treated as the complete answer, so only that customer's accounts are listed while the endpoint promises every customer the credential reaches. Requests: %v", paths)
	}
}

// Discovery must NOT become a way around the checks dispatch enforces. Sharing the resolver
// is what keeps the two from drifting; without it a credential rejected at dispatch could be
// accepted here, which makes a discovery endpoint actively misleading rather than merely
// permissive. Run for BOTH dispatchers: their tolerated-vs-propagated logic is written
// separately (LinkedIn's switch, Microsoft's negated errors.Is), so a table driving only one
// leaves the other's inversion undetectable.
func TestListAccountsStillRejectsAnUnusableConnection(t *testing.T) {
	cases := []struct {
		name string
		conn func() *model.Connection
	}{
		{"inactive", func() *model.Connection {
			c := activeLinkedInConn(goodLinkedInCreds)
			c.Status = model.StatusInactive
			return c
		}},
		{"undecodable credentials", func() *model.Connection { return activeLinkedInConn(`{not json`) }},
		{"incomplete credentials", func() *model.Connection { return activeLinkedInConn(`{"AccessToken":""}`) }},
	}
	for _, tc := range cases {
		t.Run("linkedin/"+tc.name, func(t *testing.T) {
			d := NewLinkedInDispatcher(fakeConnReader{conn: tc.conn()}, identityEncryptor{})
			_, err := d.ListAccounts(context.Background(), "cncf", model.ProviderLinkedInAds)
			if err == nil {
				t.Fatal("discovery accepted a connection dispatch would reject")
			}
			if !errors.Is(err, domain.ErrConnectionNotUsable) {
				t.Errorf("error %v does not carry ErrConnectionNotUsable, so the endpoint cannot map it to the right status", err)
			}
		})
	}

	msCases := []struct {
		name string
		conn func() *model.Connection
	}{
		{"inactive", func() *model.Connection {
			c := activeMicrosoftConn(goodMicrosoftCreds)
			c.Status = model.StatusInactive
			return c
		}},
		{"undecodable credentials", func() *model.Connection { return activeMicrosoftConn(`{not json`) }},
		{"incomplete credentials", func() *model.Connection { return activeMicrosoftConn(`{"ClientID":"only"}`) }},
	}
	for _, tc := range msCases {
		t.Run("microsoft/"+tc.name, func(t *testing.T) {
			d := NewMicrosoftDispatcher(fakeConnReader{conn: tc.conn()}, identityEncryptor{})
			_, err := d.ListAccounts(context.Background(), "cncf", model.ProviderMicrosoftAds)
			if err == nil {
				t.Fatal("discovery accepted a connection dispatch would reject")
			}
			if !errors.Is(err, domain.ErrConnectionNotUsable) {
				t.Errorf("error %v does not carry ErrConnectionNotUsable", err)
			}
		})
	}
}

// A picker row must say what the account IS. Returning an unusable account unmarked is worse
// than filtering it out: it looks exactly as selectable as a writable one, and the refusal
// arrives later at dispatch with no way back to this list.
func TestLinkedInAccountLabelSurfacesWhyAnAccountCannotServe(t *testing.T) {
	cases := []struct {
		name string
		in   linkedin.AdAccount
		want string
	}{
		{
			"servable account renders plainly",
			linkedin.AdAccount{ID: "1", Name: "LF Events", Status: "ACTIVE", Currency: "USD", ServingStatuses: []string{"RUNNABLE"}},
			"LF Events [USD]",
		},
		{
			// The case a lifecycle status alone cannot express: ACTIVE and unable to serve.
			"active but on billing hold",
			linkedin.AdAccount{ID: "1", Name: "LF Events", Status: "ACTIVE", Currency: "USD", ServingStatuses: []string{"BILLING_HOLD"}},
			"LF Events [USD] — on billing hold",
		},
		{
			// A test account never serves and never bills; binding a real campaign to one
			// produces a campaign that silently does nothing.
			"test account is named as such",
			linkedin.AdAccount{ID: "1", Name: "Sandbox", Status: "ACTIVE", Currency: "USD", Test: true, ServingStatuses: []string{"RUNNABLE"}},
			"Sandbox [USD] — TEST account — never serves",
		},
		{
			"no name falls back to the id",
			linkedin.AdAccount{ID: "507404993", Status: "ACTIVE", ServingStatuses: []string{"RUNNABLE"}},
			"507404993",
		},
		{
			// An absent servingStatuses is not evidence the account can spend: Servable is an
			// allow-list, so the label must not read as reassurance.
			"absent serving status is not treated as fine",
			linkedin.AdAccount{ID: "1", Name: "LF Events", Status: "ACTIVE"},
			"LF Events — cannot currently serve",
		},
		{
			// The gap every other case here misses by holding Status ACTIVE: lifecycle and
			// serving are independent, so a RUNNABLE account whose lifecycle is ABSENT clears
			// every serving check. StatusLabel() is "" for unrecognized as well as for ACTIVE,
			// so without a term keyed on Active() this renders as "LF Events [USD]" — identical
			// to the healthy row above, and the operator binds a paid campaign to it.
			"absent lifecycle status is not treated as active",
			linkedin.AdAccount{ID: "1", Name: "LF Events", Currency: "USD", ServingStatuses: []string{"RUNNABLE"}},
			"LF Events [USD] — account status could not be confirmed",
		},
		{
			// Same shape, but a status LinkedIn does document and this package does not map.
			// It must not be silently upgraded to healthy either.
			"unrecognized lifecycle status is not treated as active",
			linkedin.AdAccount{ID: "1", Name: "LF Events", Status: "SOMETHING_NEW", ServingStatuses: []string{"RUNNABLE"}},
			"LF Events — account status could not be confirmed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := linkedInAccountLabel(tc.in); got != tc.want {
				t.Errorf("label = %q, want %q", got, tc.want)
			}
		})
	}
}

// msWritableRole is any role Usable() admits. Its deny-list rejects RoleViewer and 0 (no
// role evidence) and allows everything else, so a concrete Microsoft role id stands in for
// "a credential that can actually write to this account". 41 is Super Admin.
const msWritableRole = 41

func TestMicrosoftAccountLabelSurfacesWhyAnAccountCannotServe(t *testing.T) {
	cases := []struct {
		name string
		in   microsoft.AdAccount
		want string
	}{
		{
			"usable account renders plainly",
			microsoft.AdAccount{ID: "1234567", Name: "LF Events", Number: "X1234567", Status: "Active", RoleID: msWritableRole},
			"LF Events (X1234567)",
		},
		{
			"suspended account says so",
			microsoft.AdAccount{ID: "1234567", Name: "LF Events", Number: "X1234567", Status: "Suspended", RoleID: msWritableRole},
			"LF Events (X1234567) — suspended",
		},
		{
			// Role is a different question from status: an ACTIVE, unpaused account the
			// credential can only READ is still unusable for a create.
			"viewer-only account is marked not writable",
			microsoft.AdAccount{ID: "1234567", Name: "LF Events", Number: "X1234567", Status: "Active", RoleID: microsoft.RoleViewer},
			"LF Events (X1234567) — not writable with this credential",
		},
		{
			// Usable() is false for a non-Active status AND for a read-only role, and
			// StatusLabel renders nothing for an ABSENT or unrecognised status — so this
			// case fell through to the role message and blamed a credential whose role
			// (41) is writable. An operator was sent to check permissions that are fine.
			// Raised by Copilot on #132.
			"absent status is not blamed on the role",
			microsoft.AdAccount{ID: "1234567", Name: "LF Events", Number: "X1234567", Status: "", RoleID: msWritableRole},
			"LF Events (X1234567) — account status could not be confirmed",
		},
		{
			// Raised by dealako on #132. Usable() compares Status EXACTLY, so " Active " is not
			// Active and the account is unusable — but a TrimSpace test in the label arm read it
			// as Active and blamed the ROLE, which is the mislabel this arm exists to remove.
			// The label predicate must be the same one the gate used, or it explains a decision
			// that was never taken.
			"padded status is not blamed on the role",
			microsoft.AdAccount{ID: "1234567", Name: "LF Events", Number: "X1234567", Status: " Active ", RoleID: msWritableRole},
			"LF Events (X1234567) — account status could not be confirmed",
		},
		{
			"no name falls back to the number",
			microsoft.AdAccount{ID: "1234567", Number: "X1234567", Status: "Active", RoleID: msWritableRole},
			"X1234567",
		},
		{
			"no name and no number falls back to the id",
			microsoft.AdAccount{ID: "1234567", Status: "Active", RoleID: msWritableRole},
			"1234567",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := microsoftAccountLabel(tc.in); got != tc.want {
				t.Errorf("label = %q, want %q", got, tc.want)
			}
		})
	}
}

// A zero-account upstream answer must return an EMPTY, NON-NIL slice.
//
// Both dispatchers rely on `make([]model.AccessibleAccount, 0, len(...))` to guarantee
// that, and nothing pinned it: every other fixture in this file returns exactly one
// element, so a regression to `var accounts []model.AccessibleAccount` would still pass
// them all while returning nil on the zero case. The distinction is load-bearing — an
// empty list means "these credentials reach no accounts", which is a real answer a picker
// can render, while nil is indistinguishable from "this platform cannot list accounts".
// Raised by dealako in review of #132.
func TestListAccountsReturnsEmptyNotNilWhenUpstreamHasNone(t *testing.T) {
	t.Run("linkedin", func(t *testing.T) {
		rec := &requestRecorder{}
		srv := linkedInAccountsServer(t, rec, "") // zero elements

		conn := activeLinkedInConn(goodLinkedInCreds)
		d := NewLinkedInDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, linkedin.WithBaseURL(srv.URL))

		accounts, err := d.ListAccounts(context.Background(), "cncf", model.ProviderLinkedInAds)
		if err != nil {
			t.Fatalf("ListAccounts: %v", err)
		}
		if accounts == nil {
			t.Error("returned NIL for a zero-account answer; the orchestrator cannot tell that from an unsupported platform")
		}
		if len(accounts) != 0 {
			t.Errorf("want an empty list, got %+v", accounts)
		}
	})

	t.Run("microsoft", func(t *testing.T) {
		// No requestRecorder here: this subtest asserts on the RESULT (empty, non-nil),
		// not on the outbound request. Recording requests nobody reads is scaffolding a
		// later reader would mistake for an intended assertion.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.Contains(r.URL.Path, "/token"):
				_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
			case strings.Contains(r.URL.Path, "User"):
				_, _ = io.WriteString(w, `{"CustomerRoles":[{"CustomerId":9999999,"RoleId":41}]}`)
			default:
				// The customer is reachable but holds no ad accounts.
				_, _ = io.WriteString(w, `{"AccountsInfo":[]}`)
			}
		}))
		t.Cleanup(srv.Close)

		conn := activeMicrosoftConn(goodMicrosoftCreds)
		d := NewMicrosoftDispatcher(fakeConnReader{conn: conn}, identityEncryptor{},
			microsoft.WithBaseURL(srv.URL), microsoft.WithCustomerBaseURL(srv.URL), microsoft.WithTokenURL(srv.URL+"/token"))

		accounts, err := d.ListAccounts(context.Background(), "cncf", model.ProviderMicrosoftAds)
		if err != nil {
			t.Fatalf("ListAccounts: %v", err)
		}
		if accounts == nil {
			t.Error("returned NIL for a zero-account answer; the orchestrator cannot tell that from an unsupported platform")
		}
		if len(accounts) != 0 {
			t.Errorf("want an empty list, got %+v", accounts)
		}
	})
}

// twitterAccountsServer answers GET /{version}/accounts with a fixed element set. The
// `next_cursor` key is REQUIRED in the body: the client refuses a response without it
// rather than reading the zero value as an exhausted cursor, because that would return a
// truncated account list as a complete one.
func twitterAccountsServer(t *testing.T, rec *requestRecorder, elements string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[`+elements+`],"next_cursor":null}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The account-less connection is the state discovery exists to rescue.
//
// validateTwitterConnection hard-fails on a missing account id for DISPATCH — correctly,
// since a client built without one cannot reach the platform — and ListAccounts tolerates
// exactly that one sentinel. Asserted through a fake server so the test proves the listing
// COMPLETED rather than merely that some other error came back.
func TestTwitterListAccountsWorksWithoutASelectedAccount(t *testing.T) {
	rec := &requestRecorder{}
	srv := twitterAccountsServer(t, rec, `{"id":"18ce54d4x5t","name":"LF Events","approval_status":"ACCEPTED"}`)

	conn := activeTwitterConn(goodTwitterCreds)
	conn.AccountID = "" // the state under test
	d := NewTwitterDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, twitter.WithBaseURL(srv.URL))

	accounts, err := d.ListAccounts(context.Background(), "cncf", model.ProviderTwitterAds)
	if err != nil {
		t.Fatalf("discovery must work on a connection with no account id — that is the connection it exists to serve: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ID != "18ce54d4x5t" {
		t.Fatalf("accounts = %+v, want the one the platform reported", accounts)
	}
	if accounts[0].Label != "LF Events" {
		t.Errorf("label = %q, want the account name", accounts[0].Label)
	}
}

// TestTwitterListAccountsWorksWithoutAFundingInstrument pins the OTHER create-only field.
// funding_instrument_id is required to CREATE a campaign, and Dispatch refuses without it,
// but an account-less connection has no reason to have chosen one yet — requiring it here
// would refuse the very connection this endpoint exists to complete. validateTwitterConnection
// deliberately does not check it; this test is what fails if a future edit moves that check
// into the shared validator.
func TestTwitterListAccountsWorksWithoutAFundingInstrument(t *testing.T) {
	rec := &requestRecorder{}
	srv := twitterAccountsServer(t, rec, `{"id":"18ce54d4x5t","name":"LF Events"}`)

	conn := activeTwitterConn(goodTwitterCreds)
	conn.AccountID = ""
	conn.ProviderConfig = nil // no funding instrument chosen either
	d := NewTwitterDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, twitter.WithBaseURL(srv.URL))

	if _, err := d.ListAccounts(context.Background(), "cncf", model.ProviderTwitterAds); err != nil {
		t.Fatalf("discovery must not require the create-only funding instrument: %v", err)
	}
}

// The request must ask about the CREDENTIAL, not about the stored account.
//
// This is the test that catches a discovery client scoped to one account: such a client
// still returns a plausible non-empty list, so only the outbound request shows the
// difference. The fixture DOES carry an account id, so there is something that could leak
// — into the PATH (/12/accounts/{id}, the single-resource form every other call in the
// client uses) or into the QUERY (X's `account_ids` parameter).
func TestTwitterListAccountsAsksAboutTheCredentialNotTheAccount(t *testing.T) {
	rec := &requestRecorder{}
	srv := twitterAccountsServer(t, rec, `{"id":"18ce54d4x5t","name":"LF Events"}`)

	conn := activeTwitterConn(goodTwitterCreds)
	conn.AccountID = "8r7gb"
	d := NewTwitterDispatcher(fakeConnReader{conn: conn}, identityEncryptor{}, twitter.WithBaseURL(srv.URL))

	if _, err := d.ListAccounts(context.Background(), "cncf", model.ProviderTwitterAds); err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	paths, queries, _ := rec.all()
	if len(paths) == 0 {
		t.Fatal("no request was made")
	}
	for i := range paths {
		if paths[i] != "/12/accounts" {
			t.Errorf("path = %q, want the COLLECTION /12/accounts", paths[i])
		}
		if strings.Contains(paths[i], "8r7gb") || strings.Contains(queries[i], "8r7gb") {
			t.Errorf("the stored account id leaked into the discovery request %q?%q — the client answers a narrower question than was asked",
				paths[i], queries[i])
		}
		if strings.Contains(queries[i], "account_ids") {
			t.Errorf("query %q sends account_ids, which scopes the answer to a subset", queries[i])
		}
	}
}

// Discovery must reject every connection dispatch rejects, EXCEPT the account-less one.
// Sharing validateTwitterConnection is what guarantees it; this pins that the sentinel
// survives, since the endpoint's 400-vs-503 mapping keys on ErrConnectionNotUsable.
func TestTwitterListAccountsStillRejectsAnUnusableConnection(t *testing.T) {
	cases := []struct {
		name string
		conn func() *model.Connection
	}{
		{"inactive", func() *model.Connection {
			c := activeTwitterConn(goodTwitterCreds)
			c.Status = model.StatusInactive
			return c
		}},
		{"undecodable credentials", func() *model.Connection { return activeTwitterConn(`{not json`) }},
		{"incomplete credentials", func() *model.Connection {
			return activeTwitterConn(`{"ConsumerKey":"ck","ConsumerSecret":"cs"}`)
		}},
	}
	for _, tc := range cases {
		t.Run("twitter/"+tc.name, func(t *testing.T) {
			d := NewTwitterDispatcher(fakeConnReader{conn: tc.conn()}, identityEncryptor{})
			_, err := d.ListAccounts(context.Background(), "cncf", model.ProviderTwitterAds)
			if err == nil {
				t.Fatal("discovery accepted a connection dispatch would reject")
			}
			if !errors.Is(err, domain.ErrConnectionNotUsable) {
				t.Errorf("error %v does not carry ErrConnectionNotUsable, so the endpoint cannot map it to the right status", err)
			}
		})
	}
}

// An upstream that reports zero accounts must produce an EMPTY, non-nil slice.
// Orchestrator.ReadAccounts rejects a nil result as a contract violation precisely so
// empty keeps its meaning, and on the wire nil would serialize as null.
func TestTwitterListAccountsReturnsEmptyNotNilWhenUpstreamHasNone(t *testing.T) {
	rec := &requestRecorder{}
	srv := twitterAccountsServer(t, rec, ``)

	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, twitter.WithBaseURL(srv.URL))
	accounts, err := d.ListAccounts(context.Background(), "cncf", model.ProviderTwitterAds)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if accounts == nil {
		t.Fatal("a credential reaching zero accounts is an ANSWER; nil is not distinguishable from \"no answer\" on the wire")
	}
	if len(accounts) != 0 {
		t.Fatalf("accounts = %+v, want empty", accounts)
	}
}

// A picker row must say what the account IS. Returning an unusable account unmarked is
// worse than filtering it out: it looks exactly as selectable as a usable one, and the
// refusal arrives later at dispatch with no way back to this list.
func TestTwitterAccountLabelSurfacesWhyAnAccountCannotBeUsed(t *testing.T) {
	cases := []struct {
		name string
		in   twitter.AdAccount
		want string
	}{
		{"plain accepted account renders as its name", twitter.AdAccount{ID: "a1", Name: "LF Events", Status: "ACCEPTED"}, "LF Events"},
		{"under review is named", twitter.AdAccount{ID: "a1", Name: "LF Events", Status: "UNDER_REVIEW"}, "LF Events — under review"},
		{"rejected is named", twitter.AdAccount{ID: "a1", Name: "LF Events", Status: "REJECTED"}, "LF Events — rejected"},
		{"deleted is named", twitter.AdAccount{ID: "a1", Name: "LF Events", Status: "ACCEPTED", Deleted: true}, "LF Events — deleted"},
		{"both reasons are named", twitter.AdAccount{ID: "a1", Name: "LF Events", Status: "REJECTED", Deleted: true}, "LF Events — rejected, deleted"},
		// A blank row in a picker is unpickable, and the id is what actually gets stored.
		{"a nameless account falls back to its id", twitter.AdAccount{ID: "18ce54d4x5t"}, "18ce54d4x5t"},
		{"a whitespace-only name falls back too", twitter.AdAccount{ID: "18ce54d4x5t", Name: "   "}, "18ce54d4x5t"},
		// X publishes no complete approval_status enum, so an UNRECOGNISED value must not
		// be rendered as a defect — the label says nothing rather than guessing.
		{"an unrecognised status is not labelled a defect", twitter.AdAccount{ID: "a1", Name: "LF Events", Status: "SOMETHING_NEW"}, "LF Events"},
		// The timezone is a PROPERTY, not a defect, so it joins the NAME rather than the
		// notes — the same shape linkedInAccountLabel gives Currency. X reports campaign
		// schedules and daily budget resets against it, so without this term two accounts
		// differing only by timezone render identically and the picker cannot tell them
		// apart. Asserting the full string is what makes that binding: a label that dropped
		// the timezone would still contain the name.
		{"the timezone is rendered into the name", twitter.AdAccount{ID: "a1", Name: "LF Events", Status: "ACCEPTED", Timezone: "America/Los_Angeles"}, "LF Events [America/Los_Angeles]"},
		{"two accounts differing only by timezone are distinguishable", twitter.AdAccount{ID: "a2", Name: "LF Events", Status: "ACCEPTED", Timezone: "Europe/Berlin"}, "LF Events [Europe/Berlin]"},
		// The timezone precedes the notes: it qualifies WHICH account this is, while the
		// notes say why that account may not be usable.
		{"timezone and a defect note coexist in that order", twitter.AdAccount{ID: "a1", Name: "LF Events", Status: "REJECTED", Timezone: "Europe/Berlin"}, "LF Events [Europe/Berlin] — rejected"},
		// An absent timezone must add no empty brackets.
		{"an absent timezone adds nothing", twitter.AdAccount{ID: "a1", Name: "LF Events", Status: "ACCEPTED"}, "LF Events"},
		{"a whitespace-only timezone adds nothing", twitter.AdAccount{ID: "a1", Name: "LF Events", Status: "ACCEPTED", Timezone: "   "}, "LF Events"},
		// The id fallback happens BEFORE the timezone is appended, so a nameless account
		// still renders its id rather than a bare bracketed timezone.
		{"a nameless account keeps its id in front of the timezone", twitter.AdAccount{ID: "18ce54d4x5t", Timezone: "Asia/Tokyo"}, "18ce54d4x5t [Asia/Tokyo]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := twitterAccountLabel(tc.in); got != tc.want {
				t.Errorf("twitterAccountLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTwitterListAccountsReturnsUnusableAccountsRatherThanFilteringThem is the
// dispatcher-level counterpart to the client's picker test, and it exists because a
// mutation SURVIVED without it: every other fixture in this file returns only ACCEPTED,
// non-deleted accounts, so a ListAccounts that silently dropped unusable rows removed
// nothing and the whole suite passed. Filtering here would answer "your credential reaches
// no ad accounts" about an account sitting right there, sending the operator to hunt a
// permissions problem that does not exist — and the label test alone cannot catch it,
// because a filtered row never reaches the labeller.
func TestTwitterListAccountsReturnsUnusableAccountsRatherThanFilteringThem(t *testing.T) {
	rec := &requestRecorder{}
	srv := twitterAccountsServer(t, rec,
		`{"id":"good1","name":"Usable","approval_status":"ACCEPTED"},`+
			`{"id":"rev1","name":"Pending","approval_status":"UNDER_REVIEW"},`+
			`{"id":"rej1","name":"Refused","approval_status":"REJECTED"},`+
			`{"id":"del1","name":"Gone","approval_status":"ACCEPTED","deleted":true}`)

	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{}, twitter.WithBaseURL(srv.URL))
	accounts, err := d.ListAccounts(context.Background(), "cncf", model.ProviderTwitterAds)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 4 {
		t.Fatalf("accounts = %+v (%d rows), want all 4 — unusable accounts are LABELLED, never dropped", accounts, len(accounts))
	}
	byID := map[string]string{}
	for _, a := range accounts {
		byID[a.ID] = a.Label
	}
	// Each unusable row must arrive AND carry its reason, so the picker can show why.
	for id, wantNote := range map[string]string{
		"rev1": "under review",
		"rej1": "rejected",
		"del1": "deleted",
	} {
		label, ok := byID[id]
		if !ok {
			t.Errorf("account %s was dropped; it must be offered with its reason", id)
			continue
		}
		if !strings.Contains(label, wantNote) {
			t.Errorf("account %s label = %q, want it to carry %q", id, label, wantNote)
		}
	}
	if label := byID["good1"]; label != "Usable" {
		t.Errorf("usable account label = %q, want the bare name with no note", label)
	}
}
