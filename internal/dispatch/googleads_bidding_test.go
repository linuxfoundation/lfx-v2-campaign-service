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

// The plumbing assertion for the bidding block, the sibling of
// TestGoogleAds_SearchConfigServingReadinessFieldsReachTheAPI: biddingStrategy /
// targetCpa / targetRoas / conversionActions in the dispatch config must reach the
// campaign create. A mapper that dropped one would otherwise create a campaign
// bidding by the channel default, which looks successful and spends against the
// wrong objective.
func TestGoogleAds_BiddingConfigFieldsReachCampaignCreate(t *testing.T) {
	opts, cap := servingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{
		"budget":50,
		"biddingStrategy":"target-cpa",
		"targetCpa":25,
		"conversionActions":["987654321","customers/1234567890/conversionActions/123456789"]
	}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	cop := servingCreate(t, cap.campaign)

	// target-cpa resolves to maximizeConversions with a target — the two caller
	// spellings collapse onto one proto strategy, and this is the assertion that the
	// resolution happened rather than the label reaching Google verbatim.
	if _, ok := cop["manualCpc"]; ok {
		t.Error("a configured strategy must replace the default manualCpc, not sit beside it")
	}
	mc, ok := cop["maximizeConversions"].(map[string]any)
	if !ok {
		t.Fatalf("maximizeConversions = %#v, want the resolved target-cpa strategy (create=%v)", cop["maximizeConversions"], cop)
	}
	// Google returns and accepts int64 proto fields as JSON strings; encoding/json
	// decodes the number we sent as a float64. Accept either spelling of 25.00 in
	// micros rather than pinning the wire type.
	if mc["targetCpaMicros"] != "25000000" && mc["targetCpaMicros"] != float64(25_000_000) {
		t.Errorf("targetCpaMicros = %#v, want 25000000 micros for a 25.00 target CPA", mc["targetCpaMicros"])
	}

	so, ok := cop["selectiveOptimization"].(map[string]any)
	if !ok {
		t.Fatalf("selectiveOptimization = %#v, want the configured conversion actions", cop["selectiveOptimization"])
	}
	actions, ok := so["conversionActions"].([]any)
	if !ok || len(actions) != 2 {
		t.Fatalf("conversionActions = %#v, want both configured actions", so["conversionActions"])
	}
	// Both caller spellings must arrive fully qualified: the bare id is the one the
	// mapper had to qualify, and sending it unqualified is rejected AFTER the budget
	// mutate.
	want := []string{
		"customers/1234567890/conversionActions/987654321",
		"customers/1234567890/conversionActions/123456789",
	}
	for i, w := range want {
		if actions[i] != w {
			t.Errorf("conversionActions[%d] = %#v, want %q", i, actions[i], w)
		}
	}
}

// With no bidding block configured the create must be byte-for-byte what it was
// before the strategy became selectable. This is the regression that matters most
// here: every campaign this service has ever created bid by manual CPC, and a
// default that drifted would silently re-bid them all.
func TestGoogleAds_NoBiddingConfigKeepsTheLegacyDefaults(t *testing.T) {
	for name, cfgJSON := range map[string]string{
		"search":     `{"googleAdsConfig":{"budget":50}}`,
		"demand gen": `{"googleAdsConfig":{"budget":50,"channel":"demand-gen"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			opts, cap := servingServers(t)
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
			if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, json.RawMessage(cfgJSON)); err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			cap.mu.Lock()
			defer cap.mu.Unlock()
			cop := servingCreate(t, cap.campaign)

			wantKey := "manualCpc"
			if name == "demand gen" {
				wantKey = "targetSpend"
			}
			got, ok := cop[wantKey].(map[string]any)
			if !ok {
				t.Fatalf("%s = %#v, want the legacy default strategy (create=%v)", wantKey, cop[wantKey], cop)
			}
			if len(got) != 0 {
				t.Errorf("%s = %#v, want an empty object — the legacy payload named the strategy and set no fields", wantKey, got)
			}
			// Exactly one strategy, and no conversion-selection field the legacy
			// payload never carried.
			for _, k := range []string{"manualCpc", "targetSpend", "maximizeConversions", "maximizeConversionValue"} {
				if _, present := cop[k]; present && k != wantKey {
					t.Errorf("create carries %s as well as %s — a campaign takes exactly one strategy", k, wantKey)
				}
			}
			if _, present := cop["selectiveOptimization"]; present {
				t.Error("create carries selectiveOptimization with no conversion actions configured")
			}
		})
	}
}

// maximize-conversion-value with a target ROAS, asserted separately because the
// ROAS is a RATIO and not micros — the one numeric field in this package that is
// not scaled, so a mapper reusing the micros conversion would be off by a million.
func TestGoogleAds_TargetROASReachesCampaignCreateAsARatio(t *testing.T) {
	opts, cap := servingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"biddingStrategy":"target-roas","targetRoas":4}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	cop := servingCreate(t, cap.campaign)
	mcv, ok := cop["maximizeConversionValue"].(map[string]any)
	if !ok {
		t.Fatalf("maximizeConversionValue = %#v, want the resolved target-roas strategy (create=%v)", cop["maximizeConversionValue"], cop)
	}
	if mcv["targetRoas"] != float64(4) {
		t.Errorf("targetRoas = %#v, want 4 — a ratio, not micros and not a percentage", mcv["targetRoas"])
	}
}

// A bad bidding input must be refused as a PRE-CREATE failure, the same contract
// the serving-readiness fields hold: the whole plan is validated in the pure
// preflight, so nothing upstream exists to reconcile. The channel mismatch and the
// conversion-actions-on-demand-gen cases are the ones worth pinning here — both are
// refusals Google would make only AFTER the budget mutate committed.
func TestGoogleAds_BadBiddingConfigIsPreCreate(t *testing.T) {
	// Each case pins a substring of the error it must produce, not merely that one
	// occurred. Every one of these configs is otherwise valid, so a mapping or
	// preflight change that broke them for an unrelated reason would still leave this
	// table green with nothing asserting the bidding guard ever ran.
	for name, tc := range map[string]struct {
		cfg  string
		want string
	}{
		"unknown strategy": {
			`{"googleAdsConfig":{"budget":50,"biddingStrategy":"maximise-clicks"}}`,
			`unknown bidding strategy "maximise-clicks"`,
		},
		"target the strategy cannot hold": {
			`{"googleAdsConfig":{"budget":50,"biddingStrategy":"maximize-clicks","targetCpa":25}}`,
			`a target CPA is not supported by the "maximize-clicks" bidding strategy`,
		},
		"target required but absent": {
			`{"googleAdsConfig":{"budget":50,"biddingStrategy":"target-roas"}}`,
			`requires a target ROAS`,
		},
		"cpc bid under an automated bid": {
			`{"googleAdsConfig":{"budget":50,"biddingStrategy":"maximize-conversions","cpcBid":2.5}}`,
			`a CPC bid is ignored by the "maximize-conversions" bidding strategy`,
		},
		"strategy not on this channel": {
			`{"googleAdsConfig":{"budget":50,"channel":"demand-gen","biddingStrategy":"target-cpa","targetCpa":25}}`,
			`bidding strategy "target-cpa" is not supported on`,
		},
		"conversion actions on demandgen": {
			`{"googleAdsConfig":{"budget":50,"channel":"demand-gen","conversionActions":["987654321"]}}`,
			`conversion actions are not supported on`,
		},
		"conversion action of another account": {
			`{"googleAdsConfig":{"budget":50,"conversionActions":["customers/9999999999/conversionActions/123456789"]}}`,
			`belongs to customer 9999999999, not the campaign's account 1234567890`,
		},
		"target out of range": {
			`{"googleAdsConfig":{"budget":50,"biddingStrategy":"target-roas","targetRoas":5000}}`,
			`target ROAS 5000 exceeds the maximum 1000`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			opts, cap := servingServers(t)
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, json.RawMessage(tc.cfg))
			if err == nil {
				t.Fatal("expected a dispatch error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
			cap.mu.Lock()
			defer cap.mu.Unlock()
			if len(cap.campaign) != 0 || len(cap.adGroup) != 0 {
				t.Error("a local input error must not reach any create mutate")
			}
		})
	}
}

// The mirror of every refusal above: what a correct caller sends must still be
// accepted on both channels. These guards run before the budget mutate, so an
// over-refusal here refuses a campaign Google would have created — the failure mode
// that costs more than any under-refusal.
func TestGoogleAds_SupportedBiddingConfigsAreAccepted(t *testing.T) {
	for name, cfgJSON := range map[string]string{
		"manual cpc with a bid":           `{"googleAdsConfig":{"budget":50,"biddingStrategy":"manual-cpc","cpcBid":2.5}}`,
		"maximize clicks":                 `{"googleAdsConfig":{"budget":50,"biddingStrategy":"maximize-clicks"}}`,
		"maximize conversions, no target": `{"googleAdsConfig":{"budget":50,"biddingStrategy":"maximize-conversions"}}`,
		"maximize conversion value":       `{"googleAdsConfig":{"budget":50,"biddingStrategy":"maximize-conversion-value"}}`,
		"demand gen maximize clicks":      `{"googleAdsConfig":{"budget":50,"channel":"demand-gen","biddingStrategy":"maximize-clicks"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			opts, cap := servingServers(t)
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
			if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, json.RawMessage(cfgJSON)); err != nil {
				t.Fatalf("refused a supported configuration: %v", err)
			}
			cap.mu.Lock()
			defer cap.mu.Unlock()
			if len(cap.campaign) == 0 {
				t.Fatal("no campaign create reached the API")
			}
		})
	}
}

// config_snapshot is persisted UNENCRYPTED, so every field added to googleAdsConfig
// has to be looked at for what it would leak. These four carry no URL and no
// credential — a strategy label, two numbers and a list of conversion-action ids
// scoped to the account — so they are snapshotted verbatim, and that decision is
// pinned here rather than left implicit. The snapshot sharing ConversionActions'
// backing array is safe only while nothing writes through it.
func TestGoogleAdsSnapshotConfig_KeepsTheBiddingBlockVerbatim(t *testing.T) {
	cfg := googleAdsConfig{
		Budget:            50,
		BiddingStrategy:   "target-cpa",
		TargetCPA:         25,
		TargetROAS:        4,
		ConversionActions: []string{"987654321"},
	}
	snapshot := googleAdsSnapshotConfig(cfg)
	if snapshot.BiddingStrategy != "target-cpa" || snapshot.TargetCPA != 25 || snapshot.TargetROAS != 4 {
		t.Errorf("snapshot bidding block = %q/%v/%v, want the configured values verbatim", snapshot.BiddingStrategy, snapshot.TargetCPA, snapshot.TargetROAS)
	}
	if len(snapshot.ConversionActions) != 1 || snapshot.ConversionActions[0] != "987654321" {
		t.Errorf("snapshot conversionActions = %v, want the configured ids", snapshot.ConversionActions)
	}
	// Nothing in the block looks like a URL, which is what would need sanitizing.
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if strings.Contains(string(raw), "://") {
		t.Errorf("snapshot carries a URL from the bidding block: %s", raw)
	}
}
