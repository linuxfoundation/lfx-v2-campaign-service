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

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
)

// Both dispatchers must SATISFY the AccountLister interface, or Orchestrator.ReadAccounts
// type-asserts, misses, and answers ErrAccountsUnsupported — the endpoint would exist and
// always fail. A compile-time assertion catches that at build rather than at runtime.
var (
	_ service.AccountLister = (*LinkedInDispatcher)(nil)
	_ service.AccountLister = (*MicrosoftDispatcher)(nil)
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

// The request must ask about the TOKEN, not about the stored account. A client scoped to one
// account returns a plausible one-account list, so the regression is invisible in the result
// and only the outbound request can catch it.
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

var _ = json.Marshal // keep encoding/json referenced if a future fixture drops its use
