// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The Microsoft monitor's currency is ONE Customer Management Account/Query read: present when
// the body is identity-trusted, describes the client's own account and names a currency the map
// knows; "" when CurrencyCode is absent, null or a name the map does not know (malformed); an
// error — which the monitor treats as "currency unknown" — when the body cannot be trusted.
func TestAccountCurrency(t *testing.T) {
	const acct = "187654321"
	for _, tc := range []struct {
		name      string
		body      string
		want      string
		untrusted bool
	}{
		{"present", `{"Account":{"Id":187654321,"Name":"Acct","CurrencyCode":"USDollar"}}`, "USD", false},
		{"present, id quoted", `{"Account":{"Id":"187654321","CurrencyCode":"Euro"}}`, "EUR", false},
		{"absent", `{"Account":{"Id":187654321,"Name":"Acct"}}`, "", false},
		{"null", `{"Account":{"Id":187654321,"CurrencyCode":null}}`, "", false},
		{"an ISO code is not a value-set name (malformed) dropped", `{"Account":{"Id":187654321,"CurrencyCode":"USD"}}`, "", false},
		{"unknown name (malformed) dropped", `{"Account":{"Id":187654321,"CurrencyCode":"usdollar"}}`, "", false},
		{"another account's body", `{"Account":{"Id":999,"CurrencyCode":"USDollar"}}`, "", true},
		{"no Account object", `{}`, "", true},
		{"duplicate key refused", `{"Account":{"Id":187654321,"CurrencyCode":"USDollar","currencyCode":"Euro"}}`, "", true},
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

// Every name the map knows yields a well-formed code: a typo in a mapped ISO value would
// otherwise be dropped silently in production.
func TestCurrencyCodeISO_EveryValueIsWellFormed(t *testing.T) {
	seen := map[string]string{}
	for name, iso := range currencyCodeISO {
		if model.NormalizeCurrencyCode(iso) != iso {
			t.Errorf("%s maps to %q, not an ISO 4217-shaped code", name, iso)
		}
		if prev, dup := seen[iso]; dup {
			t.Errorf("%s and %s both map to %s", prev, name, iso)
		}
		seen[iso] = name
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
		{"another account", http.StatusOK, `{"Account":{"Id":"UPSTREAM-CANARY","CurrencyCode":"USDollar"}}`},
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
		_, _ = io.WriteString(w, `{"Account":{"Id":187654321,"CurrencyCode":"BritishPound"}}`)
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
