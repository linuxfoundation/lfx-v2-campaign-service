// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// twitterNamingConfig is twitterMultiCfg plus a twitterConfig.naming value (raw JSON; "" omits the key).
func twitterNamingConfig(naming string) json.RawMessage {
	extra := ""
	if naming != "" {
		extra = `,"naming":` + naming
	}
	return json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetId":"1234567890"` + extra + `}}`)
}

const (
	xLegacyDispatchName = "Events | KubeCon NA 2026 | Global | Awareness | Prospecting | Promoted Post | cncf | MoFU"
	xV2DispatchName     = "Events | KubeCon NA 2026 | Global | Traffic | Prospecting | Social | Cloud Native Computing Foundation (CNCF) | MoFU"
)

// twitterConfig.naming (#587) is a synchronous 400 for a wrong shape or a value outside its closed
// list whatever the gate says, and for a missing business unit while the gate is on.
func TestTwitter_NamingPreflight(t *testing.T) {
	d := NewTwitterDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, tc := range []struct {
		name, gate, naming, want string
	}{
		{"gate off, absent", "", "", ""},
		{"gate off, valid", "", `{"region":"India","audienceIntent":"Intent","businessUnit":"LF Education"}`, ""},
		{"gate off, no business unit is fine", "", `{"region":"EMEA"}`, ""},
		{"gate off, bad region", "", `{"region":"Twitter"}`, "region"},
		{"gate off, slug as business unit", "", `{"businessUnit":"cncf"}`, "businessUnit"},
		{"unknown key", "", `{"regoin":"APAC"}`, "must be an object"},
		{"null", "", `null`, "must be an object"},
		{"non-object", "", `["NA"]`, "must be an object"},
		{"gate on, valid", "true", namingCNCF, ""},
		{"gate on, absent", "true", "", "businessUnit is required"},
		{"gate typo is off", "yes", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvTwitterNamingV2Enabled, tc.gate)
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterNamingConfig(tc.naming))
			if tc.want == "" {
				if !errors.Is(err, domain.ErrPreflightNotApplicable) {
					t.Fatalf("PreflightCreate = %v, want not applicable", err)
				}
				return
			}
			msg, pre := domain.AsPreflightRefusal(err)
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !pre || !strings.Contains(msg, "twitterConfig.naming") || !strings.Contains(msg, tc.want) {
				t.Fatalf("PreflightCreate = %v, want a pre-flight 400 containing %q", err, tc.want)
			}
		})
	}
}

// Dispatch re-checks the same rules before any request (a path that skipped PreflightCreate).
func TestTwitterDispatch_NamingRefusedBeforeAnyRequest(t *testing.T) {
	for _, tc := range []struct{ gate, naming string }{
		{"true", ""}, {"true", `{"region":"Twitter","businessUnit":"LF Events"}`},
		{"", `{"regoin":"APAC"}`}, {"", `{"region":"Twitter"}`},
	} {
		t.Setenv(constants.EnvTwitterNamingV2Enabled, tc.gate)
		w := &xWire{}
		camp, err := w.dispatcher(t).Dispatch(slotCtx(model.FirstSlotVersion, &model.Campaign{}), testBrief(), model.ProviderTwitterAds, twitterNamingConfig(tc.naming))
		if _, pre := domain.AsPreflightRefusal(err); camp != nil || !pre || !errors.Is(err, domain.ErrCreateConfigInvalid) {
			t.Errorf("gate %q naming %s: Dispatch = %+v, %v; want a not-created config refusal", tc.gate, tc.naming, camp, err)
		}
		if _, _, _, n := w.snapshot(); n != 0 {
			t.Errorf("gate %q naming %s: %d request(s) before the refusal; want 0", tc.gate, tc.naming, n)
		}
	}
}

// The gate decides the name: off, the legacy name even when naming is sent; on, the v2 name.
func TestTwitterDispatch_NamingGateSelectsTheName(t *testing.T) {
	for _, tc := range []struct{ gate, want string }{{"", xLegacyDispatchName}, {"true", xV2DispatchName}} {
		t.Run("gate="+tc.gate, func(t *testing.T) {
			t.Setenv(constants.EnvTwitterNamingV2Enabled, tc.gate)
			w := &xWire{}
			camp, err := w.dispatcher(t).Dispatch(slotCtx(model.FirstSlotVersion, &model.Campaign{}), testBrief(), model.ProviderTwitterAds, twitterNamingConfig(namingCNCF))
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if creates, _, _, _ := w.snapshot(); camp.CampaignName != tc.want || strings.Join(creates, ",") != tc.want {
				t.Fatalf("CampaignName = %q, creates = %q; want %q", camp.CampaignName, creates, tc.want)
			}
		})
	}
}

// #587 retry safety under v2, end to end: the account holds the campaign a legacy-named attempt
// created. Whether the slot's row still records that name (frozen) or is gone (nothing frozen),
// turning v2 on reuses that campaign under its legacy name and creates no campaign.
func TestTwitterDispatch_NamingV2NeverDuplicatesALegacyCampaign(t *testing.T) {
	t.Setenv(constants.EnvTwitterNamingV2Enabled, "true")
	for _, frozen := range []string{xLegacyDispatchName, ""} {
		t.Run("frozen="+frozen, func(t *testing.T) {
			w := &xWire{campaigns: []fakeCampaign{{"cmp_9", xLegacyDispatchName}}}
			ctx := model.WithDispatchCampaignName(slotCtx(model.FirstSlotVersion, &model.Campaign{}), frozen)
			camp, err := w.dispatcher(t).Dispatch(ctx, testBrief(), model.ProviderTwitterAds, twitterNamingConfig(namingCNCF))
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if creates, _, _, _ := w.snapshot(); camp.PlatformCampaignID != "cmp_9" || len(creates) != 0 || camp.CampaignName != xLegacyDispatchName {
				t.Fatalf("campaign = %q (%q), creates = %q; want cmp_9 reused under its legacy name and no campaign created", camp.PlatformCampaignID, camp.CampaignName, creates)
			}
		})
	}
}

// The frozen name also protects the opposite flip: a slot whose row records the v2 name, retried
// after the gate is turned back off, reuses the v2-named campaign instead of creating a legacy one.
func TestTwitterDispatch_FrozenV2NameSurvivesTheGateTurningOff(t *testing.T) {
	t.Setenv(constants.EnvTwitterNamingV2Enabled, "")
	w := &xWire{campaigns: []fakeCampaign{{"cmp_7", xV2DispatchName}}}
	ctx := model.WithDispatchCampaignName(slotCtx(model.FirstSlotVersion, &model.Campaign{}), xV2DispatchName)
	camp, err := w.dispatcher(t).Dispatch(ctx, testBrief(), model.ProviderTwitterAds, twitterNamingConfig(namingCNCF))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if creates, _, _, _ := w.snapshot(); camp.PlatformCampaignID != "cmp_7" || len(creates) != 0 {
		t.Fatalf("campaign = %q, creates = %q; want cmp_7 reused by its recorded name", camp.PlatformCampaignID, creates)
	}
}
