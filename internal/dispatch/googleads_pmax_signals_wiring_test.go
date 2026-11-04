// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// The dispatch half of #461: that performanceMaxSearchThemes, performanceMaxAudienceSignal
// and the extension fields on a Performance Max config reach the client at all. The signal
// mutate itself needs an asset group, which needs fetched image bytes — out of reach from
// here for the reason googleads_pmax_wiring_test.go gives — so each signal field is proved
// mapped by a refusal only the client's preflight can produce from it, and that cascade is
// pinned in internal/platform/googleads/pmax_signals_test.go.
//
// performanceMaxAudienceSignal is behind GOOGLE_ADS_PMAX_AUDIENCE_SIGNAL_ENABLED, so
// the mapping test turns the gate ON to reach the client's own refusals; the gate itself is
// TestGoogleAds_PerformanceMaxAudienceSignalGate.

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
	t.Setenv(constants.EnvGoogleAdsPMaxAudienceSignalEnabled, "true")
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
//
// The audience-signal gate is OFF here: the extensions are not gated.
func TestGoogleAds_PerformanceMaxExtensionsReachTheCampaign(t *testing.T) {
	setGateEnv(t, constants.EnvGoogleAdsPMaxAudienceSignalEnabled, unsetEnv)
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

// With GOOGLE_ADS_PMAX_AUDIENCE_SIGNAL_ENABLED anything but exactly "true", a create naming a
// performanceMaxAudienceSignal is refused in Dispatch before the budget, even one the client
// would accept (the client checks the account only), as not-created and with the
// gate's own sentence as the job's message.
func TestGoogleAds_PerformanceMaxAudienceSignalGate(t *testing.T) {
	valid := map[string]any{
		"performanceMaxCreative":       pmaxWireCreative(),
		"performanceMaxAudienceSignal": "4242",
	}
	cases := []struct {
		name, env, channel string
		extra              map[string]any
	}{
		{"unset", unsetEnv, "performance-max", valid},
		{"empty", "", "performance-max", valid},
		{"false", "false", "performance-max", valid},
		{"wrong case", "TRUE", "performance-max", valid},
		{"leading space", " true", "performance-max", valid},
		{"one", "1", "performance-max", valid},
		{"blank audience", "", "performance-max", map[string]any{
			"performanceMaxCreative":       pmaxWireCreative(),
			"performanceMaxAudienceSignal": "  ",
		}},
		{"off performance max", "", "search",
			map[string]any{"performanceMaxAudienceSignal": "4242"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setGateEnv(t, constants.EnvGoogleAdsPMaxAudienceSignalEnabled, tc.env)
			opts, cap := targetingServers(t)
			conn := fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}
			d := NewGoogleAdsDispatcher(conn, identityEncryptor{}, opts...)
			cfg := pmaxSignalsConfig(t, tc.channel, tc.extra)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
			if err == nil {
				t.Fatal("expected the dispatch to be refused while the gate is off")
			}
			for _, want := range []string{
				"performanceMaxAudienceSignal is disabled on this deployment",
				"omit performanceMaxAudienceSignal",
				"or ask an operator to enable " + constants.EnvGoogleAdsPMaxAudienceSignalEnabled,
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to contain %q", err, want)
				}
			}
			var pce *preCreateError
			if !errors.As(err, &pce) {
				t.Errorf("error = %v, want a not-created error so the claim is released", err)
			}
			got := service.DispatchFailureMessage(model.ProviderGoogleAds, err, true)
			if got != err.Error() {
				t.Errorf("job message = %q, want the gate's own sentence %q", got, err.Error())
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

	// Exactly "true" lets the same audience through: it passes the client's checks and the
	// create goes on to the image fetch, which refuses the private address before the budget.
	t.Run("exactly true", func(t *testing.T) {
		setGateEnv(t, constants.EnvGoogleAdsPMaxAudienceSignalEnabled, "true")
		assertPMaxReachesTheImageFetch(t, valid)
	})
}

// The gate leaves the rest of #461 alone: with it off, search themes pass the client's checks
// and reach the image fetch. The extensions are
// TestGoogleAds_PerformanceMaxExtensionsReachTheCampaign, which runs with the gate off too.
func TestGoogleAds_PerformanceMaxAudienceSignalGateLeavesThemesAlone(t *testing.T) {
	setGateEnv(t, constants.EnvGoogleAdsPMaxAudienceSignalEnabled, unsetEnv)
	assertPMaxReachesTheImageFetch(t, map[string]any{
		"performanceMaxCreative":     pmaxWireCreative(),
		"performanceMaxSearchThemes": []string{"kubecon", "kubecon agenda"},
	})
}

// assertPMaxReachesTheImageFetch dispatches a Performance Max config whose creative names
// private image addresses and requires the refusal to be the image fetch's: every check
// before it, the gate's and the client's, passed.
func assertPMaxReachesTheImageFetch(t *testing.T, extra map[string]any) {
	t.Helper()
	opts, cap := targetingServers(t)
	conn := fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}
	d := NewGoogleAdsDispatcher(conn, identityEncryptor{}, opts...)
	cfg := pmaxSignalsConfig(t, "performance-max", extra)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
	if err == nil || !strings.Contains(err.Error(), "10.0.0.1") {
		t.Fatalf("err = %v, want the image fetch's refusal of 10.0.0.1", err)
	}
	if strings.Contains(err.Error(), "disabled on this deployment") {
		t.Errorf("err = %v, want no gate refusal", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.sawBudget {
		t.Error("the image fetch must run BEFORE the budget mutate")
	}
}
