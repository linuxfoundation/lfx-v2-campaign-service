// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
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
		"callToActionText": "Learn more",
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
// The two marketing arrays are omitted ONE AT A TIME, because Google requires each shape
// on its own (#437): a mapper that dropped either array is now refused by name, where the
// old reciprocal rule let the other array cover for it.
func TestGoogleAds_EachDisplayFieldReachesItsOwnSlot(t *testing.T) {
	cases := map[string]struct {
		omit []string
		want string
	}{
		"marketing images":        {[]string{"marketingImages"}, "needs at least one marketing image (1.91:1)"},
		"square marketing images": {[]string{"squareMarketingImages"}, "needs at least one square marketing image (1:1)"},
		"headlines":               {[]string{"headlines"}, "needs at least 1 headline"},
		"long headline":           {[]string{"longHeadline"}, "requires a long headline"},
		"descriptions":            {[]string{"descriptions"}, "needs at least 1 description"},
		"business name":           {[]string{"businessName"}, "requires a business name"},
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

// The #462 fields: a Display config's four audience lists must reach the ad group's
// criteria mutate, each in its own oneof arm, and its placement exclusions the campaign's
// criteria mutate as negative placements. A mapper that dropped any of the five leaves the
// client's own tests green and the field unreachable through the API. The deployment gate is
// ON here; the gate itself is TestGoogleAds_DisplayAudiencesGate.
func TestGoogleAds_DisplayAudiencesAndPlacementExclusionsReachTheCriteria(t *testing.T) {
	t.Setenv(constants.EnvGoogleAdsDisplayAudiencesEnabled, "true")
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"display",
		"audienceSegments":["customers/1234567890/userLists/11"],
		"customAudiences":["customers/1234567890/customAudiences/22"],
		"customAffinities":["customers/1234567890/customInterests/33"],
		"userInterests":["customers/1234567890/userInterests/44"],
		"excludedPlacements":["example.com"]}}`)

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.adGroupCriteria) != 1 {
		t.Fatalf("got %d adGroupCriteria mutates, want 1", len(cap.adGroupCriteria))
	}
	audiences := string(cap.adGroupCriteria[0])
	for _, want := range []string{
		`"userList":{"userList":"customers/1234567890/userLists/11"}`,
		`"customAudience":{"customAudience":"customers/1234567890/customAudiences/22"}`,
		`"customAffinity":{"customAffinity":"customers/1234567890/customInterests/33"}`,
		`"userInterest":{"userInterestCategory":"customers/1234567890/userInterests/44"}`,
	} {
		if !strings.Contains(audiences, want) {
			t.Errorf("audience mutate lacks %s: %s", want, audiences)
		}
	}
	if len(cap.campaignCriteria) != 1 {
		t.Fatalf("got %d campaignCriteria mutates, want 1 (the placement exclusions)", len(cap.campaignCriteria))
	}
	if placements := string(cap.campaignCriteria[0]); !strings.Contains(placements, `"negative":true,"placement":{"url":"example.com"}`) {
		t.Errorf("placement mutate = %s, want a negative placement for example.com", placements)
	}
}

// Off Display the new fields are refused before anything is created — the dispatch half of
// the client's refusal, proving the field reaches the preflight on the Search channel too.
func TestGoogleAds_ExcludedPlacementsRefusedOnSearch(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"excludedPlacements":["example.com"]}}`)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
	if err == nil || !strings.Contains(err.Error(), "placement exclusions are not supported on Search Campaign") {
		t.Fatalf("err = %v, want the Search refusal", err)
	}
	if camp != nil {
		t.Errorf("camp = %+v, want nil — nothing was created", camp)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if cap.sawBudget {
		t.Error("a budget mutate was sent for a config the preflight must refuse")
	}
}

// With GOOGLE_ADS_DISPLAY_AUDIENCES_ENABLED anything but exactly "true", a create naming an
// account-owned Display segment is refused in Dispatch before the budget, even one the client
// would accept (the client checks the account only). The refusal is a
// not-created PreflightRefusal, so the async job shows the gate's sentence verbatim rather than
// the generic failure, and it names exactly the fields the request set.
func TestGoogleAds_DisplayAudiencesGate(t *testing.T) {
	const (
		userList = `"audienceSegments":["customers/1234567890/userLists/11"]`
		custom   = `"customAudiences":["customers/1234567890/customAudiences/22"]`
		affinity = `"customAffinities":["customers/1234567890/customInterests/33"]`
	)
	cases := []struct {
		name, env, cfg, wantFields string
		wantFull                   string // when set, the whole sentence, exactly
	}{
		{"unset", unsetEnv, `"channel":"display",` + userList, "(audienceSegments)", ""},
		{"empty", "", `"channel":"display",` + custom, "(customAudiences)", ""},
		{"false", "false", `"channel":"display",` + affinity, "(customAffinities)", ""},
		{"wrong case", "TRUE", `"channel":"display",` + userList, "(audienceSegments)", ""},
		{"leading space", " true", `"channel":"display",` + userList, "(audienceSegments)", ""},
		{"one", "1", `"channel":"display",` + custom, "(customAudiences)", ""},
		{"channel spelled loosely", "", `"channel":" Display ",` + userList, "(audienceSegments)", ""},
		{"all three", "", `"channel":"display",` + userList + `,` + custom + `,` + affinity,
			"(audienceSegments, customAudiences, customAffinities)", ""},
		{"blank entry", "", `"channel":"display","customAudiences":["  "]`, "(customAudiences)", ""},
		{"custom list off display", "", custom, "(customAudiences)", ""},
		// Two fields are joined by ", ", so the remedies are split by ";" and cannot read as
		// one more field in the list.
		{"two fields", "", `"channel":"display",` + userList + `,` + custom,
			"(audienceSegments, customAudiences)",
			`google-ads: Display audience targeting (audienceSegments, customAudiences) is disabled ` +
				`on this deployment (GOOGLE_ADS_DISPLAY_AUDIENCES_ENABLED is not "true"); ` +
				`omit audienceSegments, customAudiences; or ask an operator to set ` +
				`GOOGLE_ADS_DISPLAY_AUDIENCES_ENABLED to "true"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setGateEnv(t, constants.EnvGoogleAdsDisplayAudiencesEnabled, tc.env)
			opts, cap := targetingServers(t)
			conn := fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}
			d := NewGoogleAdsDispatcher(conn, identityEncryptor{}, opts...)
			cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,` + tc.cfg + `}}`)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
			if err == nil {
				t.Fatal("expected the dispatch to be refused while the gate is off")
			}
			for _, want := range []string{
				"Display audience targeting " + tc.wantFields + " is disabled on this deployment",
				"omit " + strings.Trim(tc.wantFields, "()"),
				"; or ask an operator to set " + constants.EnvGoogleAdsDisplayAudiencesEnabled + ` to "true"`,
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to contain %q", err, want)
				}
			}
			if tc.wantFull != "" && err.Error() != tc.wantFull {
				t.Errorf("error = %q, want exactly %q", err.Error(), tc.wantFull)
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
}

// What the gate does NOT cover, with it off: Google's own userInterests categories and the
// public-domain excludedPlacements on Display, and Search's observation-only audienceSegments.
// Each still reaches its criteria mutate.
func TestGoogleAds_DisplayAudiencesGateLeavesUngatedInputsAlone(t *testing.T) {
	cases := []struct {
		name, cfg, want string
	}{
		{"user interests", `"channel":"display",` +
			`"userInterests":["customers/1234567890/userInterests/44"]`,
			`"userInterestCategory":"customers/1234567890/userInterests/44"`},
		{"placements", `"channel":"display","excludedPlacements":["example.com"]`,
			`"placement":{"url":"example.com"}`},
		{"search user list", `"audienceSegments":["customers/1234567890/userLists/11"],` +
			`"keywords":[{"text":"kubecon","matchType":"PHRASE"}]`,
			`"userList":{"userList":"customers/1234567890/userLists/11"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvGoogleAdsDisplayAudiencesEnabled, "")
			opts, cap := targetingServers(t)
			conn := fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}
			d := NewGoogleAdsDispatcher(conn, identityEncryptor{}, opts...)
			cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,` + tc.cfg + `}}`)
			_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			cap.mu.Lock()
			defer cap.mu.Unlock()
			var bodies [][]byte
			bodies = append(bodies, cap.campaignCriteria...)
			bodies = append(bodies, cap.adGroupCriteria...)
			all := string(bytes.Join(bodies, nil))
			if !strings.Contains(all, tc.want) {
				t.Errorf("criteria mutates lack %s: campaign %q, ad group %q", tc.want,
					cap.campaignCriteria, cap.adGroupCriteria)
			}
		})
	}
}

// Exactly "true" opens the gate: the same three account-owned lists reach the ad group's
// criteria mutate.
func TestGoogleAds_DisplayAudiencesGateOpensOnExactlyTrue(t *testing.T) {
	t.Setenv(constants.EnvGoogleAdsDisplayAudiencesEnabled, "true")
	opts, cap := targetingServers(t)
	conn := fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}
	d := NewGoogleAdsDispatcher(conn, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"channel":"display",
		"audienceSegments":["customers/1234567890/userLists/11"],
		"customAudiences":["customers/1234567890/customAudiences/22"],
		"customAffinities":["customers/1234567890/customInterests/33"]}}`)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.adGroupCriteria) != 1 {
		t.Fatalf("got %d adGroupCriteria mutates, want 1", len(cap.adGroupCriteria))
	}
	for _, want := range []string{`"userList":`, `"customAudience":`, `"customAffinity":`} {
		if !strings.Contains(string(cap.adGroupCriteria[0]), want) {
			t.Errorf("audience mutate lacks %s: %s", want, cap.adGroupCriteria[0])
		}
	}
}

// unsetEnv tells setGateEnv to leave the variable UNSET rather than set to "", which
// t.Setenv alone cannot express.
const unsetEnv = "\x00unset"

// setGateEnv sets key to value for the test, or unsets it for unsetEnv; either way t.Setenv's
// cleanup restores the original.
func setGateEnv(t *testing.T, key, value string) {
	t.Helper()
	t.Setenv(key, "")
	if value == unsetEnv {
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
		return
	}
	t.Setenv(key, value)
}
