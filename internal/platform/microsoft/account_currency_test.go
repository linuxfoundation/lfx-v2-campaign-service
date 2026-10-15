// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

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
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The Microsoft monitor's currency is ONE Customer Management Account/Query read: present when
// the body is identity-trusted, describes the client's own account and carries a code from the v13
// CurrencyCode value set that is a current ISO 4217 code; "" when CurrencyCode is absent, null, not
// a string, withdrawn (RUR, YUN, …) or not a value-set code at all (the names an earlier version of
// this client wrongly mapped, such as "USDollar"); an error — which the monitor treats as
// "currency unknown" — when the body cannot be trusted.
func TestAccountCurrency(t *testing.T) {
	const acct = "187654321"
	for _, tc := range []struct {
		name      string
		body      string
		want      string
		untrusted bool
	}{
		{"USD accepted", `{"Account":{"Id":187654321,"Name":"Acct","CurrencyCode":"USD"}}`, "USD", false},
		{"EUR accepted, id quoted", `{"Account":{"Id":"187654321","CurrencyCode":"EUR"}}`, "EUR", false},
		{"JPY accepted", `{"Account":{"Id":187654321,"CurrencyCode":"JPY"}}`, "JPY", false},
		{"HRK kept (accurate for an unconverted account)", `{"Account":{"Id":187654321,"CurrencyCode":"HRK"}}`, "HRK", false},
		{"absent", `{"Account":{"Id":187654321,"Name":"Acct"}}`, "", false},
		{"null", `{"Account":{"Id":187654321,"CurrencyCode":null}}`, "", false},
		{"a number", `{"Account":{"Id":187654321,"CurrencyCode":840}}`, "", false},
		{"RUR (withdrawn) withheld", `{"Account":{"Id":187654321,"CurrencyCode":"RUR"}}`, "", false},
		{"YUN (non-ISO) withheld", `{"Account":{"Id":187654321,"CurrencyCode":"YUN"}}`, "", false},
		{"a name, USDollar, withheld", `{"Account":{"Id":187654321,"CurrencyCode":"USDollar"}}`, "", false},
		{"lower case withheld", `{"Account":{"Id":187654321,"CurrencyCode":"usd"}}`, "", false},
		{"another account's body", `{"Account":{"Id":999,"CurrencyCode":"USD"}}`, "", true},
		{"no Account object", `{}`, "", true},
		{"duplicate key refused", `{"Account":{"Id":187654321,"CurrencyCode":"USD","currencyCode":"EUR"}}`, "", true},
		{"not JSON", `<html>`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var path string
			var reqBody map[string]any
			c := newCustomerClient(t, AccountConfig{AccountID: acct}, func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				path = r.URL.Path
				_ = json.Unmarshal(raw, &reqBody)
				mu.Unlock()
				_, _ = io.WriteString(w, tc.body)
			})
			got, err := c.AccountCurrency(context.Background())
			if tc.untrusted {
				if !errors.Is(err, errAccountCurrencyUntrusted) {
					t.Fatalf("AccountCurrency = %q, %v; want errAccountCurrencyUntrusted", got, err)
				}
			} else if err != nil {
				t.Fatalf("AccountCurrency: %v", err)
			}
			if got != tc.want {
				t.Errorf("currency = %q, want %q", got, tc.want)
			}
			mu.Lock()
			defer mu.Unlock()
			if !strings.HasSuffix(path, "/CustomerManagement/v13/Account/Query") {
				t.Errorf("request path = %q, want Customer Management Account/Query", path)
			}
			if id, _ := reqBody["AccountId"].(float64); id != 187654321 {
				t.Errorf("request AccountId = %v, want the client's own account", reqBody["AccountId"])
			}
		})
	}
}

// Every publishable code is ISO 4217-shaped, and none of the withdrawn or non-ISO codes in
// Microsoft's value set is publishable.
func TestMicrosoftPublishableCurrencies(t *testing.T) {
	for code := range microsoftPublishableCurrencies {
		if model.NormalizeCurrencyCode(code) != code {
			t.Errorf("%q is not an ISO 4217-shaped code", code)
		}
	}
	for _, code := range []string{"AZM", "BGL", "BYB", "EEK", "LTL", "LVL", "ROL", "RUR", "SIT", "SKK", "VEF", "YUN", "ZWD"} {
		if _, ok := microsoftPublishableCurrencies[code]; ok {
			t.Errorf("%s is withdrawn or non-ISO but publishable", code)
		}
	}
}

// A recognised code is reused for hours; an answer with no recognised code only for minutes, since
// it is not a definitive answer.
func TestAccountCurrency_CacheLifetimes(t *testing.T) {
	for _, tc := range []struct {
		code      string
		want      string
		reusedFor time.Duration
	}{
		{"USD", "USD", accountCurrencyCacheFor},
		{"RUR", "", accountCurrencyUnrecognisedCacheFor},
	} {
		t.Run(tc.code, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
			tok := httptest.NewServer(http.HandlerFunc(tokenHandler))
			t.Cleanup(tok.Close)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				calls++
				mu.Unlock()
				_, _ = io.WriteString(w, `{"Account":{"Id":187654321,"CurrencyCode":"`+tc.code+`"}}`)
			}))
			t.Cleanup(api.Close)
			c := NewClient(testCreds(), AccountConfig{AccountID: "187654321"},
				WithTokenURL(tok.URL), WithCustomerBaseURL(api.URL), WithBaseURL("http://campaign.invalid"),
				WithClock(func() time.Time { return now }))
			read := func() {
				t.Helper()
				if got, err := c.AccountCurrency(context.Background()); err != nil || got != tc.want {
					t.Fatalf("AccountCurrency = %q, %v; want %q", got, err, tc.want)
				}
			}
			count := func() int { mu.Lock(); defer mu.Unlock(); return calls }
			read()
			now = now.Add(tc.reusedFor - time.Second)
			read()
			if n := count(); n != 1 {
				t.Errorf("%d reads within %v, want 1", n, tc.reusedFor)
			}
			now = now.Add(2 * time.Second)
			read()
			if n := count(); n != 2 {
				t.Errorf("%d reads after %v, want a re-read", n, tc.reusedFor)
			}
		})
	}
	if accountCurrencyUnrecognisedCacheFor >= time.Hour {
		t.Errorf("an unrecognised answer is cached for %v, want it short", accountCurrencyUnrecognisedCacheFor)
	}
}

// A failed read is an error that carries no upstream text, and is never a guessed currency.
func TestAccountCurrency_FailureCarriesNoUpstreamText(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"5xx", http.StatusInternalServerError, `{"Errors":[{"Message":"UPSTREAM-CANARY"}]}`},
		{"another account", http.StatusOK, `{"Account":{"Id":"UPSTREAM-CANARY","CurrencyCode":"USD"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCustomerClient(t, AccountConfig{AccountID: "187654321"}, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			got, err := c.AccountCurrency(context.Background())
			if err == nil || got != "" {
				t.Fatalf("AccountCurrency = %q, %v; want \"\" and an error", got, err)
			}
			if strings.Contains(err.Error(), "UPSTREAM-CANARY") {
				t.Errorf("error %q carries upstream text", err)
			}
		})
	}
}

// A throttled Account/Query is not backed off and retried — the currency is only a label — and a
// failure is never cached: the next read asks again, and its success is then reused.
func TestAccountCurrency_NoThrottleRetryAndFailuresNotCached(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	c := newCustomerClient(t, AccountConfig{AccountID: "187654321"}, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"Account":{"Id":187654321,"CurrencyCode":"GBP"}}`)
	})
	if got, err := c.AccountCurrency(context.Background()); err == nil || got != "" {
		t.Fatalf("throttled AccountCurrency = %q, %v; want an error", got, err)
	}
	mu.Lock()
	if calls != 1 {
		t.Errorf("%d requests for a throttled read, want 1 (no retry)", calls)
	}
	mu.Unlock()
	for i := 0; i < 2; i++ {
		if got, err := c.AccountCurrency(context.Background()); err != nil || got != "GBP" {
			t.Fatalf("AccountCurrency #%d = %q, %v; want GBP", i, got, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Errorf("%d requests, want 2: the failure re-read once, the success then cached", calls)
	}
}
