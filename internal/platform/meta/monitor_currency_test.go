// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"strings"
	"testing"
)

// The account monitor's currency is the insights rows' own account_currency: present when every
// row names one well-formed code, absent ("") when no row carried one, when one is malformed, or
// when two disagree. None of those fails the read, and the field is requested on the existing
// insights call — no extra request for it (the one other read is the budget conversion's own
// ?fields=currency read, which does not decide the published code).
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
	}{
		{"present on every row", `{"data":[` + row("111", `,"account_currency":"EUR"`) + `,` + row("222", `,"account_currency":"EUR"`) + `],"paging":{}}`, "EUR"},
		{"absent from the rows", `{"data":[` + row("111", ``) + `],"paging":{}}`, ""},
		{"no insights rows at all", `{"data":[],"paging":{}}`, ""},
		{"malformed (lower case) dropped", `{"data":[` + row("111", `,"account_currency":"eur"`) + `],"paging":{}}`, ""},
		{"malformed (four letters) dropped", `{"data":[` + row("111", `,"account_currency":"EURO"`) + `],"paging":{}}`, ""},
		// JPY's minor-unit offset is 1, not 100: the published currency is still withheld for it
		// (monitorPublishableCurrency), although budgets are now converted by its own offset.
		{"zero-decimal currency withheld", `{"data":[` + row("111", `,"account_currency":"JPY"`) + `],"paging":{}}`, ""},
		{"well-formed but unlisted currency withheld", `{"data":[` + row("111", `,"account_currency":"ZZZ"`) + `],"paging":{}}`, ""},
		{"a number", `{"data":[` + row("111", `,"account_currency":123`) + `],"paging":{}}`, ""},
		{"an object", `{"data":[` + row("111", `,"account_currency":{"code":"EUR"}`) + `],"paging":{}}`, ""},
		{"null", `{"data":[` + row("111", `,"account_currency":null`) + `],"paging":{}}`, ""},
		{"one good row, one null", `{"data":[` + row("111", `,"account_currency":"EUR"`) + `,` + row("222", `,"account_currency":null`) + `],"paging":{}}`, ""},
		{"one good row, one numeric", `{"data":[` + row("111", `,"account_currency":"EUR"`) + `,` + row("222", `,"account_currency":978`) + `],"paging":{}}`, ""},
		// encoding/json would keep the LAST of a repeated key, publishing EUR; identityjson
		// refuses the page's currency instead.
		{"exact duplicate key", `{"data":[` + row("111", `,"account_currency":"USD","account_currency":"EUR"`) + `],"paging":{}}`, ""},
		{"case-variant duplicate key", `{"data":[` + row("111", `,"account_currency":"USD","Account_Currency":"EUR"`) + `],"paging":{}}`, ""},
		{"rows disagree", `{"data":[` + row("111", `,"account_currency":"EUR"`) + `,` + row("222", `,"account_currency":"USD"`) + `],"paging":{}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, rec := monitorPageResponses(t, []string{campaigns}, []string{tc.insights})
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
