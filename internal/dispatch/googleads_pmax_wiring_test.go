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

// The dispatch-layer half of the Performance Max path, for the reason
// googleads_targeting_wiring_test.go states for the Search fields: the platform
// client's own tests prove CreatePerformanceMaxCampaign builds the right payload
// FROM a CampaignInput, and pmax_creative_test.go proves every bound; what neither
// can prove is that the wire config is mapped onto that input at all. A dropped
// `channel` or a `performanceMaxCreative` the mapper never reads leaves both suites
// green while the feature is unreachable through the API.
//
// The asset-group mutates themselves are NOT exercised from here, and deliberately:
// a non-empty asset group requires real image bytes, and the creative fetch's dial
// guard and TLS roots are relaxable only from inside the googleads package (see
// withImageDialGuard). Reaching those three mutates from dispatch would mean
// exporting a test-only hole in the one fetch that must never have one. The two
// facts this file can establish without it are the two that matter here — that the
// channel reaches the campaign shell, and that every creative list reaches the
// client's validator and then its fetch — and the cascade past that point is pinned
// in internal/platform/googleads/pmax_test.go.

// pmaxWireCreative is a complete asset group as it arrives on the wire, pointed at
// addresses the fetch is guaranteed to refuse. Private literals rather than a
// hostname: LookupIPAddr resolves an IP literal without a DNS query, so the refusal
// is the dial guard's and the test needs no network at all.
func pmaxWireCreative() map[string]any {
	return map[string]any{
		"marketingImages":       []string{"https://10.0.0.1/m.png"},
		"squareMarketingImages": []string{"https://10.0.0.1/sq.png"},
		"logoImages":            []string{"https://10.0.0.1/logo.png"},
		"headlines":             []string{"Join us at KubeCon", "Three days in Amsterdam", "Meet the maintainers"},
		"longHeadlines":         []string{"KubeCon + CloudNativeCon Europe 2026, Amsterdam"},
		"descriptions":          []string{"Talks, workshops and hallway track.", "Register now and save."},
		"businessName":          "Linux Foundation",
	}
}

func pmaxWireConfig(t *testing.T, creative map[string]any) json.RawMessage {
	t.Helper()
	inner := map[string]any{"budget": 50, "channel": "performance-max"}
	if creative != nil {
		inner["performanceMaxCreative"] = creative
	}
	raw, err := json.Marshal(map[string]any{"googleAdsConfig": inner})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return raw
}

// channel: "performance-max" must reach the kind switch, not merely be accepted and
// then created as a Search campaign. The campaign body is the only place that shows
// it — and the absence of an ad group and an ad is the other half, because
// Performance Max has neither and a cascade that grew one would still produce a
// campaign Google accepts.
func TestGoogleAds_PerformanceMaxChannelReachesTheCampaignShell(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, pmaxWireConfig(t, nil))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp == nil {
		t.Fatal("nil campaign on success")
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.campaigns) != 1 {
		t.Fatalf("got %d campaigns:mutate calls, want 1", len(cap.campaigns))
	}
	body := string(cap.campaigns[0])
	for _, want := range []string{`"advertisingChannelType":"PERFORMANCE_MAX"`, `"maximizeConversions"`} {
		if !strings.Contains(body, want) {
			t.Errorf("campaign body is missing %s — the wire channel did not reach the kind switch: %s", want, body)
		}
	}
	if strings.Contains(body, `"manualCpc"`) {
		t.Errorf("campaign body carries manualCpc; Performance Max refuses manual bidding: %s", body)
	}
	if len(cap.adGroups) != 0 || len(cap.adGroupAds) != 0 {
		t.Errorf("got %d adGroups and %d adGroupAds mutates; Performance Max has no ad group and no ad", len(cap.adGroups), len(cap.adGroupAds))
	}
}

// The creative sub-object: a complete asset group must get past every bound in the
// client's validator and reach the fetch, which then refuses the private address.
// That refusal is the proof the URL lists were carried — a mapper that dropped one
// of them would fail earlier, with that list's own "needs at least one" message,
// which is exactly what the table below pins per list.
func TestGoogleAds_PerformanceMaxCreativeConfigReachesTheImageFetch(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, pmaxWireConfig(t, pmaxWireCreative()))
	if err == nil {
		t.Fatal("expected the creative fetch to refuse the private address")
	}
	if !strings.Contains(err.Error(), "10.0.0.1") {
		t.Errorf("error does not name the refused address, so the fetch may never have been reached: %v", err)
	}
	if strings.Contains(err.Error(), "needs at least") {
		t.Errorf("the validator rejected a complete asset group, so a list was dropped between the wire and the client: %v", err)
	}
	if camp != nil {
		t.Errorf("nothing may be created for an input refused before the budget mutate, got %+v", camp)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.sawBudget {
		t.Error("the image fetch must run BEFORE the budget mutate — a later one strands a paid campaign")
	}
}

// Each wire list must land in its OWN client slot. Omitting one and asserting the
// error names that one is what distinguishes a correct mapper from one that merged
// two lists: with headlines and longHeadlines merged, dropping either would still
// leave the combined count satisfied and the campaign would create with the wrong
// asset field types.
func TestGoogleAds_EachPerformanceMaxListReachesItsOwnSlot(t *testing.T) {
	cases := map[string]struct {
		omit string
		want string
	}{
		"marketing images":        {"marketingImages", "needs at least one marketing image"},
		"square marketing images": {"squareMarketingImages", "needs at least one square marketing image"},
		"logo images":             {"logoImages", "needs at least 1 square logo"},
		"headlines":               {"headlines", "needs at least 3 headline"},
		"long headlines":          {"longHeadlines", "needs at least 1 long headline"},
		"descriptions":            {"descriptions", "needs at least 2 description"},
		"business name":           {"businessName", "requires a business name"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			opts, cap := targetingServers(t)
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
			creative := pmaxWireCreative()
			delete(creative, tc.omit)

			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, pmaxWireConfig(t, creative))
			if err == nil {
				t.Fatalf("expected %s to be required", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name the missing %s (%q)", err, name, tc.want)
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
