// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// reachServer answers the three endpoints ProbeAccountReach can touch: the token endpoint,
// customers:listAccessibleCustomers (flat mode's first leg), and googleAds:search (the
// customer_client read, used by both modes). searchRows is the payload the search returns;
// searchPath records which customer the search ran under, because in flat mode running it
// under the wrong customer would be a silent behaviour change.
type reachServer struct {
	accessible []string
	searchRows []map[string]any
	searchCode int

	searchPath  string
	searchQuery string
	searches    int
}

func (rs *reachServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/customers:listAccessibleCustomers"):
			_ = json.NewEncoder(w).Encode(listAccessibleCustomersResponse{ResourceNames: rs.accessible})
		case strings.HasSuffix(r.URL.Path, "/googleAds:search"):
			rs.searches++
			rs.searchPath = r.URL.Path
			var body searchRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			rs.searchQuery = body.Query
			if rs.searchCode != 0 {
				w.WriteHeader(rs.searchCode)
				_, _ = w.Write([]byte(`{"error":{"code":500,"message":"boom"}}`))
				return
			}
			rows := rs.searchRows
			if rows == nil {
				rows = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": rows})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func clientRow(id string, manager bool, status string) map[string]any {
	return map[string]any{"customerClient": map[string]any{
		"id": id, "descriptiveName": "acct", "manager": manager, "status": status,
	}}
}

func reachClient(t *testing.T, srv *httptest.Server, loginCustomerID string) *Client {
	t.Helper()
	return NewClient(
		Credentials{ClientID: "id", ClientSecret: "secret", DeveloperToken: "token", RefreshToken: "refresh"},
		AccountConfig{CustomerID: "1234567890", LoginCustomerID: loginCustomerID, Label: "Test"},
		WithBaseURL(srv.URL),
		WithTokenURL(srv.URL+"/token"),
		WithAPIVersion("v23"),
		WithClock(func() time.Time { return time.Unix(0, 0) }),
	)
}

// TestProbeAccountReach_FlatMode pins the second leg flat mode did not used to have.
//
// customers:listAccessibleCustomers is UNFILTERED and carries neither the manager flag nor the
// status, so membership alone answered AccountReachable for an account a campaign can never run
// in. A manager account configured as account_id therefore tested green and failed at the first
// create — the production failure this endpoint exists to catch, recreated by the endpoint
// meant to catch it. Presence is now followed by the account's own customer_client row, which
// is the same pair manager mode reads from the hierarchy walk.
func TestProbeAccountReach_FlatMode(t *testing.T) {
	const configured = "1234567890"

	cases := []struct {
		name string
		rows []map[string]any
		want AccountReach
	}{
		{"an enabled non-manager is reachable", []map[string]any{clientRow(configured, false, "ENABLED")}, AccountReachable},
		{"a manager account is not somewhere a campaign can run", []map[string]any{clientRow(configured, true, "ENABLED")}, AccountIsManager},
		{"a suspended account is reached but not enabled", []map[string]any{clientRow(configured, false, "SUSPENDED")}, AccountNotEnabled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := &reachServer{accessible: []string{"customers/" + configured}, searchRows: tc.rows}
			srv := rs.start(t)
			got, err := reachClient(t, srv, "").ProbeAccountReach(context.Background(), configured)
			if err != nil {
				t.Fatalf("ProbeAccountReach: %v", err)
			}
			if got != tc.want {
				t.Errorf("reach = %v, want %v", got, tc.want)
			}
			if rs.searches != 1 {
				t.Errorf("%d searches; flat mode must read the account's own row exactly once", rs.searches)
			}
			// Under the CONFIGURED customer, not under some manager — flat mode has none.
			if !strings.Contains(rs.searchPath, "/customers/"+configured+"/googleAds:search") {
				t.Errorf("search ran at %q, want it scoped to the configured customer", rs.searchPath)
			}
			// Asking for the one row by id, not reading the table: under a manager account that
			// table is the whole hierarchy, and being a manager is the case this call detects.
			if !strings.Contains(rs.searchQuery, "customer_client.id = "+configured) {
				t.Errorf("query = %q, want it narrowed to the configured id", rs.searchQuery)
			}
		})
	}

	t.Run("an account absent from the enumeration is unreachable, and costs no second call", func(t *testing.T) {
		rs := &reachServer{accessible: []string{"customers/9999999999"}}
		srv := rs.start(t)
		got, err := reachClient(t, srv, "").ProbeAccountReach(context.Background(), configured)
		if err != nil {
			t.Fatalf("ProbeAccountReach: %v", err)
		}
		if got != AccountUnreachable {
			t.Errorf("reach = %v, want AccountUnreachable", got)
		}
		if rs.searches != 0 {
			t.Error("the properties read ran for an account the credential does not reach")
		}
	})

	// The honest answer for "reached, properties unknown". AccountReachable would be a success
	// nothing established, and AccountUnreachable a confirmed verdict contradicting the
	// enumeration that just named the account — so this leaves as an error, which
	// ProbeInconclusive classifies as inconclusive by its default for an unrecognised error.
	t.Run("a missing self row is inconclusive, not a verdict", func(t *testing.T) {
		rs := &reachServer{accessible: []string{"customers/" + configured}, searchRows: []map[string]any{}}
		srv := rs.start(t)
		got, err := reachClient(t, srv, "").ProbeAccountReach(context.Background(), configured)
		if err == nil {
			t.Fatalf("reach = %v with no error; an unestablished property must not read as a verdict", got)
		}
		if got != AccountUnreachable {
			t.Errorf("reach = %v alongside an error; the zero value is the only safe pairing", got)
		}
		if !ProbeInconclusive(err) {
			t.Errorf("err = %v is not inconclusive; it would become a confirmed answer about the connection", err)
		}
		if ProbeCredentialRejected(err) {
			t.Errorf("err = %v reads as a credential refusal; nothing here evaluated the credential", err)
		}
	})

	t.Run("a 5xx on the properties read stays inconclusive", func(t *testing.T) {
		rs := &reachServer{accessible: []string{"customers/" + configured}, searchCode: http.StatusInternalServerError}
		srv := rs.start(t)
		_, err := reachClient(t, srv, "").ProbeAccountReach(context.Background(), configured)
		if err == nil {
			t.Fatal("a 5xx on the second leg must not be swallowed into a verdict")
		}
		if !ProbeInconclusive(err) {
			t.Errorf("err = %v is not inconclusive", err)
		}
	})
}

// TestProbeAccountReach_ManagerMode is the regression guard on the mode flat mode now matches:
// the walk is unfiltered, so a manager or non-enabled account is reported as what it IS rather
// than vanishing into an absence and being reported as unreachable.
func TestProbeAccountReach_ManagerMode(t *testing.T) {
	const configured = "1234567890"
	const manager = "5555555555"

	cases := []struct {
		name string
		rows []map[string]any
		want AccountReach
	}{
		{"enabled non-manager", []map[string]any{clientRow(configured, false, "ENABLED")}, AccountReachable},
		{"manager", []map[string]any{clientRow(configured, true, "ENABLED")}, AccountIsManager},
		{"cancelled", []map[string]any{clientRow(configured, false, "CANCELED")}, AccountNotEnabled},
		{"absent from the hierarchy", []map[string]any{clientRow("9999999999", false, "ENABLED")}, AccountUnreachable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := &reachServer{searchRows: tc.rows}
			srv := rs.start(t)
			got, err := reachClient(t, srv, manager).ProbeAccountReach(context.Background(), configured)
			if err != nil {
				t.Fatalf("ProbeAccountReach: %v", err)
			}
			if got != tc.want {
				t.Errorf("reach = %v, want %v", got, tc.want)
			}
			if !strings.Contains(rs.searchPath, "/customers/"+manager+"/googleAds:search") {
				t.Errorf("search ran at %q, want it scoped to the manager", rs.searchPath)
			}
		})
	}
}
