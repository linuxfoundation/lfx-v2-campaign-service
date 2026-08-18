// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

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

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// The Microsoft reporting contract follows Microsoft's published v13 docs but has never
// been exercised against a live Bing account, so the read is gated OFF by default. The
// gate is the only thing standing between an unverified contract and numbers a dashboard
// would render as authoritative — which makes an untested gate the one most worth pinning.

// TestMicrosoftReadMetrics_DisabledGateRefusesTheRead pins the default-OFF behaviour: a
// disabled read must answer ErrMetricsUnsupported, never an empty-but-successful result.
// A zero returned here is indistinguishable, to every consumer, from a measured zero.
func TestMicrosoftReadMetrics_DisabledGateRefusesTheRead(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "")
	d := NewMicrosoftDispatcher(nil, identityEncryptor{})

	got, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMicrosoftAds,
		&model.Campaign{Platform: model.ProviderMicrosoftAds, PlatformCampaignID: "999"},
		model.MetricsWindowLast7Days)

	if !errors.Is(err, domain.ErrMetricsUnsupported) {
		t.Errorf("want ErrMetricsUnsupported, got %v", err)
	}
	if got != nil {
		t.Errorf("a refused read must return no metrics, got %+v", got)
	}
}

// TestMicrosoftReadMetrics_GateFailsClosedOnEveryNonTrueValue pins that the gate accepts
// ONLY the literal "true". Anything else — including values an operator might reasonably
// expect to work — must leave the unverified read disabled.
func TestMicrosoftReadMetrics_GateFailsClosedOnEveryNonTrueValue(t *testing.T) {
	for _, v := range []string{"", "TRUE", "True", "1", "yes", "on", " true", "true "} {
		t.Run("value="+v, func(t *testing.T) {
			t.Setenv(constants.EnvMicrosoftMetricsEnabled, v)
			d := NewMicrosoftDispatcher(nil, identityEncryptor{})

			_, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMicrosoftAds,
				&model.Campaign{Platform: model.ProviderMicrosoftAds, PlatformCampaignID: "999"},
				model.MetricsWindowLast7Days)

			if !errors.Is(err, domain.ErrMetricsUnsupported) {
				t.Errorf("%q must leave the read disabled, got %v", v, err)
			}
		})
	}
}

// TestMicrosoftReadMetrics_EnabledGatePassesTheGate proves the gate is a gate and not an
// unconditional refusal: with it on, the call proceeds past the gate and fails later, on
// credential resolution. Without this the disabled-path tests above would still pass
// against an implementation that refused every read.
func TestMicrosoftReadMetrics_EnabledGatePassesTheGate(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	d := NewMicrosoftDispatcher(fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{})

	_, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMicrosoftAds,
		&model.Campaign{Platform: model.ProviderMicrosoftAds, PlatformCampaignID: "999"},
		model.MetricsWindowLast7Days)

	if err == nil {
		t.Fatal("expected an error from credential resolution")
	}
	if errors.Is(err, domain.ErrMetricsUnsupported) {
		t.Errorf("the gate was ON, so the read must not answer ErrMetricsUnsupported: %v", err)
	}
}

// TestMicrosoftReadMetrics_MissingPlatformCampaignIDIsRejected pins that the id check sits
// AFTER the gate — a campaign never provisioned upstream has nothing to read, and asking
// Microsoft for an empty id would be a request we know cannot succeed.
func TestMicrosoftReadMetrics_MissingPlatformCampaignIDIsRejected(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	d := NewMicrosoftDispatcher(nil, identityEncryptor{})

	for name, camp := range map[string]*model.Campaign{
		"nil campaign": nil,
		"empty id":     {Platform: model.ProviderMicrosoftAds},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMicrosoftAds, camp,
				model.MetricsWindowLast7Days)
			if err == nil {
				t.Fatal("expected an error")
			}
			if errors.Is(err, domain.ErrMetricsUnsupported) {
				t.Errorf("this is a missing-id error, not an unsupported-platform one: %v", err)
			}
		})
	}
}

// TestMicrosoftReadMetrics_ForeignAccountIs409AndNeverQueries pins the account-provenance
// guard on the READ path. The campaign row records the ad account it was created under; the
// connection the project resolves TODAY points somewhere else (UpdateMicrosoftAds can
// re-point it). Microsoft campaign ids are unique only WITHIN an account, so the stored id
// queried against the new account returns either nothing — a false "no metrics" — or an
// unrelated campaign's numbers rendered as this campaign's measurement. The read must fail
// with domain.ErrCampaignAccountMismatch (409) and must not reach Microsoft at all.
//
// Both blob shapes are covered: the explicit accountId the create path stamps, and the
// legacy row that only carries the account in its console URL's aid= parameter.
func TestMicrosoftReadMetrics_ForeignAccountIs409AndNeverQueries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
	}{
		{"accountId field", `{"accountId":"7654321","campaignId":"999"}`},
		{"legacy aid fallback", `{"campaignId":"999","microsoftAdsUrl":"https://ads.microsoft.com/campaign/vnext/campaigns?aid=7654321"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				t.Error("no token may be fetched for a campaign owned by another ad account")
			}))
			defer tokenSrv.Close()
			apiSrv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				t.Error("Microsoft must not be queried for a campaign owned by another ad account")
			}))
			defer apiSrv.Close()

			// activeMicrosoftConn resolves to account 1234567; the rows above record 7654321.
			d := NewMicrosoftDispatcher(
				fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
				microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL),
				microsoft.WithReportingBaseURL(apiSrv.URL),
			)
			camp := &model.Campaign{
				Platform:           model.ProviderMicrosoftAds,
				PlatformCampaignID: "999",
				Result:             json.RawMessage(tc.result),
			}
			got, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMicrosoftAds, camp,
				model.MetricsWindowLast7Days)
			if err == nil {
				t.Fatal("expected a mismatch error")
			}
			if !errors.Is(err, domain.ErrCampaignAccountMismatch) {
				t.Errorf("error must wrap ErrCampaignAccountMismatch (409), got %T: %v", err, err)
			}
			if got != nil {
				t.Errorf("a refused read must return no metrics, got %+v", got)
			}
			// Assert the VALUES, not just that an error happened: a message naming the wrong
			// pair of accounts would still satisfy the sentinel check above.
			if !strings.Contains(err.Error(), "7654321") || !strings.Contains(err.Error(), "1234567") {
				t.Errorf("error must name the created account (7654321) and the resolved one (1234567), got %v", err)
			}
		})
	}
}

// TestMicrosoftReadMetrics_UnknownOrMatchingAccountStillReads is the other half of the guard:
// it must not become a wall. A row with no recoverable account id (every row written before
// the field existed and before the console URL was parsed for it) cannot PROVE a mismatch,
// and a row recording the SAME account the connection resolves to is not one — so both must
// proceed past the guard and reach Microsoft. Absence means "unknown, proceed", exactly as
// the google-ads adapter treats an absent customer id.
func TestMicrosoftReadMetrics_UnknownOrMatchingAccountStillReads(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result string
	}{
		{"no provenance recorded", `{"campaignId":"999"}`},
		{"matching accountId", `{"accountId":"1234567","campaignId":"999"}`},
		{"matching legacy aid", `{"campaignId":"999","microsoftAdsUrl":"https://ads.microsoft.com/campaign/vnext/campaigns?aid=1234567"}`},
		{"unparseable blob", `not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
			var mu sync.Mutex
			var submitted bool
			tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"access_token":"tok","expires_in":3600,"token_type":"Bearer"}`)
			}))
			defer tokenSrv.Close()
			apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "GenerateReport/Submit") {
					mu.Lock()
					submitted = true
					mu.Unlock()
				}
				w.WriteHeader(http.StatusBadGateway)
			}))
			defer apiSrv.Close()

			d := NewMicrosoftDispatcher(
				fakeConnReader{conn: activeMicrosoftConn(goodMicrosoftCreds)}, identityEncryptor{},
				microsoft.WithTokenURL(tokenSrv.URL), microsoft.WithBaseURL(apiSrv.URL),
				microsoft.WithReportingBaseURL(apiSrv.URL),
			)
			camp := &model.Campaign{
				Platform:           model.ProviderMicrosoftAds,
				PlatformCampaignID: "999",
				Result:             json.RawMessage(tc.result),
			}
			_, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMicrosoftAds, camp,
				model.MetricsWindowLast7Days)
			if errors.Is(err, domain.ErrCampaignAccountMismatch) {
				t.Fatalf("this row does not prove a mismatch and must not be refused as one: %v", err)
			}
			mu.Lock()
			reached := submitted
			mu.Unlock()
			if !reached {
				t.Error("the read must proceed past the guard and submit the report request")
			}
		})
	}
}
