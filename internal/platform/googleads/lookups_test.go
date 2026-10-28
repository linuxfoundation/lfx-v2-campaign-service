// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The suggest call is account-agnostic: it goes to geoTargetConstants:suggest with no customer
// segment, carries the query and country in Google's body shape, and works on a client with
// no CustomerID.
func TestSuggestGeoTargetConstants_RequestAndParse(t *testing.T) {
	var (
		mu      sync.Mutex
		gotPath string
		gotBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath = r.URL.Path
		_ = json.Unmarshal(b, &gotBody)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"geoTargetConstantSuggestions":[`+
			`{"geoTargetConstant":{"resourceName":"geoTargetConstants/1026201","id":"1026201","name":"Austin","countryCode":"US","targetType":"City","status":"ENABLED","canonicalName":"Austin,Texas,United States"},"reach":"2100000"},`+
			`{"geoTargetConstant":{"resourceName":"geoTargetConstants/1026201","id":"1026201","name":"Austin","countryCode":"US","targetType":"City","status":"ENABLED"}},`+
			`{"geoTargetConstant":{"resourceName":"geoTargetConstants/21176","name":"Texas","countryCode":"US","targetType":"State","status":"ENABLED"}},`+
			`{"geoTargetConstant":{"resourceName":"geoTargetConstants/007","id":"007","name":"bad"}},`+
			`{"reach":"5"}`+
			`]}`)
	}))
	defer srv.Close()
	c := newManagerTestClient(t, srv)

	rows, err := c.SuggestGeoTargetConstants(context.Background(), " Austin ", "us")
	if err != nil {
		t.Fatalf("SuggestGeoTargetConstants: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/v23/geoTargetConstants:suggest" {
		t.Errorf("path = %q", gotPath)
	}
	if gotBody["countryCode"] != "US" || gotBody["locale"] != "en" {
		t.Errorf("body = %v", gotBody)
	}
	names, _ := gotBody["locationNames"].(map[string]any)["names"].([]any)
	if len(names) != 1 || names[0] != "Austin" {
		t.Errorf("locationNames = %v", gotBody["locationNames"])
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want Austin once (deduped) and Texas (id recovered from the resource name)", rows)
	}
	if rows[0].ID != "1026201" || rows[0].CanonicalName != "Austin,Texas,United States" || rows[0].Reach == nil || *rows[0].Reach != 2100000 {
		t.Errorf("austin = %+v", rows[0])
	}
	if rows[1].ID != "21176" || rows[1].Reach != nil {
		t.Errorf("texas = %+v", rows[1])
	}
	// Every returned id must be one googleAdsConfig.geoTargets accepts.
	for _, r := range rows {
		if _, err := validateGeoTargets([]string{r.ID}); err != nil {
			t.Errorf("suggested id %q is refused by the geoTargets validator: %v", r.ID, err)
		}
	}
}

func TestSuggestGeoTargetConstants_EmptyQueryIsRefusedLocally(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request may be sent for an empty query")
	}))
	defer srv.Close()
	if _, err := newManagerTestClient(t, srv).SuggestGeoTargetConstants(context.Background(), "  ", ""); err == nil {
		t.Fatal("want an error for an empty query")
	}
}

func TestListUserLists_QueryAndParse(t *testing.T) {
	var (
		mu       sync.Mutex
		gotPath  string
		gotQuery string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAccountsToken(w, r) {
			return
		}
		var req searchRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		gotPath, gotQuery = r.URL.Path, req.Query
		mu.Unlock()
		_, _ = io.WriteString(w, `{"results":[`+
			`{"userList":{"resourceName":"customers/1234567890/userLists/2","name":"zeta","type":"LOGICAL","membershipStatus":"OPEN","accountUserListStatus":"ENABLED","sizeRangeForSearch":"LESS_THAN_FIVE_HUNDRED"}},`+
			`{"userList":{"resourceName":"customers/1234567890/userLists/1","name":"Alpha","type":"CRM_BASED","membershipStatus":"OPEN","sizeForSearch":"1500","eligibleForSearch":true}},`+
			`{"userList":{"resourceName":"customers/1234567890/customAudiences/9","name":"not a list"}}`+
			`]}`)
	}))
	defer srv.Close()

	lists, err := newManagerTestClient(t, srv).ListUserLists(context.Background(), "1234567890")
	if err != nil {
		t.Fatalf("ListUserLists: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/v23/customers/1234567890/googleAds:search" {
		t.Errorf("path = %q", gotPath)
	}
	for _, f := range []string{"user_list.resource_name", "user_list.size_for_search", "user_list.eligible_for_search", "user_list.membership_status", "FROM user_list"} {
		if !strings.Contains(gotQuery, f) {
			t.Errorf("query %q is missing %s", gotQuery, f)
		}
	}
	if len(lists) != 2 || lists[0].Name != "Alpha" || lists[1].Name != "zeta" {
		t.Fatalf("lists = %+v", lists)
	}
	if lists[0].ID != "1" || !lists[0].EligibleForSearch || lists[0].SizeForSearch == nil || *lists[0].SizeForSearch != 1500 {
		t.Errorf("alpha = %+v", lists[0])
	}
	if lists[1].AccountStatus != "ENABLED" || lists[1].SizeRangeForSearch != "LESS_THAN_FIVE_HUNDRED" {
		t.Errorf("zeta = %+v", lists[1])
	}
	// Every returned resource name must be one audienceSegments accepts.
	for _, l := range lists {
		if _, err := validateAudienceSegments([]string{l.ResourceName}); err != nil {
			t.Errorf("resource name %q is refused by the audienceSegments validator: %v", l.ResourceName, err)
		}
	}
}

func TestListUserLists_RefusesAMalformedCustomerID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no request may be sent for a malformed customer id")
	}))
	defer srv.Close()
	if _, err := newManagerTestClient(t, srv).ListUserLists(context.Background(), "123-456"); err == nil {
		t.Fatal("want an error for a dashed customer id")
	}
}

func TestNormalizeGeoTargetType(t *testing.T) {
	for _, in := range []string{"Postal Code", "postal_code", "POSTAL-CODE", "PostalCode"} {
		if got := NormalizeGeoTargetType(in); got != "postalcode" {
			t.Errorf("NormalizeGeoTargetType(%q) = %q", in, got)
		}
	}
}
