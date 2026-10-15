// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"strings"
	"testing"
)

// The LinkedIn monitor reads spend as costInLocalCurrency and takes the currency from the
// budgets' currencyCode in the campaign list it already reads: the account's own code when every
// budget names the same well-formed one. A missing or malformed code leaves it "", and two codes
// that disagree also withhold pacing from the rows whose budget named one. No extra request.
func TestListAccountCampaignsWithCurrency(t *testing.T) {
	const analytics = `{"elements":[{"pivotValues":["urn:li:sponsoredCampaign:111"],"impressions":1000,"clicks":50,"costInLocalCurrency":"12.50"}]}`
	campaigns := func(elements string) string { return `{"elements":[` + elements + `],"metadata":{}}` }
	daily := func(id, code string) string {
		return `{"id":` + id + `,"name":"C` + id + `","status":"ACTIVE","dailyBudget":{"amount":"50.00"` + code + `}}`
	}
	for _, tc := range []struct {
		name         string
		list         string
		want         string
		pacingWithld bool // rows whose budget named a currency carry PacingUnknown
	}{
		{"every budget in USD", campaigns(daily("111", `,"currencyCode":"USD"`) + `,` +
			`{"id":222,"name":"C222","status":"ACTIVE","totalBudget":{"amount":"900.00","currencyCode":"USD"}}`), "USD", false},
		{"a EUR account names EUR", campaigns(daily("111", `,"currencyCode":"EUR"`)), "EUR", false},
		{"a daily budget alone", campaigns(daily("111", `,"currencyCode":"JPY"`)), "JPY", false},
		{"a total budget alone", campaigns(`{"id":111,"name":"C111","status":"ACTIVE","totalBudget":{"amount":"900.00","currencyCode":"GBP"}}`), "GBP", false},
		{"one budget without a code", campaigns(daily("111", `,"currencyCode":"USD"`) + `,` + daily("222", ``)), "", false},
		{"no budget names a currency", campaigns(daily("111", ``)), "", false},
		{"no budgets at all", campaigns(`{"id":111,"name":"C111","status":"ACTIVE"}`), "", false},
		{"malformed code dropped", campaigns(daily("111", `,"currencyCode":"usd"`)), "", false},
		{"codes disagree", campaigns(daily("111", `,"currencyCode":"USD"`) + `,` + daily("222", `,"currencyCode":"EUR"`)), "", true},
		{"a malformed code disagrees", campaigns(daily("111", `,"currencyCode":"USD"`) + `,` + daily("222", `,"currencyCode":"US"`)), "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := adAccountsServer(t, tc.list, analytics)
			c := NewClient(Credentials{AccessToken: "tok-secret-abc"}, RuntimeConfig{}, WithBaseURL(srv.URL))
			rows, currency, err := c.ListAccountCampaignsWithCurrency(context.Background(), "512345678", 7)
			if err != nil {
				t.Fatalf("ListAccountCampaignsWithCurrency: %v", err)
			}
			if currency != tc.want {
				t.Errorf("currency = %q, want %q", currency, tc.want)
			}
			if len(rows) == 0 {
				t.Fatal("no rows returned; the currency must never cost the rows")
			}
			for _, r := range rows {
				if r.PacingUnknown != (tc.pacingWithld && r.budgetCurrencyNamed) {
					t.Errorf("row %s PacingUnknown = %v, want %v", r.CampaignID, r.PacingUnknown, tc.pacingWithld && r.budgetCurrencyNamed)
				}
			}
			if n := len(rec.all()); n != 2 {
				t.Errorf("made %d requests, want the campaign list and the analytics read only: %v", n, rec.all())
			}
		})
	}
}

// Spend is costInLocalCurrency — the account's own currency — and costInUsd is neither requested
// nor read, so a EUR account's spend and budgets are in one currency.
func TestListAccountCampaigns_SpendIsLocalCurrency(t *testing.T) {
	srv, rec := adAccountsServer(t,
		`{"elements":[{"id":111,"name":"C111","status":"ACTIVE","dailyBudget":{"amount":"50.00","currencyCode":"EUR"}}],"metadata":{}}`,
		`{"elements":[{"pivotValues":["urn:li:sponsoredCampaign:111"],"impressions":1000,"clicks":50,"costInLocalCurrency":"40.00","costInUsd":"43.20"}]}`,
	)
	c := NewClient(Credentials{AccessToken: "tok-secret-abc"}, RuntimeConfig{}, WithBaseURL(srv.URL))
	rows, currency, err := c.ListAccountCampaignsWithCurrency(context.Background(), "512345678", 7)
	if err != nil {
		t.Fatalf("ListAccountCampaignsWithCurrency: %v", err)
	}
	if currency != "EUR" || len(rows) != 1 || rows[0].Spend != 40 || rows[0].DailyBudget != 50 {
		t.Fatalf("currency=%q rows=%+v, want EUR spend 40 against a EUR 50/day budget", currency, rows)
	}
	for _, u := range rec.all() {
		if strings.Contains(u, "/adAnalytics") && (!strings.Contains(u, "costInLocalCurrency") || strings.Contains(u, "costInUsd")) {
			t.Errorf("analytics request = %s, want costInLocalCurrency and not costInUsd", u)
		}
	}
}
