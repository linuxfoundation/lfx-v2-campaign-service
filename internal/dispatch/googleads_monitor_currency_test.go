// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

// The Google Ads monitor read carries the account's currency from customer.currency_code on the
// same GAQL rows (#454), so the rule engine can size its money thresholds in it.
func TestGoogleAds_ReadAccountMonitor_CarriesCurrency(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[
			{"customer":{"currencyCode":"JPY"},"campaign":{"id":"1","name":"a","status":"ENABLED","advertisingChannelType":"SEARCH"},
			 "campaignBudget":{"amountMicros":"1000000000"},"metrics":{"impressions":"100","clicks":"5","costMicros":"250000000"}},
			{"customer":{"currencyCode":"JPY"},"campaign":{"id":"2","name":"b","status":"ENABLED","advertisingChannelType":"DISPLAY"},
			 "campaignBudget":{"amountMicros":"2000000000"},"metrics":{"impressions":"200","clicks":"1","costMicros":"50000000"}}
		]}`)
	}))
	t.Cleanup(apiSrv.Close)
	d := NewGoogleAdsDispatcher(
		fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL),
	)
	read, err := d.ReadAccountMonitor(context.Background(), "cncf", model.ProviderGoogleAds, "1234567890", 30)
	if err != nil {
		t.Fatalf("ReadAccountMonitor: %v", err)
	}
	if read.Currency != "JPY" || len(read.Rows) != 2 {
		t.Errorf("read = %+v, want two rows in JPY", read)
	}
}

func TestGoogleAdsAccountCurrency(t *testing.T) {
	row := func(code string) googleads.AccountCampaignRow {
		return googleads.AccountCampaignRow{CurrencyCode: code}
	}
	cases := []struct {
		name string
		rows []googleads.AccountCampaignRow
		want string
	}{
		{"every row agrees", []googleads.AccountCampaignRow{row("EUR"), row("EUR")}, "EUR"},
		{"no rows", nil, ""},
		{"one row without a code", []googleads.AccountCampaignRow{row("EUR"), row("")}, ""},
		{"first row without a code", []googleads.AccountCampaignRow{row(""), row("EUR")}, ""},
		{"malformed code", []googleads.AccountCampaignRow{row("eur")}, ""},
		{"rows disagree", []googleads.AccountCampaignRow{row("EUR"), row("USD")}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := googleAdsAccountCurrency(tc.rows); got != tc.want {
				t.Errorf("googleAdsAccountCurrency = %q, want %q", got, tc.want)
			}
		})
	}
}
