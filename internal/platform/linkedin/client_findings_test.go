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

// ---------------------------------------------------------------------------
// Second-round Copilot findings
// ---------------------------------------------------------------------------

// TestFindByName_NumericID verifies that a search response returning `id` as a
// JSON number (a long, the real LinkedIn shape) is decoded without failure and
// findByName returns its string form. Before flexibleID this failed
// json.Unmarshal outright.
func TestFindByName_NumericID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// id is an unquoted JSON number, as LinkedIn actually returns it.
		_, _ = io.WriteString(w, `{"elements":[{"name":"Events | Numeric | CNCF","status":"ACTIVE","id":12345}]}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	id, err := c.findByName(context.Background(), "adAccounts/123456789/adCampaignGroups", "Events | Numeric | CNCF")
	if err != nil {
		t.Fatalf("findByName with numeric id: %v", err)
	}
	if id != "12345" {
		t.Errorf("expected numeric id normalized to \"12345\", got %q", id)
	}
}

// TestFindByName_OffsetPaginationNoLinks verifies that a normal offset-paginated
// response that OMITS paging.links still advances past page one: full pages
// (len == pageSize) keep pagination going, and a later-page match is found. This
// is the shape LinkedIn returns in practice.
func TestFindByName_OffsetPaginationNoLinks(t *testing.T) {
	const pageSize = 50
	const matchStart = 3 * pageSize // match on page index 3

	var mu sync.Mutex
	var getCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		getCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")

		start, _ := strconv.Atoi(r.URL.Query().Get("start"))
		if start == matchStart {
			// The match arrives on a full page (still pageSize elements) but with
			// NO paging block at all — the client must have paginated here purely
			// on the "full page" heuristic.
			var sb strings.Builder
			sb.WriteString(`{"elements":[{"name":"Events | Deep | CNCF","status":"ACTIVE","id":"urn:li:sponsoredCampaignGroup:888"}`)
			for i := 1; i < pageSize; i++ {
				sb.WriteString(`,{"name":"Other","status":"ACTIVE","id":1}`)
			}
			sb.WriteString(`]}`)
			_, _ = io.WriteString(w, sb.String())
			return
		}
		// A FULL page of non-matching elements, with NO paging.links whatsoever.
		var sb strings.Builder
		sb.WriteString(`{"elements":[`)
		for i := 0; i < pageSize; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			sb.WriteString(`{"name":"Other","status":"ACTIVE","id":1}`)
		}
		sb.WriteString(`]}`)
		_, _ = io.WriteString(w, sb.String())
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	id, err := c.findByName(context.Background(), "adAccounts/123456789/adCampaigns", "Events | Deep | CNCF")
	if err != nil {
		t.Fatalf("findByName: %v", err)
	}
	if id != "888" {
		t.Errorf("expected later-page match id 888, got %q", id)
	}
	mu.Lock()
	defer mu.Unlock()
	if getCount < 4 {
		t.Errorf("expected pagination past page 1 without paging.links, only made %d GETs", getCount)
	}
}

// TestValidateRegistrationURL covers accept/reject cases for the up-front URL
// validation.
func TestValidateRegistrationURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"https ok", "https://events.example.org/reg", false},
		{"http ok", "http://events.example.org/reg", false},
		{"empty", "", true},
		{"whitespace", "   ", true},
		{"relative", "/reg", true},
		{"scheme-relative", "//events.example.org/reg", true},
		{"ftp scheme", "ftp://events.example.org/reg", true},
		{"no host", "https://", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRegistrationURL(tc.url)
			if tc.wantErr && err == nil {
				t.Errorf("validateRegistrationURL(%q): want error, got nil", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateRegistrationURL(%q): want nil, got %v", tc.url, err)
			}
		})
	}
}

// TestCreateCampaign_RejectsBadRegistrationURLBeforeAnyPOST verifies that an
// empty/relative registration URL is rejected up front — before any POST that
// would create a permanent campaign group or campaign. The test server fails on
// any POST, so a passing test proves no POST was attempted.
func TestCreateCampaign_RejectsBadRegistrationURLBeforeAnyPOST(t *testing.T) {
	var mu sync.Mutex
	var postCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			postCount++
			mu.Unlock()
			t.Errorf("unexpected POST %s — URL should have been rejected first", r.URL.Path)
			http.Error(w, "should not POST", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"elements":[]}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))

	for _, bad := range []string{"", "/relative/only"} {
		_, err := c.CreateCampaign(context.Background(), CampaignInput{
			EventName:        "E",
			RegistrationURL:  bad,
			BudgetUSD:        100,
			StartDate:        "2099-01-01",
			EndDate:          "2099-02-01",
			TargetingProfile: "cloud-native",
			Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
		})
		if err == nil {
			t.Errorf("CreateCampaign with registration URL %q: expected error, got nil", bad)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if postCount != 0 {
		t.Errorf("expected zero POSTs for invalid registration URL, got %d", postCount)
	}
}

// TestCreateCampaign_PartialVariantFailureReported verifies that when a later
// variant fails after an earlier one succeeded, CreateCampaign returns an error
// that reports the partial success AND a non-nil *CampaignResult carrying the
// group/campaign IDs and the successful creative count, so the caller does not
// blindly retry.
func TestCreateCampaign_PartialVariantFailureReported(t *testing.T) {
	var mu sync.Mutex
	var darkPostCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[]}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaign:200"}`)
		case strings.Contains(r.URL.Path, "posts"):
			darkPostCount++
			// First dark post succeeds; the second one fails.
			if darkPostCount >= 2 {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	res, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "KubeCon",
		RegistrationURL:  "https://events.example.org/kubecon",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		Variants: []CreativeVariant{
			{IntroText: "a", Headline: "one"},
			{IntroText: "b", Headline: "two"}, // this variant's dark post fails
		},
	})
	if err == nil {
		t.Fatal("expected an error on mid-variant failure, got nil")
	}
	if !strings.Contains(err.Error(), "1 of 2") {
		t.Errorf("error should report 1 of 2 variants created, got: %v", err)
	}
	if !strings.Contains(err.Error(), "do NOT blindly retry") {
		t.Errorf("error should warn against blind retry, got: %v", err)
	}
	if res == nil {
		t.Fatal("expected a non-nil partial CampaignResult, got nil")
	}
	if res.CampaignGroupID != "100" || res.CampaignID != "200" {
		t.Errorf("partial result should carry created group/campaign IDs, got group=%q campaign=%q", res.CampaignGroupID, res.CampaignID)
	}
	if res.CreativeCount != 1 {
		t.Errorf("partial result should report 1 successful creative, got %d", res.CreativeCount)
	}
}

// TestCreateCampaign_UnknownAccountFailsClosed verifies that an unknown account
// id supplied through the sole public entry point (CreateCampaign) fails closed
// before any POST — the account-scoped hierarchy helpers are unexported so a
// caller cannot bypass the resolveAccountID cross-tenant check.
func TestCreateCampaign_UnknownAccountFailsClosed(t *testing.T) {
	var mu sync.Mutex
	var postCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			postCount++
			mu.Unlock()
			t.Errorf("unexpected POST — unknown account should fail closed first")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"elements":[]}`)
	}))
	defer srv.Close()

	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	_, err := c.CreateCampaign(context.Background(), CampaignInput{
		EventName:        "E",
		AdAccountID:      "000000", // not in the runtime config
		RegistrationURL:  "https://events.example.org/reg",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		TargetingProfile: "cloud-native",
		Variants:         []CreativeVariant{{IntroText: "a", Headline: "b"}},
	})
	if err == nil {
		t.Fatal("expected fail-closed error for unknown account id")
	}
	mu.Lock()
	defer mu.Unlock()
	if postCount != 0 {
		t.Errorf("no POST should be issued for an unknown account, got %d", postCount)
	}
}
