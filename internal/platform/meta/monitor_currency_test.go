// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"strings"
	"testing"
)

// The account monitor's currency is the insights rows' own account_currency: present when every
// row names one well-formed code that the budgets were converted in — the ad account's currency
// from the budget conversion's own ?fields=currency read, with a minor-unit offset
// currencyOffsetFor knows, whatever that offset is — and absent ("") when no row carried one,
// when one is malformed, when two disagree, when the code is unknown, or when it is not the
// budgets' currency. None of those fails the read, and the field is requested on the existing
// insights call — no extra request for it.
func TestListAccountCampaignsWithCurrency(t *testing.T) {
	row := func(id, currencyField string) string {
		return `{"campaign_id":"` + id + `","impressions":"10","clicks":"1","spend":"1.50"` + currencyField + `}`
	}
	campaigns := `{"data":[` +
		`{"id":"111","name":"One","status":"ACTIVE","daily_budget":"1000"},` +
		`{"id":"222","name":"Two","status":"ACTIVE","daily_budget":"1000"}],"paging":{}}`
	for _, tc := range []struct {
		name     string
		insights string
		want     string
		// budget is the ad account's currency from the ?fields=currency read; "" serves "EUR".
		budget string
	}{
		{"present on every row", `{"data":[` + row("111", `,"account_currency":"EUR"`) + `,` + row("222", `,"account_currency":"EUR"`) + `],"paging":{}}`, "EUR", ""},
		{"absent from the rows", `{"data":[` + row("111", ``) + `],"paging":{}}`, "", ""},
		{"no insights rows at all", `{"data":[],"paging":{}}`, "", ""},
		{"malformed (lower case) dropped", `{"data":[` + row("111", `,"account_currency":"eur"`) + `],"paging":{}}`, "", ""},
		{"malformed (four letters) dropped", `{"data":[` + row("111", `,"account_currency":"EURO"`) + `],"paging":{}}`, "", ""},
		// Budgets are converted with the account currency's own offset, so a zero-decimal
		// currency (offset 1) is published like a two-decimal one.
		{"zero-decimal currency published", `{"data":[` + row("111", `,"account_currency":"JPY"`) + `],"paging":{}}`, "JPY", "JPY"},
		{"another zero-decimal currency published", `{"data":[` + row("111", `,"account_currency":"KRW"`) + `],"paging":{}}`, "KRW", "KRW"},
		// No offset is known for an unlisted code, so its budgets are left unconverted.
		{"well-formed but unlisted currency withheld", `{"data":[` + row("111", `,"account_currency":"ZZZ"`) + `],"paging":{}}`, "", "ZZZ"},
		// Budgets converted at USD's scale must not be labelled JPY, nor JPY's labelled EUR.
		{"insights currency is not the budgets' currency", `{"data":[` + row("111", `,"account_currency":"JPY"`) + `],"paging":{}}`, "", "USD"},
		{"budgets' currency is not the insights currency", `{"data":[` + row("111", `,"account_currency":"EUR"`) + `],"paging":{}}`, "", "JPY"},
		{"a number", `{"data":[` + row("111", `,"account_currency":123`) + `],"paging":{}}`, "", ""},
		{"an object", `{"data":[` + row("111", `,"account_currency":{"code":"EUR"}`) + `],"paging":{}}`, "", ""},
		{"null", `{"data":[` + row("111", `,"account_currency":null`) + `],"paging":{}}`, "", ""},
		{"one good row, one null", `{"data":[` + row("111", `,"account_currency":"EUR"`) + `,` + row("222", `,"account_currency":null`) + `],"paging":{}}`, "", ""},
		{"one good row, one numeric", `{"data":[` + row("111", `,"account_currency":"EUR"`) + `,` + row("222", `,"account_currency":978`) + `],"paging":{}}`, "", ""},
		// encoding/json would keep the LAST of a repeated key, publishing EUR; identityjson
		// refuses the page's currency instead.
		{"exact duplicate key", `{"data":[` + row("111", `,"account_currency":"USD","account_currency":"EUR"`) + `],"paging":{}}`, "", ""},
		{"case-variant duplicate key", `{"data":[` + row("111", `,"account_currency":"USD","Account_Currency":"EUR"`) + `],"paging":{}}`, "", ""},
		{"rows disagree", `{"data":[` + row("111", `,"account_currency":"EUR"`) + `,` + row("222", `,"account_currency":"USD"`) + `],"paging":{}}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget := tc.budget
			if budget == "" {
				budget = "EUR"
			}
			srv, rec := monitorServer(t, monitorFixture{campaigns: []string{campaigns}, insights: []string{tc.insights},
				currency: `{"currency":"` + budget + `","id":"act_123"}`})
			rows, currency, err := newMonitorClient(srv).ListAccountCampaignsWithCurrency(context.Background(), "act_123", 30)
			if err != nil {
				t.Fatalf("ListAccountCampaignsWithCurrency: %v", err)
			}
			if currency != tc.want {
				t.Errorf("currency = %q, want %q", currency, tc.want)
			}
			if len(rows) != 2 {
				t.Errorf("got %d rows, want both campaigns whatever the currency: %+v", len(rows), rows)
			}
			for _, r := range rows {
				if r.CampaignID == "111" && strings.Contains(tc.insights, `"campaign_id":"111"`) && (r.Impressions != 10 || r.FetchFailed) {
					t.Errorf("row 111 = %+v, want its metrics read whatever the currency field holds", r)
				}
			}
			if rec.count() != 3 {
				t.Errorf("made %d requests, want the insights, campaign list and account-currency reads only: %v", rec.count(), rec.all())
			}
			for _, u := range rec.all() {
				if strings.Contains(u, "/insights") && !strings.Contains(u, "account_currency") {
					t.Errorf("insights request does not ask for account_currency: %s", u)
				}
			}
		})
	}
}
