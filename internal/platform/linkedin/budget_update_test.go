// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// budgetClient builds a client bound to a test account, which is what both budget calls
// address the campaign under.
func budgetClient(url string) *Client {
	return NewClient(
		Credentials{AccessToken: "test-token"},
		RuntimeConfig{DefaultAccountID: "509430019"},
		WithBaseURL(url),
	)
}

// TestValidateBudgetAmount_RulesAreCheckedAgainstTheWireValue pins the contract the create path
// and the write path now SHARE. Each case names the rule it stands for, because the point of
// the shared validator is that an amount refused on create is refused identically on edit.
func TestValidateBudgetAmount_RulesAreCheckedAgainstTheWireValue(t *testing.T) {
	cases := []struct {
		name     string
		amount   float64
		lifetime bool
		wantWire string
		wantErr  string
	}{
		{name: "daily at the minimum", amount: 10, wantWire: "10.00"},
		{name: "lifetime at the minimum", amount: 100, lifetime: true, wantWire: "100.00"},
		{name: "extra precision is truncated to the wire precision", amount: 10.005, wantWire: "10.01"},
		{
			// The rule that makes "validate the rounded value" load-bearing: 9.999
			// is SENT as "10.00" and therefore MEETS the $10 minimum. Checking the
			// raw float would refuse an amount the platform accepts.
			name: "just under the daily minimum but rounds up to it", amount: 9.999, wantWire: "10.00",
		},
		{name: "below the daily minimum", amount: 9.99, wantErr: "below LinkedIn's minimum"},
		{name: "below the lifetime minimum", amount: 99.99, lifetime: true, wantErr: "below LinkedIn's minimum"},
		{name: "sub-cent rounds to zero at the boundary", amount: 0.001, wantErr: "would round to zero at the API boundary"},
		{name: "zero", amount: 0, wantErr: "greater than zero"},
		{name: "negative", amount: -50, wantErr: "greater than zero"},
		{name: "NaN", amount: math.NaN(), wantErr: "finite number"},
		{name: "positive infinity", amount: math.Inf(1), wantErr: "finite number"},
		{name: "negative infinity", amount: math.Inf(-1), wantErr: "finite number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire, rounded, err := ValidateBudgetAmount(tc.amount, tc.lifetime)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got wire %q", tc.wantErr, wire)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not mention %q", err, tc.wantErr)
				}
				if wire != "" {
					t.Errorf("a refused amount must not yield a wire string, got %q", wire)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if wire != tc.wantWire {
				t.Errorf("wire = %q, want %q", wire, tc.wantWire)
			}
			// The rounded value must be the wire string's value, not the input:
			// every minimum above is checked against it.
			if got := formatWire(rounded); got != tc.wantWire {
				t.Errorf("rounded value %v does not round-trip to the wire string %q (got %q)", rounded, tc.wantWire, got)
			}
		})
	}
}

func formatWire(v float64) string {
	w, _, err := ValidateBudgetAmount(v, false)
	if err != nil {
		return "unvalidatable"
	}
	return w
}

func TestGetCampaignBudget_ReadsEachFieldAndTheCurrency(t *testing.T) {
	// Captured on the SERVER's goroutine and read on the test's, so the handoff needs a
	// happens-before edge: `make test` runs with -race, and this value is exactly what the
	// assertions below read.
	var mu sync.Mutex
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		capturedPath = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":123456,"status":"ACTIVE",
			"dailyBudget":{"amount":"250.00","currencyCode":"USD"}}`)
	}))
	defer srv.Close()

	got, err := budgetClient(srv.URL).GetCampaignBudget(context.Background(), "123456")
	if err != nil {
		t.Fatalf("GetCampaignBudget: %v", err)
	}
	mu.Lock()
	gotPath := capturedPath
	mu.Unlock()
	// The campaign is addressed UNDER the resolved account, which is what keeps the read
	// inside the account the project's connection resolves to.
	if !strings.Contains(gotPath, "adAccounts/509430019/adCampaigns/123456") {
		t.Errorf("campaign was not addressed under the account: %q", gotPath)
	}
	if got.DailyBudget == nil || *got.DailyBudget != 250 {
		t.Errorf("dailyBudget = %v, want 250", got.DailyBudget)
	}
	// An ABSENT field must stay nil, never zero: nil is what the pacing guard reads as
	// "this campaign does not use this field".
	if got.TotalBudget != nil {
		t.Errorf("absent totalBudget must be nil, got %v", *got.TotalBudget)
	}
	if got.CurrencyCode != "USD" || got.Status != "ACTIVE" {
		t.Errorf("currency/status not carried: %+v", got)
	}
	if got.AmountUnparseable {
		t.Error("a well-formed amount must not set AmountUnparseable")
	}
}

// TestGetCampaignBudget_UnparseableAmountIsFlaggedNotSwallowed pins the difference between "no
// budget" and "a budget this client could not read". They are opposite facts, and only one of
// them is safe to write against.
func TestGetCampaignBudget_UnparseableAmountIsFlaggedNotSwallowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":123456,"dailyBudget":{"amount":"not-a-number","currencyCode":"USD"}}`)
	}))
	defer srv.Close()

	got, err := budgetClient(srv.URL).GetCampaignBudget(context.Background(), "123456")
	if err != nil {
		t.Fatalf("GetCampaignBudget: %v", err)
	}
	if !got.AmountUnparseable {
		t.Fatal("an unreadable amount must set AmountUnparseable")
	}
	if got.DailyBudget != nil {
		t.Error("an unreadable amount must not also produce a value")
	}
}

// TestGetCampaignBudget_RefusesAResponseAboutAnotherCampaign: a budget read that answered about
// a different id describes a campaign the caller never named.
func TestGetCampaignBudget_RefusesAResponseAboutAnotherCampaign(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":999999,"dailyBudget":{"amount":"250.00","currencyCode":"USD"}}`)
	}))
	defer srv.Close()

	_, err := budgetClient(srv.URL).GetCampaignBudget(context.Background(), "123456")
	if err == nil || !strings.Contains(err.Error(), "returned campaign 999999") {
		t.Fatalf("want a refusal naming the other campaign, got %v", err)
	}
}

func TestGetCampaignBudget_RefusesANonNumericCampaignID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a malformed id must be refused before any HTTP call")
	}))
	defer srv.Close()

	if _, err := budgetClient(srv.URL).GetCampaignBudget(context.Background(), "../adAccounts/1"); err == nil {
		t.Fatal("want a refusal for a non-numeric campaign id")
	}
}

func TestUpdateCampaignBudget_WritesTheFieldThePacingSelects(t *testing.T) {
	cases := []struct {
		name      string
		lifetime  bool
		wantField string
		otherFiel string
	}{
		{name: "daily", lifetime: false, wantField: "dailyBudget", otherFiel: "totalBudget"},
		{name: "lifetime", lifetime: true, wantField: "totalBudget", otherFiel: "dailyBudget"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Captured on the server's goroutine, read on the test's: the mutex is the
			// happens-before edge -race requires, and these values ARE the assertions.
			var mu sync.Mutex
			var capturedBody map[string]any
			var capturedMethod string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				capturedMethod = r.Header.Get("X-Restli-Method")
				_ = json.NewDecoder(r.Body).Decode(&capturedBody)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()

			if err := budgetClient(srv.URL).UpdateCampaignBudget(context.Background(), "123456", "250.00", tc.lifetime); err != nil {
				t.Fatalf("UpdateCampaignBudget: %v", err)
			}
			mu.Lock()
			body, method := capturedBody, capturedMethod
			mu.Unlock()
			if method != "PARTIAL_UPDATE" {
				t.Errorf("X-Restli-Method = %q, want PARTIAL_UPDATE", method)
			}
			patch, _ := body["patch"].(map[string]any)
			set, _ := patch["$set"].(map[string]any)
			field, ok := set[tc.wantField].(map[string]any)
			if !ok {
				t.Fatalf("body did not set %s: %v", tc.wantField, body)
			}
			// The AMOUNT SENT is the caller's wire string verbatim: nothing between
			// the validator and the wire may re-format it.
			if field["amount"] != "250.00" {
				t.Errorf("amount = %v, want the wire string \"250.00\"", field["amount"])
			}
			if field["currencyCode"] != "USD" {
				t.Errorf("currencyCode = %v, want USD", field["currencyCode"])
			}
			// The OTHER field must be left entirely alone — writing both would leave
			// the campaign with two budgets in force.
			if _, present := set[tc.otherFiel]; present {
				t.Errorf("%s must not be touched by a %s write", tc.otherFiel, tc.wantField)
			}
		})
	}
}

// TestUpdateCampaignBudget_RefusesAnAmountThatDidNotComeFromTheValidator is the construction
// guard: it catches a caller that formatted its own string and bypassed every minimum.
func TestUpdateCampaignBudget_RefusesAnAmountThatDidNotComeFromTheValidator(t *testing.T) {
	for _, amount := range []string{"", "  ", "one hundred", "100.00 USD"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Errorf("amount %q must be refused before any HTTP call", amount)
		}))
		err := budgetClient(srv.URL).UpdateCampaignBudget(context.Background(), "123456", amount, false)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "ValidateBudgetAmount") {
			t.Errorf("amount %q: want a refusal naming the validator, got %v", amount, err)
		}
	}
}

// TestUpdateCampaignBudget_A5xxIsUnconfirmedA4xxIsDefinite is the split the dispatcher's
// classification depends on. A 5xx on a money-moving write may have been applied; a 4xx was
// not, and reporting it as "verify upstream" would send an operator after nothing.
func TestUpdateCampaignBudget_A5xxIsUnconfirmedA4xxIsDefinite(t *testing.T) {
	cases := []struct {
		name            string
		status          int
		wantUnconfirmed bool
	}{
		{name: "server error", status: http.StatusBadGateway, wantUnconfirmed: true},
		{name: "bad request", status: http.StatusBadRequest, wantUnconfirmed: false},
		{name: "forbidden", status: http.StatusForbidden, wantUnconfirmed: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"message":"nope"}`)
			}))
			defer srv.Close()

			err := budgetClient(srv.URL).UpdateCampaignBudget(context.Background(), "123456", "250.00", false)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := IsOutcomeUnconfirmed(err); got != tc.wantUnconfirmed {
				t.Errorf("IsOutcomeUnconfirmed = %v, want %v (err: %v)", got, tc.wantUnconfirmed, err)
			}
		})
	}
}
