// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// newConversionTestClient is a client bound to customer 1234567890 that talks to srv.
func newConversionTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return NewClient(
		Credentials{ClientID: "id", ClientSecret: "secret", DeveloperToken: "token", RefreshToken: "refresh"},
		AccountConfig{CustomerID: "1234567890"},
		WithBaseURL(srv.URL),
		WithTokenURL(srv.URL+"/token"),
		WithAPIVersion("v23"),
	)
}

// The read is one googleAds:search on the client's customer, selecting ENABLED actions only,
// and each row decodes into the fields a picker shows.
func TestListConversionActions_QueryAndParse(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []string
		gotQuery string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		var req searchRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		gotQuery = req.Query
		mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[`+
			`{"conversionAction":{"resourceName":"customers/1234567890/conversionActions/111","id":"111","name":"Event Registrations","status":"ENABLED","type":"WEBPAGE","category":"SIGNUP","primaryForGoal":true}},`+
			`{"conversionAction":{"resourceName":"customers/1234567890/conversionActions/222","id":"222","name":"Newsletter","status":"ENABLED","type":"UPLOAD_CLICKS","category":"LEAD"}}`+
			`]}`)
	}))
	defer srv.Close()

	got, err := newConversionTestClient(t, srv).ListConversionActions(context.Background())
	if err != nil {
		t.Fatalf("ListConversionActions: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 1 || requests[0] != "POST /v23/customers/1234567890/googleAds:search" {
		t.Fatalf("requests = %v, want exactly one googleAds:search on the client's customer", requests)
	}
	if !strings.Contains(gotQuery, "WHERE conversion_action.status = 'ENABLED'") {
		t.Errorf("query %q does not restrict to ENABLED actions", gotQuery)
	}
	for _, f := range []string{"conversion_action.id", "conversion_action.resource_name", "conversion_action.name", "conversion_action.type", "conversion_action.category", "conversion_action.primary_for_goal", "FROM conversion_action", "ORDER BY conversion_action.name"} {
		if !strings.Contains(gotQuery, f) {
			t.Errorf("query %q is missing %s", gotQuery, f)
		}
	}
	want := []ConversionAction{
		{ID: "111", ResourceName: "customers/1234567890/conversionActions/111", Name: "Event Registrations", Status: "ENABLED", Type: "WEBPAGE", Category: "SIGNUP", PrimaryForGoal: true},
		{ID: "222", ResourceName: "customers/1234567890/conversionActions/222", Name: "Newsletter", Status: "ENABLED", Type: "UPLOAD_CLICKS", Category: "LEAD"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d actions, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("action %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// Every returned id and resource name must be one the create's validator accepts.
	for _, a := range got {
		if _, err := validateConversionActions("1234567890", []string{a.ID, a.ResourceName}); err != nil {
			t.Errorf("action %q is refused by validateConversionActions: %v", a.ID, err)
		}
	}
}

// An account with no enabled action is an empty, non-nil list, not an error.
func TestListConversionActions_EmptyIsAnEmptyList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	got, err := newConversionTestClient(t, srv).ListConversionActions(context.Background())
	if err != nil {
		t.Fatalf("ListConversionActions: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("got %#v, want a non-nil empty list", got)
	}
}

// A row without its id or resource name fails the whole read.
func TestListConversionActions_RowMissingItsIDFailsTheRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		_, _ = io.WriteString(w, `{"results":[`+
			`{"conversionAction":{"resourceName":"customers/1234567890/conversionActions/111","id":"111","name":"ok","status":"ENABLED"}},`+
			`{"conversionAction":{"resourceName":"customers/1234567890/conversionActions/222","name":"no id","status":"ENABLED"}}`+
			`]}`)
	}))
	defer srv.Close()
	if got, err := newConversionTestClient(t, srv).ListConversionActions(context.Background()); err == nil {
		t.Fatalf("got %+v, want an error for a row without an id", got)
	}
}

// More rows than maxConversionActionRows fail the read rather than being truncated.
func TestListConversionActions_TooManyRowsFailsTheRead(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"results":[`)
	for i := 0; i <= maxConversionActionRows; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"conversionAction":{"resourceName":"customers/1234567890/conversionActions/%d","id":"%d","status":"ENABLED"}}`, i+1, i+1)
	}
	b.WriteString(`]}`)
	body := b.String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	if _, err := newConversionTestClient(t, srv).ListConversionActions(context.Background()); err == nil {
		t.Fatal("want an error for more than maxConversionActionRows actions")
	}
}
