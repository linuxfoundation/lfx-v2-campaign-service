// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// currencyCreateServer serves the create path's requests for an account denominated in currency,
// and records the raw ad-set POST body so the budget can be asserted as it went on the wire. It
// also counts every POST, so a refusal can be shown to have created nothing upstream at all.
func currencyCreateServer(t *testing.T, currency string) (*httptest.Server, func() (string, int)) {
	t.Helper()
	var (
		mu       sync.Mutex
		adSet    string
		posts    int
		creative int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			mu.Lock()
			posts++
			mu.Unlock()
		}
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "filtering"):
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/act_777"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1,"currency":"`+currency+`"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adsets"):
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			adSet = string(body)
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adcreatives"):
			mu.Lock()
			creative++
			n := creative
			mu.Unlock()
			_, _ = io.WriteString(w, `{"id":"creative_`+strconv.Itoa(n)+`"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() (string, int) {
		mu.Lock()
		defer mu.Unlock()
		return adSet, posts
	}
}

// TestCreateCampaign_TheBudgetSentIsTheBudgetRequested is #235's create-path invariant. The
// service persists the REQUESTED amount (campaigns.budget_amount), so the only way the row can
// equal what Meta holds is for every accepted amount to reach Meta exactly — amount x the
// account currency's minor-unit offset, with no rounding — and for every amount that cannot be
// sent exactly to be refused before anything is created — no campaign, no ad set. JPY and KRW are zero-decimal (offset 1:
// the minor unit IS the whole unit); USD is two-decimal (offset 100). The offset is derived from
// the account's own currency, as in production, not set explicitly.
func TestCreateCampaign_TheBudgetSentIsTheBudgetRequested(t *testing.T) {
	cases := []struct {
		name      string
		currency  string
		budget    float64
		wantMinor int64 // the daily_budget on the wire; 0 when refused
		wantErr   string
	}{
		{name: "JPY whole yen is sent unscaled", currency: "JPY", budget: 5000, wantMinor: 5000},
		{name: "JPY one yen is the floor", currency: "JPY", budget: 1, wantMinor: 1},
		{name: "JPY a fraction of a yen is refused, not rounded to 1", currency: "JPY", budget: 1.4, wantErr: "enter whole amounts for this currency"},
		{name: "JPY half a yen is refused, not rounded up", currency: "JPY", budget: 2500.5, wantErr: "enter whole amounts for this currency"},
		{name: "KRW whole won is sent unscaled", currency: "KRW", budget: 30000, wantMinor: 30000},
		{name: "KRW a fraction of a won is refused", currency: "KRW", budget: 30000.25, wantErr: "enter whole amounts for this currency"},
		{name: "USD dollars and cents are sent in cents", currency: "USD", budget: 25.5, wantMinor: 2550},
		{name: "USD float representation error still encodes exactly", currency: "USD", budget: 19.99, wantMinor: 1999},
		{name: "USD a fraction of a cent is refused", currency: "USD", budget: 25.505, wantErr: "enter at most 2 decimal places"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, adSet := currencyCreateServer(t, tc.currency)
			c := NewClient(
				Credentials{AccessToken: "tok"},
				AccountConfig{AccountID: "act_777", PageID: "987654321"},
				WithBaseURL(srv.URL), WithClock(fixedMetaClock()),
			)
			in := metaFieldInput()
			in.Budget = tc.budget
			_, err := c.CreateCampaign(context.Background(), in)
			body, posts := adSet()

			if tc.wantErr != "" {
				if !errors.Is(err, ErrBudgetAmountInvalid) || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want an ErrBudgetAmountInvalid containing %q", err, tc.wantErr)
				}
				if posts != 0 {
					t.Fatalf("%d POSTs were sent for a refused budget; it must be refused before anything is created", posts)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			if body == "" {
				t.Fatal("no ad set was sent")
			}
			// Assert the raw wire value: decoding into map[string]any would turn a quoted
			// string and a bare number into the same Go value and hide a type change.
			var raw map[string]json.RawMessage
			if err := json.Unmarshal([]byte(body), &raw); err != nil {
				t.Fatalf("ad-set body is not JSON: %v (%s)", err, body)
			}
			want := strconv.FormatInt(tc.wantMinor, 10)
			if got := string(raw["daily_budget"]); got != want {
				t.Fatalf("daily_budget on the wire = %s, want %s", got, want)
			}
			// The persisted amount is the requested one; it must read back as exactly what was
			// sent once the minor units are divided by the currency's offset.
			offset, ok := currencyOffsetFor(tc.currency)
			if !ok {
				t.Fatalf("no offset for %s", tc.currency)
			}
			if back := float64(tc.wantMinor) / float64(offset); back != tc.budget {
				t.Fatalf("%d minor units of %s is %v, but %v would be persisted", tc.wantMinor, tc.currency, back, tc.budget)
			}
		})
	}
}

// TestCurrencyOffsetFor_ZeroAndTwoDecimal pins the scales the invariant above depends on.
func TestCurrencyOffsetFor_ZeroAndTwoDecimal(t *testing.T) {
	for cur, want := range map[string]int64{"JPY": 1, "KRW": 1, "USD": 100} {
		if got, ok := currencyOffsetFor(cur); !ok || got != want {
			t.Errorf("currencyOffsetFor(%s) = %d, %v; want %d", cur, got, ok, want)
		}
	}
}
