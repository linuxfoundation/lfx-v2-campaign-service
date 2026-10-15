// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"testing"
)

// The LinkedIn monitor's currency comes from the budgets' currencyCode in the campaign list it
// already reads, and is only ever "USD" (spend is costInUsd, so only a USD account has one
// currency for every amount). Absent, malformed, disagreeing and non-USD codes all leave it "",
// and none of them fails the read or adds a request.
func TestListAccountCampaignsWithCurrency(t *testing.T) {
	const analytics = `{"elements":[{"pivotValues":["urn:li:sponsoredCampaign:111"],"impressions":1000,"clicks":50,"costInUsd":"12.50"}]}`
	campaigns := func(elements string) string { return `{"elements":[` + elements + `],"metadata":{}}` }
	daily := func(id, code string) string {
		return `{"id":` + id + `,"name":"C` + id + `","status":"ACTIVE","dailyBudget":{"amount":"50.00"` + code + `}}`
	}
	for _, tc := range []struct {
		name string
		list string
		want string
	}{
		{"every budget in USD", campaigns(daily("111", `,"currencyCode":"USD"`) + `,` +
			`{"id":222,"name":"C222","status":"ACTIVE","totalBudget":{"amount":"900.00","currencyCode":"USD"}}`), "USD"},
		{"a daily budget alone in USD", campaigns(daily("111", `,"currencyCode":"USD"`)), "USD"},
		{"a total budget alone in USD", campaigns(`{"id":111,"name":"C111","status":"ACTIVE","totalBudget":{"amount":"900.00","currencyCode":"USD"}}`), "USD"},
		{"one budget without a code among USD ones", campaigns(daily("111", `,"currencyCode":"USD"`) + `,` + daily("222", ``)), ""},
		{"no budget names a currency", campaigns(daily("111", ``)), ""},
		{"no budgets at all", campaigns(`{"id":111,"name":"C111","status":"ACTIVE"}`), ""},
		{"malformed code dropped", campaigns(daily("111", `,"currencyCode":"usd"`)), ""},
		{"one malformed among good ones", campaigns(daily("111", `,"currencyCode":"USD"`) + `,` + daily("222", `,"currencyCode":"US"`)), ""},
		{"codes disagree", campaigns(daily("111", `,"currencyCode":"USD"`) + `,` + daily("222", `,"currencyCode":"EUR"`)), ""},
		// A well-formed, consistent EUR account: spend (costInUsd) and budgets (EUR) are in two
		// currencies, so the monitor names neither.
		{"a non-USD account names none", campaigns(daily("111", `,"currencyCode":"EUR"`)), ""},
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
				t.Error("no rows returned; the currency must never cost the rows")
			}
			if n := len(rec.all()); n != 2 {
				t.Errorf("made %d requests, want the campaign list and the analytics read only: %v", n, rec.all())
			}
		})
	}
}
