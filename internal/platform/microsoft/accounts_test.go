// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

func accountsBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, raw)
	}
	return got
}

// TestListAdAccounts_AsksAboutTheCredentialsNotAnAccount pins the property the whole
// feature rests on: discovery must work for a connection that has NO account id, since
// that is the state it exists to resolve. It asserts the request carries no account
// identity at all — not the header, not a CustomerId — and lands on the Customer
// Management path rather than Campaign Management.
func TestListAdAccounts_AsksAboutTheCredentialsNotAnAccount(t *testing.T) {
	var gotPath, gotMethod, gotAcctHeader, gotCustHeader, gotDev string
	var hasAcctHeader bool
	var body map[string]any
	c := newCustomerClient(t, AccountConfig{}, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotAcctHeader = r.Header.Get("CustomerAccountId")
		_, hasAcctHeader = r.Header["Customeraccountid"]
		gotCustHeader = r.Header.Get("CustomerId")
		gotDev = r.Header.Get("DeveloperToken")
		body = accountsBody(t, r)
		_, _ = io.WriteString(w, `{"AccountsInfo":[]}`)
	})

	got, err := c.ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts with no account id must succeed: %v", err)
	}
	if got == nil {
		t.Error("zero accounts must be an empty slice, not nil: nil serializes as null, which reads as 'no answer' rather than 'no accounts'")
	}
	if want := "/CustomerManagement/v13/AccountsInfo/Query"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	// Presence, not value: an EMPTY CustomerAccountId is still a claim about an account,
	// and the connection making this call has none. Asserting only on the value would pass
	// against a client that always sets the header.
	if hasAcctHeader {
		t.Errorf("CustomerAccountId was sent (value %q); a discovery call must omit the header entirely", gotAcctHeader)
	}
	if gotCustHeader != "" {
		t.Errorf("CustomerId = %q, want it omitted when the connection has none", gotCustHeader)
	}
	if gotDev != "devtok" {
		t.Errorf("DeveloperToken = %q, want it still sent", gotDev)
	}
	if _, present := body["CustomerId"]; present {
		t.Error("CustomerId must be ABSENT from the body, not null or 0: Microsoft infers the customer from the credentials only when the element is omitted")
	}
	if v, ok := body["OnlyParentAccounts"].(bool); !ok || v {
		t.Errorf("OnlyParentAccounts = %v, want false so linked accounts under other customers are included", body["OnlyParentAccounts"])
	}
}

// TestListAdAccounts_SendsAConfiguredCustomerID is the other half: when the connection
// DOES carry a customer id, it must reach Microsoft, or an agency connection silently
// enumerates the wrong customer's accounts.
func TestListAdAccounts_SendsAConfiguredCustomerID(t *testing.T) {
	var gotHeader string
	var body map[string]any
	c := newCustomerClient(t, AccountConfig{CustomerID: "9988776"}, func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("CustomerId")
		body = accountsBody(t, r)
		_, _ = io.WriteString(w, `{"AccountsInfo":[]}`)
	})
	if _, err := c.ListAdAccounts(context.Background()); err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	if gotHeader != "9988776" {
		t.Errorf("CustomerId header = %q, want 9988776", gotHeader)
	}
	// json.Number must reach the wire as a NUMBER; Microsoft types CustomerId as long,
	// and a quoted string is a different request.
	if v, ok := body["CustomerId"].(float64); !ok || v != 9988776 {
		t.Errorf("body CustomerId = %#v, want the number 9988776", body["CustomerId"])
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
	c := newCustomerClient(t, AccountConfig{}, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"AccountsInfo":[
			{"Id":1234567,"Name":" Good Account ","Number":"X1234567","AccountLifeCycleStatus":"Active"},
			{"Id":2222222,"Name":"Suspended One","Number":"X2222222","AccountLifeCycleStatus":"Suspended"},
			{"Id":3333333,"Name":"Billing Paused","Number":"X3333333","AccountLifeCycleStatus":"Active","PauseReason":2},
			{"Id":4444444,"Name":"Draft One","Number":"X4444444","AccountLifeCycleStatus":"Draft"},
			{"Id":5555555,"Name":"Odd One","Number":"X5555555","AccountLifeCycleStatus":"Nebulous","PauseReason":9}
		]}`)
	})
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
			c := newCustomerClient(t, AccountConfig{}, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			})
			got, err := c.ListAdAccounts(context.Background())
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
		{"string with letters", `"acct-7"`},
		{"empty string", `""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCustomerClient(t, AccountConfig{}, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"AccountsInfo":[
					{"Id":1234567,"AccountLifeCycleStatus":"Active"},
					{"Id":`+tc.id+`,"AccountLifeCycleStatus":"Active"}
				]}`)
			})
			got, err := c.ListAdAccounts(context.Background())
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
	c := newCustomerClient(t, AccountConfig{}, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"AccountsInfo":[{"Id":`+big+`,"AccountLifeCycleStatus":"Active"}]}`)
	})
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
// credentials' account context.
func TestListAdAccounts_NeverEchoesTheResponseBody(t *testing.T) {
	const marker = "s3cret-marker"
	c := newCustomerClient(t, AccountConfig{}, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `not json `+marker)
	})
	_, err := c.ListAdAccounts(context.Background())
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
	var calls int
	c := newCustomerClient(t, AccountConfig{}, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"AccountsInfo":[{"Id":1234567,"AccountLifeCycleStatus":"Active"}]}`)
	})
	got, err := c.ListAdAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAdAccounts: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls = %d, want the 429 retried once", calls)
	}
	if len(got) != 1 {
		t.Errorf("got %d accounts, want 1", len(got))
	}
}

// TestDoRequest_StillSendsTheAccountHeader guards the refactor that split doRequest and
// doCustomerRequest: the campaign path must keep its per-account identity. Without this,
// making the header conditional could silently drop it everywhere.
func TestDoRequest_StillSendsTheAccountHeader(t *testing.T) {
	var gotAcct, gotPath string
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAcct = r.Header.Get("CustomerAccountId")
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	if _, err := c.doRequest(context.Background(), http.MethodGet, "Campaigns", nil, true); err != nil {
		t.Fatalf("doRequest: %v", err)
	}
	if gotAcct != "1234567" {
		t.Errorf("CustomerAccountId = %q, want the campaign path still account-scoped", gotAcct)
	}
	if !strings.HasPrefix(gotPath, "/CampaignManagement/") {
		t.Errorf("path = %q, want the campaign service unchanged", gotPath)
	}
}
