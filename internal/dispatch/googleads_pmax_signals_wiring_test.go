// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// The dispatch half of #461: that performanceMaxSearchThemes, performanceMaxAudienceSignal
// and the extension fields on a Performance Max config reach the client at all. The signal
// mutate itself needs an asset group, which needs fetched image bytes — out of reach from
// here for the reason googleads_pmax_wiring_test.go gives — so each signal field is proved
// mapped by a refusal only the client's preflight can produce from it, and that cascade is
// pinned in internal/platform/googleads/pmax_signals_test.go.

func pmaxSignalsConfig(t *testing.T, channel string, extra map[string]any) json.RawMessage {
	t.Helper()
	inner := map[string]any{"budget": 50, "channel": channel}
	for k, v := range extra {
		inner[k] = v
	}
	raw, err := json.Marshal(map[string]any{"googleAdsConfig": inner})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return raw
}

func TestGoogleAds_PerformanceMaxSignalFieldsReachTheClient(t *testing.T) {
	for name, tc := range map[string]struct {
		channel string
		extra   map[string]any
		want    string
	}{
		// No creative, so no asset group: only a mapped theme list can produce this.
		"themes without an asset group": {
			channel: "performance-max",
			extra:   map[string]any{"performanceMaxSearchThemes": []string{"kubecon"}},
			want:    "this request creates none (no PerformanceMaxCreative)",
		},
		"audience without an asset group": {
			channel: "performance-max",
			extra:   map[string]any{"performanceMaxAudienceSignal": "4242"},
			want:    "this request creates none (no PerformanceMaxCreative)",
		},
		// With a complete creative the signals are checked before the image fetch, so a
		// foreign audience is refused by name rather than by the fetch.
		"foreign audience": {
			channel: "performance-max",
			extra: map[string]any{
				"performanceMaxCreative":       pmaxWireCreative(),
				"performanceMaxAudienceSignal": "customers/9999999999/audiences/5",
			},
			want: "belongs to customer 9999999999",
		},
		"over-long theme": {
			channel: "performance-max",
			extra: map[string]any{
				"performanceMaxCreative":     pmaxWireCreative(),
				"performanceMaxSearchThemes": []string{strings.Repeat("k", 81)},
			},
			want: "exceeding the 80 limit",
		},
		"themes on search": {
			channel: "search",
			extra:   map[string]any{"performanceMaxSearchThemes": []string{"kubecon"}},
			want:    "search themes and audience signals are not supported on Search Campaign",
		},
		"audience on demand gen": {
			channel: "demand-gen",
			extra:   map[string]any{"performanceMaxAudienceSignal": "4242"},
			want:    "search themes and audience signals are not supported on DemandGen Campaign",
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts, cap := targetingServers(t)
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, pmaxSignalsConfig(t, tc.channel, tc.extra))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if camp != nil {
				t.Errorf("nothing may be created for a refused input, got %+v", camp)
			}
			cap.mu.Lock()
			defer cap.mu.Unlock()
			if cap.sawBudget {
				t.Error("the refusal must happen BEFORE the budget mutate")
			}
		})
	}
}

// Sitelinks, callouts and structured snippets on a Performance Max config reach the
// campaign and are linked to it at the campaign level (#461); a call extension on the
// same channel is refused before anything is created.
func TestGoogleAds_PerformanceMaxExtensionsReachTheCampaign(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	extra := map[string]any{
		"sitelinks":          []map[string]any{{"text": "Register now", "finalUrl": "https://events.example.org/register"}},
		"callouts":           []string{"Free to attend"},
		"structuredSnippets": []map[string]any{{"header": "Brands", "values": []string{"Kubernetes", "Prometheus", "Envoy"}}},
	}
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, pmaxSignalsConfig(t, "performance-max", extra)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	cap.mu.Lock()
	links := string(cap.campaignAssets)
	campaigns := len(cap.campaigns)
	var campaignBody string
	if campaigns > 0 {
		campaignBody = string(cap.campaigns[0])
	}
	cap.mu.Unlock()
	if !strings.Contains(campaignBody, `"advertisingChannelType":"PERFORMANCE_MAX"`) {
		t.Fatalf("campaign create = %s, want a Performance Max campaign", campaignBody)
	}
	for _, ft := range []string{`"fieldType":"SITELINK"`, `"fieldType":"CALLOUT"`, `"fieldType":"STRUCTURED_SNIPPET"`} {
		if strings.Count(links, ft) != 1 {
			t.Errorf("campaignAssets body = %s, want exactly one %s", links, ft)
		}
	}
	if strings.Count(links, `"campaign":"customers/1234567890/campaigns/222"`) != 3 {
		t.Errorf("campaignAssets body = %s, want every link on the created campaign", links)
	}

	opts, cap = targetingServers(t)
	d = NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	call := map[string]any{"callExtensions": []map[string]any{{"countryCode": "US", "phoneNumber": "+1 415 555 0100"}}}
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, pmaxSignalsConfig(t, "performance-max", call))
	if err == nil || !strings.Contains(err.Error(), "supported on Search campaigns only, not on PerformanceMax Campaign") {
		t.Errorf("err = %v, want call extensions refused on Performance Max", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.sawBudget {
		t.Error("the refusal must happen BEFORE the budget mutate")
	}
}
