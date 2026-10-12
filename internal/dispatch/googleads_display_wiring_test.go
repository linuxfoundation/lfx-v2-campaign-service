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

// The dispatch-layer half of the Display path, for the reason
// googleads_targeting_wiring_test.go states for the Search fields: the platform
// client's own tests prove CreateDisplayCampaign builds the right payload FROM a
// CampaignInput, and display_creative_test.go proves every bound; what neither can
// prove is that the wire config is mapped onto that input at all. A dropped `channel`
// or a `displayCreative` the mapper never reads leaves both suites green while the
// feature is unreachable through the API.
//
// The ad mutate itself is NOT exercised from here, for the reason
// googleads_pmax_wiring_test.go gives: a Display creative requires real image bytes,
// and the creative fetch's dial guard and TLS roots are relaxable only from inside the
// googleads package (see withImageDialGuard). Reaching the ad from dispatch would mean
// exporting a test-only hole in the one fetch that must never have one. The two facts
// this file can establish without it are the two that matter here — that the channel
// reaches the campaign shell, and that every creative field reaches the client's
// validator and then its fetch — and the cascade past that point is pinned in
// internal/platform/googleads/display_test.go.

// displayWireCreative is a complete responsive display ad as it arrives on the wire,
// pointed at addresses the fetch is guaranteed to refuse. Private literals rather than a
// hostname: LookupIPAddr resolves an IP literal without a DNS query, so the refusal is
// the dial guard's and the test needs no network at all.
func displayWireCreative() map[string]any {
	return map[string]any{
		"marketingImages":       []string{"https://10.0.0.1/m.png"},
		"squareMarketingImages": []string{"https://10.0.0.1/sq.png"},
		"logoImages":            []string{"https://10.0.0.1/logo.png"},
		"squareLogoImages":      []string{"https://10.0.0.1/sqlogo.png"},
		"headlines":             []string{"Join us at KubeCon"},
		// A STRING, not a list — the one place this channel's wire shape departs from
		// Performance Max and Video, and the thing a copied-from-a-sibling mapper gets
		// wrong. Spelled here the way the config struct spells it so a change to either
		// breaks this test rather than silently dropping the field.
		"longHeadline":     "KubeCon + CloudNativeCon Europe 2026, Amsterdam",
		"descriptions":     []string{"Talks, workshops and hallway track."},
		"businessName":     "Linux Foundation",
		"callToActionText": "Register",
	}
}

func displayWireConfig(t *testing.T, creative map[string]any) json.RawMessage {
	t.Helper()
	inner := map[string]any{"budget": 50, "channel": "display"}
	if creative != nil {
		inner["displayCreative"] = creative
	}
	raw, err := json.Marshal(map[string]any{"googleAdsConfig": inner})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return raw
}

// channel: "display" must reach the kind switch, not merely be accepted and then created
// as a Search campaign. The campaign body is the only place that shows it — and the
// ABSENCE of advertisingChannelSubType is the other half, because Video is the sibling
// that pins one and a cascade that copied its shell would send VIDEO_ACTION on a DISPLAY
// campaign.
func TestGoogleAds_DisplayChannelReachesTheCampaignShell(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, displayWireConfig(t, nil))
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
	for _, want := range []string{`"advertisingChannelType":"DISPLAY"`, `"maximizeConversions"`} {
		if !strings.Contains(body, want) {
			t.Errorf("campaign body is missing %s — the wire channel did not reach the kind switch: %s", want, body)
		}
	}
	if strings.Contains(body, `"advertisingChannelSubType"`) {
		t.Errorf("campaign body pins a channel sub-type; a plain Display campaign takes none: %s", body)
	}
	if strings.Contains(body, `"manualCpc"`) {
		t.Errorf("campaign body carries manualCpc; this cascade bids conversions on Display: %s", body)
	}
	// One ad group and no ad: Display serves through an ad group like Search and Video,
	// and with no creative supplied the cascade stops before the ad.
	if len(cap.adGroups) != 1 {
		t.Errorf("got %d adGroups:mutate calls, want 1", len(cap.adGroups))
	}
	if len(cap.adGroupAds) != 0 {
		t.Errorf("got %d adGroupAds mutates for a campaign with no creative, want 0", len(cap.adGroupAds))
	}
}

// The creative sub-object: a complete responsive display ad must get past every bound in
// the client's validator and reach the fetch, which then refuses the private address.
// That refusal is the proof the fields were carried — a mapper that dropped one of them
// would fail earlier, with that field's own message, which is exactly what the table
// below pins per field.
func TestGoogleAds_DisplayCreativeConfigReachesTheImageFetch(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, displayWireConfig(t, displayWireCreative()))
	if err == nil {
		t.Fatal("expected the creative fetch to refuse the private address")
	}
	if !strings.Contains(err.Error(), "10.0.0.1") {
		t.Errorf("error does not name the refused address, so the fetch may never have been reached: %v", err)
	}
	if strings.Contains(err.Error(), "needs at least") || strings.Contains(err.Error(), "requires a") {
		t.Errorf("the validator rejected a complete creative, so a field was dropped between the wire and the client: %v", err)
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

// Each wire field must land in its OWN client slot. Omitting one and asserting the error
// names that one is what distinguishes a correct mapper from one that merged two fields:
// with headlines and the long headline merged, dropping either would still leave the
// combined count satisfied and the ad would carry the wrong asset field types.
//
// The two marketing arrays are omitted TOGETHER, because the requirement is reciprocal —
// either one alone satisfies it — and a table that omitted just one would pass against a
// mapper that never read that array at all.
func TestGoogleAds_EachDisplayFieldReachesItsOwnSlot(t *testing.T) {
	cases := map[string]struct {
		omit []string
		want string
	}{
		"marketing images": {
			[]string{"marketingImages", "squareMarketingImages"},
			"needs at least one marketing image or one square marketing image",
		},
		"headlines":     {[]string{"headlines"}, "needs at least 1 headline"},
		"long headline": {[]string{"longHeadline"}, "requires a long headline"},
		"descriptions":  {[]string{"descriptions"}, "needs at least 1 description"},
		"business name": {[]string{"businessName"}, "requires a business name"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			opts, cap := targetingServers(t)
			d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
			creative := displayWireCreative()
			for _, k := range tc.omit {
				delete(creative, k)
			}

			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, displayWireConfig(t, creative))
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
				t.Error("the refusal must happen BEFORE the budget mutate — a later one strands a paid budget")
			}
		})
	}
}

// The optional fields, and optional must mean optional. Both logo arrays and the
// call-to-action text are omitted at once: each is a field Demand Gen or the config
// struct's own neighbours make required, so a validator that borrowed a sibling's floor
// would refuse here instead of reaching the fetch. Reaching the fetch — the private
// address refusal — is the proof the input was accepted in full.
func TestGoogleAds_DisplayLogosAndCallToActionAreOptional(t *testing.T) {
	opts, _ := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	creative := displayWireCreative()
	for _, k := range []string{"logoImages", "squareLogoImages", "callToActionText"} {
		delete(creative, k)
	}

	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, displayWireConfig(t, creative))
	if err == nil {
		t.Fatal("expected the creative fetch to refuse the private address")
	}
	if !strings.Contains(err.Error(), "10.0.0.1") {
		t.Errorf("a creative with no logos and no call to action was refused before the fetch, so one of them is being treated as required: %v", err)
	}
}

// The recorded channel type is what the settings readback compares against
// campaign.advertising_channel_type. A snapshot that recorded "display" as SEARCH would
// report drift on every correctly-created Display campaign.
func TestGoogleAdsRecordedChannelType_Display(t *testing.T) {
	got := googleAdsRecordedChannelType(context.Background(), &model.Campaign{
		ConfigSnapshot: json.RawMessage(`{"channel":"display"}`),
	})
	if got == nil {
		t.Fatal("no recorded channel type for a snapshot that names the display channel")
	}
	if *got != googleAdsChannelTypeDisplay {
		t.Errorf("recorded channel type = %q, want %q", *got, googleAdsChannelTypeDisplay)
	}
}
