// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func budgetClient(url string, cfg AccountConfig) *Client {
	return NewClient(Credentials{AccessToken: "test-token"}, cfg, WithBaseURL(url))
}

// TestBudgetToMinorUnits pins the encoder both the create and the write path now share. The
// offset is NOT an FX rate — it is the currency's minor-unit scale — so the arithmetic must be
// exact at the boundaries rather than merely plausible.
func TestBudgetToMinorUnits(t *testing.T) {
	cases := []struct {
		name    string
		budget  float64
		offset  int64
		want    int64
		wantErr string
	}{
		{name: "usd", budget: 25.50, offset: 100, want: 2550},
		{name: "usd rounds to the nearest cent", budget: 25.505, offset: 100, want: 2551},
		{name: "zero-decimal currency", budget: 3000, offset: 1, want: 3000},
		{name: "one minor unit is the floor", budget: 0.01, offset: 100, want: 1},
		{name: "below one minor unit", budget: 0.004, offset: 100, wantErr: "budget too small"},
		{name: "zero", budget: 0, offset: 100, wantErr: "budget too small"},
		{name: "negative", budget: -5, offset: 100, wantErr: "budget too small"},
		// The only budget-magnitude ceiling there is: converting an out-of-range
		// float to int64 is implementation-defined, so the product is range-checked
		// as a float BEFORE the conversion.
		{name: "overflows int64 after scaling", budget: math.MaxFloat64, offset: 100, wantErr: "budget too large"},
		{name: "positive infinity", budget: math.Inf(1), offset: 100, wantErr: "budget too large"},
		// NaN fails every ordered comparison, so it would slip past the range check
		// and convert to an implementation-defined int64 unless named explicitly.
		{name: "NaN", budget: math.NaN(), offset: 100, wantErr: "finite number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := budgetToMinorUnits(tc.budget, tc.offset)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v (value %d)", tc.wantErr, err, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %d minor units, want %d", got, tc.want)
			}
		})
	}
}

// TestResolveBudgetMinorUnits_TheAccountCurrencyIsAuthoritative pins the precedence rule the
// create path established: a stale explicit offset must never be allowed to mis-scale money.
func TestResolveBudgetMinorUnits_TheAccountCurrencyIsAuthoritative(t *testing.T) {
	cases := []struct {
		name     string
		currency string
		cfg      AccountConfig
		budget   float64
		want     int64
		wantErr  string
	}{
		{
			name: "derived from a recognized currency", currency: "USD",
			cfg: AccountConfig{AccountID: "act_1"}, budget: 25.50, want: 2550,
		},
		{
			// A zero-decimal currency: assuming 100 here would encode the budget
			// 100x too high.
			name: "derived for a zero-decimal currency", currency: "JPY",
			cfg: AccountConfig{AccountID: "act_1"}, budget: 3000, want: 3000,
		},
		{
			name: "an explicit offset agreeing with the currency is accepted", currency: "USD",
			cfg: AccountConfig{AccountID: "act_1", CurrencyOffset: 100}, budget: 25.50, want: 2550,
		},
		{
			// The stale-override case: CurrencyOffset:100 on an account now
			// denominated in JPY.
			name: "an explicit offset conflicting with the currency is REFUSED", currency: "JPY",
			cfg: AccountConfig{AccountID: "act_1", CurrencyOffset: 100}, budget: 3000,
			wantErr: "conflicts with the account's currency",
		},
		{
			name: "an unknown currency with no explicit offset is REFUSED", currency: "XYZ",
			cfg: AccountConfig{AccountID: "act_1"}, budget: 25.50,
			wantErr: "unsupported or missing currency code",
		},
		{
			name: "an unknown currency falls back to an explicit offset", currency: "XYZ",
			cfg: AccountConfig{AccountID: "act_1", CurrencyOffset: 100}, budget: 25.50, want: 2550,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.Contains(r.URL.RawQuery, "currency") {
					t.Errorf("preflight must request the currency, got %q", r.URL.RawQuery)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"name":"LF","account_status":1,"currency":"`+tc.currency+`"}`)
			}))
			defer srv.Close()

			got, _, err := budgetClient(srv.URL, tc.cfg).ResolveBudgetMinorUnits(context.Background(), tc.budget)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %d minor units, want %d", got, tc.want)
			}
		})
	}
}

// TestResolveBudgetMinorUnits_RefusesWithNoAccountSelected: with no account there is no
// currency, and with no currency there is no scale that could be assumed safely.
func TestResolveBudgetMinorUnits_RefusesWithNoAccountSelected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("no account means no request should be made at all")
	}))
	defer srv.Close()

	_, _, err := budgetClient(srv.URL, AccountConfig{}).ResolveBudgetMinorUnits(context.Background(), 25)
	if err == nil || !strings.Contains(err.Error(), "ad account must be selected") {
		t.Fatalf("want a refusal naming the missing account, got %v", err)
	}
}

// TestResolveBudgetMinorUnits_DoesNotGateOnAccountStatus. Lowering the budget of an ad account
// under review is the one action that reduces exposure; refusing it would block exactly that.
func TestResolveBudgetMinorUnits_DoesNotGateOnAccountStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"LF","account_status":2,"currency":"USD"}`)
	}))
	defer srv.Close()

	got, _, err := budgetClient(srv.URL, AccountConfig{AccountID: "act_1"}).ResolveBudgetMinorUnits(context.Background(), 25)
	if err != nil {
		t.Fatalf("a disabled account must still resolve a scale: %v", err)
	}
	if got != 2500 {
		t.Errorf("got %d, want 2500", got)
	}
}

const adSetBudgetPreamble = `{"id":"777","status":"PAUSED","campaign_id":"555"`

func adSetBudgetServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "campaign") {
			t.Errorf("the campaign's budget must be fetched in the SAME request: %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGetAdSetBudget_ReadsBothLevels(t *testing.T) {
	srv := adSetBudgetServer(t, adSetBudgetPreamble+`,"daily_budget":"2550"}`)

	got, err := budgetClient(srv.URL, AccountConfig{AccountID: "act_1"}).GetAdSetBudget(context.Background(), "777")
	if err != nil {
		t.Fatalf("GetAdSetBudget: %v", err)
	}
	if got.DailyBudgetMinor == nil || *got.DailyBudgetMinor != 2550 {
		t.Errorf("daily_budget = %v, want 2550", got.DailyBudgetMinor)
	}
	// An ABSENT field stays nil, never zero: nil is what the pacing guard reads as
	// "this ad set does not use this field".
	if got.LifetimeBudgetMinor != nil {
		t.Errorf("absent lifetime_budget must be nil, got %v", *got.LifetimeBudgetMinor)
	}
	if got.CampaignBudgetOptimized() {
		t.Error("an ad set with its own budget is not CBO")
	}
	if got.CampaignID != "555" || got.Status != "PAUSED" {
		t.Errorf("campaign id / status not carried: %+v", got)
	}
}

// TestGetAdSetBudget_ReportsCampaignBudgetOptimization is the read half of Meta's
// shared-budget guard: the amount lives on the campaign and the ad set's own fields are absent.
func TestGetAdSetBudget_ReportsCampaignBudgetOptimization(t *testing.T) {
	for _, field := range []string{"daily_budget", "lifetime_budget"} {
		t.Run(field, func(t *testing.T) {
			srv := adSetBudgetServer(t, adSetBudgetPreamble+`,"campaign":{"id":"555","`+field+`":"100000"}}`)

			got, err := budgetClient(srv.URL, AccountConfig{AccountID: "act_1"}).GetAdSetBudget(context.Background(), "777")
			if err != nil {
				t.Fatalf("GetAdSetBudget: %v", err)
			}
			if !got.CampaignBudgetOptimized() {
				t.Fatal("a campaign-level budget must be reported as CBO")
			}
			if got.DailyBudgetMinor != nil || got.LifetimeBudgetMinor != nil {
				t.Error("a CBO ad set holds no budget of its own")
			}
		})
	}
}

// TestGetAdSetBudget_UnparseableAmountIsFlaggedNotSwallowed: "no budget here" and "a budget
// here that could not be read" are opposite facts, and only one is safe to write against. The
// campaign level matters most — an unreadable CBO amount read as absent would defeat the guard.
func TestGetAdSetBudget_UnparseableAmountIsFlaggedNotSwallowed(t *testing.T) {
	cases := map[string]string{
		"ad set level":   adSetBudgetPreamble + `,"daily_budget":"lots"}`,
		"campaign level": adSetBudgetPreamble + `,"campaign":{"id":"555","daily_budget":"lots"}}`,
		"fractional":     adSetBudgetPreamble + `,"daily_budget":"25.50"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := adSetBudgetServer(t, body)

			got, err := budgetClient(srv.URL, AccountConfig{AccountID: "act_1"}).GetAdSetBudget(context.Background(), "777")
			if err != nil {
				t.Fatalf("GetAdSetBudget: %v", err)
			}
			if !got.AmountUnparseable {
				t.Fatal("an unreadable amount must set AmountUnparseable")
			}
			if got.DailyBudgetMinor != nil || got.CampaignDailyBudgetMinor != nil {
				t.Error("an unreadable amount must not also produce a value")
			}
		})
	}
}

func TestGetAdSetBudget_RefusesAResponseAboutAnotherAdSet(t *testing.T) {
	srv := adSetBudgetServer(t, `{"id":"888","campaign_id":"555","daily_budget":"2550"}`)

	_, err := budgetClient(srv.URL, AccountConfig{AccountID: "act_1"}).GetAdSetBudget(context.Background(), "777")
	if err == nil || !strings.Contains(err.Error(), "returned ad set 888") {
		t.Fatalf("want a refusal naming the other ad set, got %v", err)
	}
}

func TestGetAdSetBudget_RefusesANonNumericAdSetID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a malformed id must be refused before any HTTP call")
	}))
	defer srv.Close()

	if _, err := budgetClient(srv.URL, AccountConfig{AccountID: "act_1"}).GetAdSetBudget(context.Background(), "777?fields=x"); err == nil {
		t.Fatal("want a refusal for a non-numeric ad set id")
	}
}

func TestUpdateAdSetBudget_WritesTheFieldThePacingSelects(t *testing.T) {
	cases := []struct {
		name      string
		lifetime  bool
		wantField string
		otherFiel string
	}{
		{name: "daily", lifetime: false, wantField: "daily_budget", otherFiel: "lifetime_budget"},
		{name: "lifetime", lifetime: true, wantField: "lifetime_budget", otherFiel: "daily_budget"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			var path string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path = r.URL.Path
				_ = json.NewDecoder(r.Body).Decode(&body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"success":true}`)
			}))
			defer srv.Close()

			if err := budgetClient(srv.URL, AccountConfig{AccountID: "act_1"}).UpdateAdSetBudget(context.Background(), "777", 2550, tc.lifetime); err != nil {
				t.Fatalf("UpdateAdSetBudget: %v", err)
			}
			if path != "/777" {
				t.Errorf("wrote to %q, want the ad set node /777", path)
			}
			// Sent as a STRING, the vocabulary Meta reports and accepts these in.
			if body[tc.wantField] != "2550" {
				t.Errorf("%s = %v, want the string \"2550\"", tc.wantField, body[tc.wantField])
			}
			// The OTHER field must be left entirely alone, and so must the schedule:
			// an amount change may not alter the flight.
			if _, present := body[tc.otherFiel]; present {
				t.Errorf("%s must not be touched by a %s write", tc.otherFiel, tc.wantField)
			}
			if _, present := body["end_time"]; present {
				t.Error("an amount change must not send a schedule field")
			}
			if len(body) != 1 {
				t.Errorf("only the budget field may be written, got %v", body)
			}
		})
	}
}

// TestUpdateAdSetBudget_RefusesAnAmountThatDidNotComeFromTheEncoder is the construction guard:
// it catches a caller that computed its own minor-unit value and bypassed the floor and range.
func TestUpdateAdSetBudget_RefusesAnAmountThatDidNotComeFromTheEncoder(t *testing.T) {
	for _, minor := range []int64{0, -1} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Errorf("%d minor units must be refused before any HTTP call", minor)
		}))
		err := budgetClient(srv.URL, AccountConfig{AccountID: "act_1"}).UpdateAdSetBudget(context.Background(), "777", minor, false)
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), "ResolveBudgetMinorUnits") {
			t.Errorf("%d: want a refusal naming the encoder, got %v", minor, err)
		}
	}
}

// TestUpdateAdSetBudget_A5xxIsUnconfirmedA4xxIsDefinite is the split the dispatcher's
// classification depends on.
func TestUpdateAdSetBudget_A5xxIsUnconfirmedA4xxIsDefinite(t *testing.T) {
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
				_, _ = io.WriteString(w, `{"error":{"message":"nope","code":100}}`)
			}))
			defer srv.Close()

			err := budgetClient(srv.URL, AccountConfig{AccountID: "act_1"}).UpdateAdSetBudget(context.Background(), "777", 2550, false)
			if err == nil {
				t.Fatal("want an error")
			}
			if got := IsOutcomeUnconfirmed(err); got != tc.wantUnconfirmed {
				t.Errorf("IsOutcomeUnconfirmed = %v, want %v (err: %v)", got, tc.wantUnconfirmed, err)
			}
		})
	}
}
