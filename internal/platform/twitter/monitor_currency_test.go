// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// The X monitor's currency rides on the account read it already makes for the timezone: present
// when that body is identity-trusted, describes the configured account and carries a well-formed
// code; "" otherwise — and in every case the timezone (and so the monitor) is unaffected.
func TestAccountCurrency(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"present", `{"data":{"id":"account123","timezone":"America/Los_Angeles","currency":"EUR"}}`, "EUR"},
		{"absent", `{"data":{"id":"account123","timezone":"America/Los_Angeles"}}`, ""},
		{"null", `{"data":{"id":"account123","timezone":"America/Los_Angeles","currency":null}}`, ""},
		{"malformed (lower case) dropped", `{"data":{"id":"account123","timezone":"America/Los_Angeles","currency":"eur"}}`, ""},
		{"malformed (too long) dropped", `{"data":{"id":"account123","timezone":"America/Los_Angeles","currency":"EURO"}}`, ""},
		{"another account's body", `{"data":{"id":"other456","timezone":"America/Los_Angeles","currency":"EUR"}}`, ""},
		{"duplicate key refused", `{"data":{"id":"account123","timezone":"America/Los_Angeles","currency":"USD","Currency":"EUR"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := monitorServer(t, map[string]http.HandlerFunc{"/12/accounts/account123": jsonBody(tc.body)})
			c := monitorClient(srv.URL, time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
			got, err := c.AccountCurrency(context.Background())
			if err != nil {
				t.Fatalf("AccountCurrency: %v", err)
			}
			if got != tc.want {
				t.Errorf("currency = %q, want %q", got, tc.want)
			}
			// The timezone is read from the same body and is untouched by the currency's fate.
			loc, err := c.AccountTimezone(context.Background())
			if err != nil || loc.String() != "America/Los_Angeles" {
				t.Errorf("AccountTimezone = %v, %v; want America/Los_Angeles whatever the currency", loc, err)
			}
			if n := len(rec.byPath("/accounts/account123")); n != 1 {
				t.Errorf("%d account reads, want 1: the currency and the timezone share one cached read", n)
			}
		})
	}
}

// A failed account read is an error for the currency (the caller drops it), never a guess.
func TestAccountCurrency_ReadFailure(t *testing.T) {
	srv, _ := monitorServer(t, map[string]http.HandlerFunc{"/12/accounts/account123": func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}})
	c := monitorClient(srv.URL, time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	if got, err := c.AccountCurrency(context.Background()); err == nil || got != "" {
		t.Errorf("AccountCurrency = %q, %v; want \"\" and an error", got, err)
	}
}

// After the monitor's campaign list has read the account, the currency costs no further call.
func TestAccountCurrency_ReusesTheCampaignListsAccountRead(t *testing.T) {
	srv, rec := monitorServer(t, map[string]http.HandlerFunc{
		"/12/accounts/account123":            jsonBody(`{"data":{"id":"account123","timezone":"UTC","currency":"GBP"}}`),
		"/12/accounts/account123/campaigns":  jsonBody(`{"data":[],"next_cursor":null}`),
		"/12/accounts/account123/line_items": jsonBody(`{"data":[],"next_cursor":null}`),
	})
	c := monitorClient(srv.URL, time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	if _, err := c.ListAccountCampaigns(context.Background()); err != nil {
		t.Fatalf("ListAccountCampaigns: %v", err)
	}
	if got, err := c.AccountCurrency(context.Background()); err != nil || got != "GBP" {
		t.Fatalf("AccountCurrency = %q, %v; want GBP", got, err)
	}
	if n := len(rec.byPath("/accounts/account123")); n != 1 {
		t.Errorf("%d account reads, want 1", n)
	}
}
