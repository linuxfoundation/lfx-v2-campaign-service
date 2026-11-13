// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// metaInsightsServer answers the Insights read with body, recording each query.
func metaInsightsServer(t *testing.T, body string) (*MetaDispatcher, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{}, meta.WithBaseURL(srv.URL))
	return d, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), queries...) }
}

const metaInsightsWithActions = `{"data":[{"impressions":"1000","clicks":"40","spend":"25.00",` +
	`"actions":[{"action_type":"offsite_conversion.fb_pixel_complete_registration","value":"7"},` +
	`{"action_type":"offsite_conversion.fb_pixel_purchase","value":"12"},{"action_type":"link_click","value":"40"}]}]}`

func metaCampaignWithEvent(event string) *model.Campaign {
	camp := &model.Campaign{Platform: model.ProviderMetaAds, PlatformCampaignID: "777"}
	if event != "" {
		camp.Result = []byte(`{"CampaignID":"777","ConversionEvent":"` + event + `"}`)
	}
	return camp
}

// On, a campaign whose create result records COMPLETE_REGISTRATION reports the count of that
// action type, not purchases or clicks, with the read asking for actions.
func TestMeta_ReadMetrics_ConversionsFromTheRecordedEvent(t *testing.T) {
	t.Setenv(constants.EnvMetaConversionsMetricsEnabled, "true")
	d, queries := metaInsightsServer(t, metaInsightsWithActions)
	m, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMetaAds, metaCampaignWithEvent("COMPLETE_REGISTRATION"), model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("ReadMetrics: %v", err)
	}
	if m.Conversions == nil || *m.Conversions != 7 || m.ResolvedConversionsStatus() != model.ConversionsReported {
		t.Fatalf("conversions = %v status = %q, want 7 reported", m.Conversions, m.ResolvedConversionsStatus())
	}
	if q := queries(); len(q) != 1 || !strings.Contains(q[0], "fields=impressions,clicks,spend,actions") {
		t.Errorf("queries = %v, want actions requested", queries())
	}
}

// Off, or with no recorded event, the read is exactly today's: no actions requested and
// conversions unavailable, whatever the response carries.
func TestMeta_ReadMetrics_ConversionsUnavailableWithoutFlagOrEvent(t *testing.T) {
	for name, tc := range map[string]struct {
		flag, event string
	}{
		"flag off":      {"", "COMPLETE_REGISTRATION"},
		"flag not true": {"TRUE", "PURCHASE"},
		"no event":      {"true", ""},
		"unknown event": {"true", "ADD_TO_CART"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(constants.EnvMetaConversionsMetricsEnabled, tc.flag)
			d, queries := metaInsightsServer(t, metaInsightsWithActions)
			m, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMetaAds, metaCampaignWithEvent(tc.event), model.MetricsWindowLast30Days)
			if err != nil {
				t.Fatalf("ReadMetrics: %v", err)
			}
			if m.Conversions != nil || m.ConversionsStatus != model.ConversionsUnavailable {
				t.Fatalf("conversions = %v status = %q, want nil unavailable", m.Conversions, m.ConversionsStatus)
			}
			for _, q := range queries() {
				if strings.Contains(q, "actions") {
					t.Errorf("actions requested: %s", q)
				}
			}
		})
	}
}

// A window with no clicks and no matching action is no_conversions, not a reported zero.
func TestMeta_ReadMetrics_ZeroClicksZeroConversionsIsNoConversions(t *testing.T) {
	t.Setenv(constants.EnvMetaConversionsMetricsEnabled, "true")
	d, _ := metaInsightsServer(t, `{"data":[{"impressions":"10","clicks":"0","spend":"0.10"}]}`)
	m, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMetaAds, metaCampaignWithEvent("LEAD"), model.MetricsWindowLast30Days)
	if err != nil {
		t.Fatalf("ReadMetrics: %v", err)
	}
	if m.ResolvedConversionsStatus() != model.ConversionsNoConversions || m.ReportedConversions() != nil {
		t.Fatalf("status = %q reported = %v, want no_conversions with no total", m.ResolvedConversionsStatus(), m.ReportedConversions())
	}
}
