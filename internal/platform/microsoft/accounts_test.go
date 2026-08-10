// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newCustomerClient wires a client whose CUSTOMER MANAGEMENT base points at a test
// server, leaving the campaign base alone. Two separate hosts is the whole reason
// doCustomerRequest exists, so a helper that pointed both at one server would make a
// call routed to the wrong service look correct.
func newCustomerClient(t *testing.T, account AccountConfig, h http.HandlerFunc) *Client {
	t.Helper()
	tok := httptest.NewServer(http.HandlerFunc(tokenHandler))
	t.Cleanup(tok.Close)
	api := httptest.NewServer(h)
	t.Cleanup(api.Close)
	return NewClient(testCreds(), account,
		WithTokenURL(tok.URL), WithCustomerBaseURL(api.URL),
		WithBaseURL("http://campaign.invalid"),
		WithClock(fixedClock()), withRetryBaseDelay(time.Millisecond))
}

// acctRecorder captures what a fake handler saw. The handler runs on the SERVER's
// goroutine while the test goroutine reads the captures after the call returns: that
// pairing is ordered in practice but carries no happens-before edge, so -race is
// entitled to report it. The mutex matches the atomic/mutex discipline the 429 tests in
// client_test.go already use.
//
// It also decodes the body HERE and records any failure rather than calling t.Fatalf.
// Fatalf is documented as callable only from the goroutine running the test; from a
// handler it runtime.Goexit()s the wrong goroutine, so the test carries on against a
// half-filled recorder and reports whatever assertion happens to fail next instead of
// the decode error that actually went wrong.
type acctRecorder struct {
	mu      sync.Mutex
	path    string
	method  string
	acct    string
	hasAcct bool
	cust    string
	dev     string
	body    map[string]any
	bodyErr error
}

func (a *acctRecorder) capture(r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.path, a.method = r.URL.Path, r.Method
	a.acct = r.Header.Get("CustomerAccountId")
	// Go canonicalises header names in the map, so the presence probe uses that form.
	_, a.hasAcct = r.Header["Customeraccountid"]
	a.cust = r.Header.Get("CustomerId")
	a.dev = r.Header.Get("DeveloperToken")
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		a.bodyErr = fmt.Errorf("read body: %w", err)
		return
	}
	// An EMPTY body is not a decode failure — the campaign-path guard below issues a
	// GET with no body at all, and body stays nil for it. Only a non-empty body that
	// will not parse is a problem worth failing on.
	if len(raw) == 0 {
		return
	}
	// UseNumber, not a plain Unmarshal: into `any` a JSON number becomes a float64, so a
	// test asserting on a customer or account id in the body would compare against
	// "5.550001e+06" and, above 2^53, against a DIFFERENT id than the client sent —
	// exactly the precision loss the production decode uses json.Number to avoid.
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&a.body); err != nil {
		a.bodyErr = fmt.Errorf("body is not JSON: %w (%s)", err, raw)
	}
}

// read returns a snapshot under the lock and fails the test on the TEST goroutine if
// the handler could not decode the body.
func (a *acctRecorder) read(t *testing.T) acctRecorder {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bodyErr != nil {
		t.Fatalf("handler could not decode the request: %v", a.bodyErr)
	}
	return acctRecorder{
		path: a.path, method: a.method, acct: a.acct, hasAcct: a.hasAcct,
		cust: a.cust, dev: a.dev, body: a.body,
	}
}

// withCustomerRoles answers User/Query with one CustomerRole per id and delegates every
// other path to next. Discovery on a connection with no configured customer now makes two
// different calls, and a helper that served one canned body to both would let a test pass
// against a client that sent the account query to the wrong path.
func withCustomerRoles(ids []string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/User/Query") {
			roles := make([]string, 0, len(ids))
			for _, id := range ids {
				roles = append(roles, `{"CustomerId":`+id+`}`)
			}
			_, _ = io.WriteString(w, `{"CustomerRoles":[`+strings.Join(roles, ",")+`]}`)
			return
		}
		next(w, r)
	}
}

// TestListAdAccounts_AsksAboutTheCredentialsNotAnAccount pins the property the whole
// feature rests on: discovery must work for a connection that has NO account id, since
// that is the state it exists to resolve. It asserts the request carries no ACCOUNT
// identity — neither the CustomerAccountId header nor the CustomerId one — and lands on
// the Customer Management path rather than Campaign Management. The customer id in the
// BODY is a different thing: it is discovered from the credentials, not asserted by the
// connection.
func TestListAdAccounts_AsksAboutTheCredentialsNotAnAccount(t *testing.T) {
	rec := &acctRecorder{}
	c := newCustomerClient(t, AccountConfig{}, withCustomerRoles([]string{"5550001"},
		func(w http.ResponseWriter, r *http.Request) {
			rec.capture(r)
			_, _ = io.WriteString(w, `{"AccountsInfo":[]}`)
		}))

	got, err := c.ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts with no account id must succeed: %v", err)
	}
	if got == nil {
		t.Error("zero accounts must be an empty slice, not nil: nil serializes as null, which reads as 'no answer' rather than 'no accounts'")
	}
	saw := rec.read(t)
	if want := "/CustomerManagement/v13/AccountsInfo/Query"; saw.path != want {
		t.Errorf("path = %q, want %q", saw.path, want)
	}
	if saw.method != http.MethodPost {
		t.Errorf("method = %q, want POST", saw.method)
	}
	// Presence, not value: an EMPTY CustomerAccountId is still a claim about an account,
	// and the connection making this call has none. Asserting only on the value would pass
	// against a client that always sets the header.
	if saw.hasAcct {
		t.Errorf("CustomerAccountId was sent (value %q); a discovery call must omit the header entirely", saw.acct)
	}
	if saw.cust != "" {
		t.Errorf("CustomerId = %q, want it omitted when the connection has none", saw.cust)
	}
	if saw.dev != "devtok" {
		t.Errorf("DeveloperToken = %q, want it still sent", saw.dev)
	}
	// The BODY does carry a customer id, and it is not the connection's — the connection
	// has none. It is the one the credentials' own CustomerRole named. Sending nothing
	// would make Microsoft pick a single customer for us, which is the narrowing this
	// call exists to avoid; sending 0 or null would be a request for customer zero.
	if v, _ := saw.body["CustomerId"].(json.Number); v.String() != "5550001" {
		t.Errorf("body CustomerId = %v, want the customer id discovered from the credentials' own role", saw.body["CustomerId"])
	}
	if v, ok := saw.body["OnlyParentAccounts"].(bool); !ok || v {
		t.Errorf("OnlyParentAccounts = %v, want false so accounts LINKED to this customer are included alongside the ones it owns", saw.body["OnlyParentAccounts"])
	}
}

// TestListAdAccounts_SendsAConfiguredCustomerID is the other half: when the connection
// DOES carry a customer id, it must reach Microsoft, or an agency connection silently
// enumerates the wrong customer's accounts.
func TestListAdAccounts_SendsAConfiguredCustomerID(t *testing.T) {
	rec := &acctRecorder{}
	c := newCustomerClient(t, AccountConfig{CustomerID: "9988776"}, func(w http.ResponseWriter, r *http.Request) {
		rec.capture(r)
		_, _ = io.WriteString(w, `{"AccountsInfo":[]}`)
	})
	if _, err := c.ListAdAccounts(context.Background()); err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	saw := rec.read(t)
	if saw.cust != "9988776" {
		t.Errorf("CustomerId header = %q, want 9988776", saw.cust)
	}
	// json.Number must reach the wire as a NUMBER; Microsoft types CustomerId as long,
	// and a quoted string is a different request. The recorder decodes with UseNumber,
	// so a JSON number lands as json.Number and a quoted one as a Go string — this
	// assertion fails on the quoted form rather than accepting it.
	if v, ok := saw.body["CustomerId"].(json.Number); !ok || v.String() != "9988776" {
		t.Errorf("body CustomerId = %#v, want the number 9988776", saw.body["CustomerId"])
	}
}

// TestListAdAccounts_AConfiguredCustomerIDSkipsRoleDiscovery pins the other half of that
// decision. A connection scoped to a customer on purpose must not be widened back out to
// every customer the credentials reach — the operator excluded them — and it must not pay
// for a User/Query it cannot use.
func TestListAdAccounts_AConfiguredCustomerIDSkipsRoleDiscovery(t *testing.T) {
	var userQueries, accountQueries int32
	c := newCustomerClient(t, AccountConfig{CustomerID: "9988776"}, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/User/Query") {
			atomic.AddInt32(&userQueries, 1)
			// Two roles, neither of them the configured one: if role discovery ran,
			// the accounts below would be fetched twice and for the wrong customers.
			_, _ = io.WriteString(w, `{"CustomerRoles":[{"CustomerId":1111111},{"CustomerId":2222222}]}`)
			return
		}
		atomic.AddInt32(&accountQueries, 1)
		_, _ = io.WriteString(w, `{"AccountsInfo":[{"Id":1234567,"AccountLifeCycleStatus":"Active"}]}`)
	})
	got, err := c.ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	if n := atomic.LoadInt32(&userQueries); n != 0 {
		t.Errorf("User/Query calls = %d, want 0: the customer is already known", n)
	}
	if n := atomic.LoadInt32(&accountQueries); n != 1 {
		t.Errorf("AccountsInfo/Query calls = %d, want exactly 1", n)
	}
	if len(got) != 1 {
		t.Errorf("got %d accounts, want 1", len(got))
	}
}

// TestListAdAccounts_EnumeratesEveryCustomerTheCredentialsReach is the finding this
// method was rewritten for.
//
// AccountsInfo/Query is scoped to ONE customer whichever way it is called: Microsoft
// documents it as returning accounts "accessible from the specified customer", and
// omitting CustomerId only means "the user's credentials are used to determine THE
// customer" — still singular. A user administering several customers therefore got one
// customer's accounts and no indication the rest existed. A picker that quietly omits an
// account is worse than one that fails: the user concludes the account is not connectable
// and goes looking for a permissions problem that is not there.
//
// OnlyParentAccounts=false does not cover it. A linked account is one attached to the
// customer being queried; a second customer the same user administers is a different
// relationship, and only User/Query names it.
func TestListAdAccounts_EnumeratesEveryCustomerTheCredentialsReach(t *testing.T) {
	var mu sync.Mutex
	var queried []string
	c := newCustomerClient(t, AccountConfig{}, withCustomerRoles([]string{"1111111", "2222222"},
		func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				CustomerID json.Number `json:"CustomerId"`
			}
			dec := json.NewDecoder(r.Body)
			dec.UseNumber()
			if err := dec.Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			queried = append(queried, req.CustomerID.String())
			mu.Unlock()
			switch req.CustomerID.String() {
			case "1111111":
				_, _ = io.WriteString(w, `{"AccountsInfo":[{"Id":1000001,"Name":"First","AccountLifeCycleStatus":"Active"}]}`)
			case "2222222":
				_, _ = io.WriteString(w, `{"AccountsInfo":[{"Id":2000002,"Name":"Second","AccountLifeCycleStatus":"Active"}]}`)
			default:
				_, _ = io.WriteString(w, `{"AccountsInfo":[]}`)
			}
		}))

	got, err := c.ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	mu.Lock()
	sawQueried := strings.Join(queried, ",")
	mu.Unlock()
	if sawQueried != "1111111,2222222" {
		t.Errorf("customers queried = %q, want both roles enumerated in Microsoft's order", sawQueried)
	}
	if len(got) != 2 || got[0].ID != "1000001" || got[1].ID != "2000002" {
		t.Fatalf("accounts = %+v, want the union of both customers' accounts", got)
	}
}

// TestListAdAccounts_DeduplicatesALinkedAccount: the same account is reachable under more
// than one customer — that is what a link IS, and OnlyParentAccounts=false asks for them
// deliberately. Offering it twice makes a user wonder which entry is the real one.
func TestListAdAccounts_DeduplicatesALinkedAccount(t *testing.T) {
	c := newCustomerClient(t, AccountConfig{}, withCustomerRoles([]string{"1111111", "2222222"},
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"AccountsInfo":[{"Id":1000001,"Name":"Shared","AccountLifeCycleStatus":"Active"}]}`)
		}))
	got, err := c.ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d accounts, want the linked account offered once", len(got))
	}
}

// TestListAdAccounts_OneCustomerFailingFailsTheWholeCall. A partial union is exactly the
// bug this rewrite removes, and it is indistinguishable from a complete one at the
// boundary — so the second customer erroring must not degrade into the first customer's
// accounts.
func TestListAdAccounts_OneCustomerFailingFailsTheWholeCall(t *testing.T) {
	c := newCustomerClient(t, AccountConfig{}, withCustomerRoles([]string{"1111111", "2222222"},
		func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				CustomerID json.Number `json:"CustomerId"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.CustomerID.String() == "2222222" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = io.WriteString(w, `{"AccountsInfo":[{"Id":1000001,"AccountLifeCycleStatus":"Active"}]}`)
		}))
	got, err := c.ListAdAccounts(context.Background())
	if err == nil {
		t.Fatalf("want an error, got %d accounts — a short list reads as a complete one", len(got))
	}
	if got != nil {
		t.Errorf("accounts = %+v, want nil alongside the error", got)
	}
}

// TestListAdAccounts_RoleDiscoveryFailsClosed. Microsoft documents "at minimum one list
// item will be returned", so an absent OR empty CustomerRoles is a response that does not
// match the contract — not the answer "these credentials reach no customers". Reporting
// it as zero accounts would send the user to look for a permissions problem when the
// protocol changed underneath us. An unusable customer id fails for the same reason it
// does on the account side: a response that far from the documented shape is not the
// response we think it is.
func TestListAdAccounts_RoleDiscoveryFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"absent":         `{}`,
		"empty":          `{"CustomerRoles":[]}`,
		"null":           `{"CustomerRoles":null}`,
		"non-integer id": `{"CustomerRoles":[{"CustomerId":1.5e3}]}`,
		"negative id":    `{"CustomerRoles":[{"CustomerId":-1}]}`,
		// Zero and an int64 overflow are digit strings, so a transport-shaped check
		// (`^[0-9]+$`) passes them; a Microsoft customer id is a POSITIVE int64, so
		// neither can name one and querying them would be a request about nothing.
		"zero id":           `{"CustomerRoles":[{"CustomerId":0}]}`,
		"overflows int64":   `{"CustomerRoles":[{"CustomerId":9223372036854775808}]}`,
		"undecodable":       `not json`,
		"missing id":        `{"CustomerRoles":[{}]}`,
		"one bad among two": `{"CustomerRoles":[{"CustomerId":1111111},{"CustomerId":-1}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := newCustomerClient(t, AccountConfig{}, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/User/Query") {
					_, _ = io.WriteString(w, body)
					return
				}
				_, _ = io.WriteString(w, `{"AccountsInfo":[{"Id":1000001,"AccountLifeCycleStatus":"Active"}]}`)
			})
			got, err := c.ListAdAccounts(context.Background())
			if err == nil {
				t.Fatalf("want an error, got %d accounts", len(got))
			}
			if got != nil {
				t.Errorf("accounts = %+v, want nil alongside the error", got)
			}
		})
	}
}

// TestListAdAccounts_RoleDiscoveryNeverEchoesTheBody. A User/Query body is the one most
// likely to carry personal data — Microsoft's User object has a contact block, a password
// field and a secret answer — and an error travels further than the response does.
func TestListAdAccounts_RoleDiscoveryNeverEchoesTheBody(t *testing.T) {
	const marker = "s3cret-user-marker"
	c := newCustomerClient(t, AccountConfig{}, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/User/Query") {
			_, _ = io.WriteString(w, `not json `+marker)
			return
		}
		_, _ = io.WriteString(w, `{"AccountsInfo":[]}`)
	})
	_, err := c.ListAdAccounts(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("error echoed the User/Query body: %q", err.Error())
	}
}

// TestListAdAccounts_OmitsTheAccountHeaderEvenWhenOneIsConfigured is the case the
// no-account test cannot reach.
//
// Discovery exists partly to RE-POINT a connection that already has an account, so the
// configured-account path is not a corner: it is half the traffic. A client that sent
// CustomerAccountId "only when there is one to send" would look perfectly reasonable and
// would pass the empty-config assertion, while scoping every re-point to the account the
// user is trying to move away from — which returns that account and hides the rest.
func TestListAdAccounts_OmitsTheAccountHeaderEvenWhenOneIsConfigured(t *testing.T) {
	rec := &acctRecorder{}
	c := newCustomerClient(t, AccountConfig{AccountID: "1234567", CustomerID: "9988776"},
		func(w http.ResponseWriter, r *http.Request) {
			rec.capture(r)
			_, _ = io.WriteString(w, `{"AccountsInfo":[]}`)
		})
	if _, err := c.ListAdAccounts(context.Background()); err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	saw := rec.read(t)
	if saw.hasAcct {
		t.Errorf("CustomerAccountId was sent (value %q); discovery asks about the CREDENTIALS, "+
			"and scoping it to the configured account would hide every other account the user "+
			"might be re-pointing to", saw.acct)
	}
	// The customer id is a different thing and MUST still be sent — this test must not
	// pass by way of a client that dropped both.
	if saw.cust != "9988776" {
		t.Errorf("CustomerId header = %q, want 9988776 still sent", saw.cust)
	}
}

func TestListAdAccounts_RejectsAMalformedCustomerID(t *testing.T) {
	c := newCustomerClient(t, AccountConfig{CustomerID: "99\r\nX-Injected: 1"}, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a malformed customer id must be rejected before any request is sent")
		w.WriteHeader(http.StatusOK)
	})
	if _, err := c.ListAdAccounts(context.Background()); err == nil {
		t.Fatal("want an error for a non-digits customer id")
	}
}

// TestListAdAccounts_ReturnsUnusableAccountsWithTheirReason is the anti-filter test.
// Dropping a suspended or paused account answers "your credentials reach no ad accounts"
// about an account sitting right there, sending the user to look for a permissions
// problem that does not exist.
func TestListAdAccounts_ReturnsUnusableAccountsWithTheirReason(t *testing.T) {
	c := newCustomerClient(t, AccountConfig{}, withCustomerRoles([]string{"5550001"},
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"AccountsInfo":[
			{"Id":1234567,"Name":" Good Account ","Number":"X1234567","AccountLifeCycleStatus":"Active"},
			{"Id":2222222,"Name":"Suspended One","Number":"X2222222","AccountLifeCycleStatus":"Suspended"},
			{"Id":3333333,"Name":"Billing Paused","Number":"X3333333","AccountLifeCycleStatus":"Active","PauseReason":2},
			{"Id":4444444,"Name":"Draft One","Number":"X4444444","AccountLifeCycleStatus":"Draft"},
			{"Id":5555555,"Name":"Odd One","Number":"X5555555","AccountLifeCycleStatus":"Nebulous","PauseReason":9}
		]}`)
		}))
	got, err := c.ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d accounts, want all 5 returned rather than filtered", len(got))
	}

	for _, tc := range []struct {
		idx         int
		usable      bool
		statusLabel string
		pauseLabel  string
	}{
		{0, true, "", ""},
		{1, false, "suspended", ""},
		// Active but paused: the two fields disagree, which is exactly why they are
		// kept apart. Usable must be false, and the reason must come from the pause
		// field, since the status field has nothing to report.
		{2, false, "", "paused by billing"},
		{3, false, "not finished being set up", ""},
		// Unrecognized on BOTH axes. Usable defaults to false (allow-list), the status
		// has no label because this package has nothing to say about it, and the flag
		// is reported verbatim rather than guessed at.
		{4, false, "", "paused (unrecognized reason 9)"},
	} {
		a := got[tc.idx]
		if a.Usable() != tc.usable {
			t.Errorf("account %d (%s/%d) Usable() = %v, want %v", tc.idx, a.Status, a.PauseReason, a.Usable(), tc.usable)
		}
		if a.StatusLabel() != tc.statusLabel {
			t.Errorf("account %d StatusLabel() = %q, want %q", tc.idx, a.StatusLabel(), tc.statusLabel)
		}
		if a.PauseLabel() != tc.pauseLabel {
			t.Errorf("account %d PauseLabel() = %q, want %q", tc.idx, a.PauseLabel(), tc.pauseLabel)
		}
	}
	if got[0].Name != "Good Account" {
		t.Errorf("name = %q, want it trimmed", got[0].Name)
	}
	if got[0].Number != "X1234567" {
		t.Errorf("number = %q, want Microsoft's human-facing account number preserved", got[0].Number)
	}
	if got[0].ID != "1234567" {
		t.Errorf("id = %q, want the digits-only form the connection stores", got[0].ID)
	}
}

// TestListAdAccounts_AbsentEnvelopeIsNotZeroAccounts is the false-absence guard. `{}`
// and `{"AccountsInfo":[]}` mean different things, and only the second is an answer.
func TestListAdAccounts_AbsentEnvelopeIsNotZeroAccounts(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"absent", `{}`},
		{"null", `{"AccountsInfo":null}`},
		{"not json", `<html>gateway</html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// withCustomerRoles, not a bare handler: role discovery runs FIRST for a
			// connection with no configured customer, so a handler that served tc.body to
			// every path would fail in discoveryCustomerIDs and never reach the guard this
			// test names. reached pins that — the assertions below are about the ACCOUNT
			// response, and a test that errors one call earlier passes for the wrong reason.
			var reached int32
			c := newCustomerClient(t, AccountConfig{}, withCustomerRoles([]string{"5550001"},
				func(w http.ResponseWriter, _ *http.Request) {
					atomic.AddInt32(&reached, 1)
					_, _ = io.WriteString(w, tc.body)
				}))
			got, err := c.ListAdAccounts(context.Background())
			if atomic.LoadInt32(&reached) == 0 {
				t.Fatal("AccountsInfo/Query was never reached; this test would pass on a role-discovery failure instead")
			}
			if err == nil {
				t.Fatalf("want an error, got %d accounts — a body that cannot prove a result set must not read as zero accounts", len(got))
			}
			if got != nil {
				t.Errorf("accounts = %#v, want nil alongside the error", got)
			}
		})
	}
}

// TestListAdAccounts_UnusableIDFailsTheWholeCall pins fail-rather-than-truncate. A row
// this far from the documented shape means the response is not the one we think it is,
// so skipping the row would return a SHORT list that is indistinguishable from a
// complete one — and the caller acts on the absence.
func TestListAdAccounts_UnusableIDFailsTheWholeCall(t *testing.T) {
	for _, tc := range []struct{ name, id string }{
		{"float", `1.5e3`},
		{"negative", `-1`},
		// Both pass a digits-only check and neither can be an account: an id is a
		// positive int64, so 0 names nothing and 2^63 is past the domain entirely.
		{"zero", `0`},
		{"overflows int64", `9223372036854775808`},
		{"string with letters", `"acct-7"`},
		{"empty string", `""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// See TestListAdAccounts_AbsentEnvelopeIsNotZeroAccounts: without
			// withCustomerRoles the rows below never reach the account decoder, and
			// removing the id validation would not make this test fail.
			var reached int32
			c := newCustomerClient(t, AccountConfig{}, withCustomerRoles([]string{"5550001"},
				func(w http.ResponseWriter, _ *http.Request) {
					atomic.AddInt32(&reached, 1)
					_, _ = io.WriteString(w, `{"AccountsInfo":[
					{"Id":1234567,"AccountLifeCycleStatus":"Active"},
					{"Id":`+tc.id+`,"AccountLifeCycleStatus":"Active"}
				]}`)
				}))
			got, err := c.ListAdAccounts(context.Background())
			if atomic.LoadInt32(&reached) == 0 {
				t.Fatal("AccountsInfo/Query was never reached; the id validation under test never ran")
			}
			if err == nil {
				t.Fatalf("want an error, got %d accounts", len(got))
			}
			if got != nil {
				t.Errorf("accounts = %#v, want nil rather than the partial list", got)
			}
		})
	}
}

// TestListAdAccounts_PreservesALargeID is the reason Id is decoded as json.Number.
// Microsoft types it as a long; decoding through float64 silently loses precision above
// 2^53 and yields a WRONG id that still looks like an id — which then gets stored on the
// connection and fails, or worse succeeds, against someone else's account.
func TestListAdAccounts_PreservesALargeID(t *testing.T) {
	const big = "9007199254740993" // 2^53 + 1: the smallest integer float64 cannot hold
	c := newCustomerClient(t, AccountConfig{}, withCustomerRoles([]string{"5550001"},
		func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"AccountsInfo":[{"Id":`+big+`,"AccountLifeCycleStatus":"Active"}]}`)
		}))
	got, err := c.ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	if len(got) != 1 || got[0].ID != big {
		t.Fatalf("id = %q, want %q exactly", got[0].ID, big)
	}
}

// TestListAdAccounts_NeverEchoesTheResponseBody: an upstream body this code has failed
// to parse is a body nothing is known about, and it travels in an error alongside the
// credentials' account context. This covers the ACCOUNT response specifically —
// TestListAdAccounts_RoleDiscoveryNeverEchoesTheBody covers the other call, and without
// withCustomerRoles here the two would be the same test twice.
func TestListAdAccounts_NeverEchoesTheResponseBody(t *testing.T) {
	const marker = "s3cret-marker"
	var reached int32
	c := newCustomerClient(t, AccountConfig{}, withCustomerRoles([]string{"5550001"},
		func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt32(&reached, 1)
			_, _ = io.WriteString(w, `not json `+marker)
		}))
	_, err := c.ListAdAccounts(context.Background())
	if atomic.LoadInt32(&reached) == 0 {
		t.Fatal("AccountsInfo/Query was never reached; this would duplicate the role-discovery redaction test")
	}
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("error echoed the response body: %q", err.Error())
	}
}

// TestListAdAccounts_RetriesA429 pins that discovery is treated as idempotent. It
// creates nothing, so a retry cannot double-create — and without the retry a transient
// rate limit fails a user's first attempt to connect an account.
func TestListAdAccounts_RetriesA429(t *testing.T) {
	var calls int32
	c := newCustomerClient(t, AccountConfig{}, withCustomerRoles([]string{"5550001"},
		func(w http.ResponseWriter, _ *http.Request) {
			if atomic.AddInt32(&calls, 1) == 1 {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = io.WriteString(w, `{"AccountsInfo":[{"Id":1234567,"AccountLifeCycleStatus":"Active"}]}`)
		}))
	got, err := c.ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("calls = %d, want the 429 retried once", n)
	}
	if len(got) != 1 {
		t.Errorf("got %d accounts, want 1", len(got))
	}
}

// TestListAdAccounts_RejectsAnUnusableConfiguredCustomer covers the asymmetry Copilot
// found: the configured customer id was being trusted more than a discovered one.
//
// doCustomerRequest does validate CustomerID, but as HEADER BYTES — accountIDRE is
// `^[0-9]+$`, so `0` and anything past MaxInt64 pass. On the discovery path that value is
// not a header, it is the answer to "whose accounts are these", enumerated under and
// offered as a picker. The two cases below are exactly the ones the transport check
// cannot see, and both are inert as bytes and impossible as identities.
func TestListAdAccounts_RejectsAnUnusableConfiguredCustomer(t *testing.T) {
	for _, tc := range []struct{ name, customer string }{
		{"zero", "0"},
		{"overflows int64", "9223372036854775808"},
		{"leading zero", "0123456"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCustomerClient(t, AccountConfig{CustomerID: tc.customer},
				func(_ http.ResponseWriter, _ *http.Request) {
					t.Error("a request was made; an unusable configured customer must fail " +
						"before anything is enumerated under it")
				})
			_, err := c.ListAdAccounts(context.Background())
			if err == nil {
				t.Fatalf("customer %q was accepted; it cannot name a real customer", tc.customer)
			}
			if !strings.Contains(err.Error(), "customer id") {
				t.Errorf("error does not say which value is wrong: %v", err)
			}
		})
	}
}
