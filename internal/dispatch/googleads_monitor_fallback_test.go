// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
)

const systemGoogleAdsCreds = `{"ClientID":"system-cid","ClientSecret":"csec","DeveloperToken":"dev","RefreshToken":"rt"}`

// systemGoogleAdsConnFor is the LF system row configured for customerID, with a credential whose
// client id ("system-cid") differs from the project fixture's ("cid").
func systemGoogleAdsConnFor(customerID string) *model.Connection {
	c := activeGoogleAdsConn(systemGoogleAdsCreds)
	c.AccountID = customerID
	return c
}

// monitorFallbackDispatcher wires a Google Ads dispatcher whose token endpoint records the
// client_id of every refresh, so a test can tell WHICH stored credential authenticated the
// monitor read, and whose API answers one campaign row.
func monitorFallbackDispatcher(t *testing.T, reader *scopedConnReader) (*GoogleAdsDispatcher, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var clientIDs []string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		clientIDs = append(clientIDs, r.PostForm.Get("client_id"))
		mu.Unlock()
		_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[
			{"customer":{"currencyCode":"USD"},"campaign":{"id":"24447438160","name":"a","status":"PAUSED","advertisingChannelType":"SEARCH"},
			 "campaignBudget":{"amountMicros":"10000000"},"metrics":{"impressions":"0","clicks":"0","costMicros":"0"}}
		]}`)
	}))
	t.Cleanup(apiSrv.Close)
	d := NewGoogleAdsDispatcher(reader, identityEncryptor{},
		googleads.WithTokenURL(tokenSrv.URL), googleads.WithBaseURL(apiSrv.URL))
	return d, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), clientIDs...)
	}
}

// #591: a project with no Google Ads connection of its own monitors the customer the LF system
// row is configured for — the customer its campaigns were created in — on the system credential.
// Before #591 this was a 404, so nothing created through the system account could be monitored.
func TestGoogleAds_ReadAccountMonitor_FallsBackToSystemForItsCustomer(t *testing.T) {
	d, clientIDs := monitorFallbackDispatcher(t, &scopedConnReader{
		rows: map[string]*model.Connection{model.SystemProjectID: systemGoogleAdsConnFor("8666746580")},
	})
	read, err := d.ReadAccountMonitor(context.Background(), "tlf", model.ProviderGoogleAds, "8666746580", 30)
	if err != nil {
		t.Fatalf("ReadAccountMonitor on the LF system customer: %v", err)
	}
	if len(read.Rows) != 1 || read.Rows[0].PlatformCampaignID != "24447438160" {
		t.Errorf("rows = %+v, want the one campaign the API returned", read.Rows)
	}
	if got := clientIDs(); len(got) == 0 || got[0] != "system-cid" {
		t.Errorf("token refreshes used client ids %v, want the LF system credential", got)
	}
}

// The LF system credential reaches more customers than the one LF creates campaigns in, and none
// of the others can hold a campaign this service created on the system account. Monitoring one of
// them keeps the 404 a project without a connection of its own always got, and nothing is sent.
func TestGoogleAds_ReadAccountMonitor_SystemFallbackRefusesOtherCustomers(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary fallback", true: "forced-system mode"}[forced], func(t *testing.T) {
			d, clientIDs := monitorFallbackDispatcher(t, &scopedConnReader{
				rows: map[string]*model.Connection{model.SystemProjectID: systemGoogleAdsConnFor("8666746580")},
			})
			d.creds.forceSystemPaidAds = forced
			_, err := d.ReadAccountMonitor(context.Background(), "tlf", model.ProviderGoogleAds, "1111111111", 30)
			if !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("err = %v, want domain.ErrNotFound for a customer the LF system row is not configured for", err)
			}
			if got := clientIDs(); len(got) != 0 {
				t.Errorf("token refreshes %v, want none: the refusal must come before any upstream call", got)
			}
		})
	}
}

// Which credential reads the monitored customer follows which stored row is configured for it,
// the rule every operation on an existing campaign uses (resolveExisting): the project's own row
// when it is, the LF system row when that one is — including when the project has a connection
// of its own pointed elsewhere, which is every project while forced-system creation is on.
func TestGoogleAds_ReadAccountMonitor_ReadsWithTheRowConfiguredForTheCustomer(t *testing.T) {
	own := googleAdsConnFor("1234567890") // client id "cid"
	system := systemGoogleAdsConnFor("8666746580")
	cases := []struct {
		name      string
		accountID string
		want      string
	}{
		{"the project's own customer", "1234567890", "cid"},
		{"the LF system customer, with a connection of its own elsewhere", "8666746580", "system-cid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, clientIDs := monitorFallbackDispatcher(t, &scopedConnReader{
				rows: map[string]*model.Connection{"tlf": own, model.SystemProjectID: system},
			})
			if _, err := d.ReadAccountMonitor(context.Background(), "tlf", model.ProviderGoogleAds, tc.accountID, 30); err != nil {
				t.Fatalf("ReadAccountMonitor: %v", err)
			}
			if got := clientIDs(); len(got) == 0 || got[0] != tc.want {
				t.Errorf("token refreshes used client ids %v, want %q", got, tc.want)
			}
		})
	}
}

// With no connection of its own and no LF system row, the read is still the 404 it always was.
func TestGoogleAds_ReadAccountMonitor_NoConnectionAnywhereIsNotFound(t *testing.T) {
	d, clientIDs := monitorFallbackDispatcher(t, &scopedConnReader{rows: map[string]*model.Connection{}})
	_, err := d.ReadAccountMonitor(context.Background(), "tlf", model.ProviderGoogleAds, "8666746580", 30)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("err = %v, want domain.ErrNotFound", err)
	}
	if got := clientIDs(); len(got) != 0 {
		t.Errorf("token refreshes %v, want none", got)
	}
}

// MonitorAccountForScope names the one customer the project's campaigns record, ignoring entries
// that record none when another does, and refuses when none records one or two disagree — the
// refusals MonitorScopeForAccount would return for the same scope on a named customer.
func TestGoogleAds_MonitorAccountForScope(t *testing.T) {
	entry := func(id, result string) model.ProjectCampaignScope {
		return model.ProjectCampaignScope{PlatformCampaignID: id, Result: []byte(result)}
	}
	d := NewGoogleAdsDispatcher(fakeConnReader{err: errors.New("must not be reached")}, identityEncryptor{})
	cases := []struct {
		name        string
		scope       []model.ProjectCampaignScope
		want        string
		wantUnknown bool
		wantErr     bool
	}{
		{"one customer", []model.ProjectCampaignScope{
			entry("1", `{"customerId":"8666746580"}`),
			entry("2", `{"googleAdsUrl":"https://ads.google.com/aw/campaigns?ocid=8666746580&campaignId=2"}`),
		}, "8666746580", false, false},
		{"an unrecorded entry beside a recorded one", []model.ProjectCampaignScope{
			entry("1", `{"customerId":"8666746580"}`), entry("2", ``),
		}, "8666746580", false, false},
		{"two customers", []model.ProjectCampaignScope{
			entry("1", `{"customerId":"8666746580"}`), entry("2", `{"customerId":"1111111111"}`),
		}, "", false, true},
		{"none recorded", []model.ProjectCampaignScope{entry("1", ``), entry("2", `{}`)}, "", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := d.MonitorAccountForScope(tc.scope)
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("MonitorAccountForScope = %q, %v; want %q, error %v", got, err, tc.want, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, domain.ErrCampaignAccountMismatch) {
				t.Errorf("err = %v, want it to wrap domain.ErrCampaignAccountMismatch (the monitor's 409)", err)
			}
			if got := errors.Is(err, domain.ErrCampaignProvenanceUnknown); got != tc.wantUnknown {
				t.Errorf("wraps ErrCampaignProvenanceUnknown = %v, want %v: %v", got, tc.wantUnknown, err)
			}
		})
	}
}
