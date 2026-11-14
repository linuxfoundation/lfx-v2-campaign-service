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

// linkedInNamingConfig is linkedInMultiCfg plus a linkedInConfig.naming value (raw JSON; "" omits
// the key).
func linkedInNamingConfig(naming string) json.RawMessage {
	extra := ""
	if naming != "" {
		extra = `,"naming":` + naming
	}
	return json.RawMessage(`{"linkedInConfig":{
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"]}],
		"variants":[{"introText":"Join us — it's great and long enough","headline":"KubeCon 2099"}]` + extra + `
	}}`)
}

const (
	liLegacyName = "Events | KubeCon NA 2026 | LinkedIn | Traffic | Prospecting | Static | cncf | MoFU"
	liV2Name     = "Events | KubeCon NA 2026 | NA | Traffic | Prospecting | Static | Cloud Native Computing Foundation (CNCF) | MoFU"
	liCNCF       = `{"businessUnit":"Cloud Native Computing Foundation (CNCF)"}`
)

// linkedInConfig.naming (#587) is a synchronous 400 for a wrong shape or a value outside its closed
// list whatever the gate says, and for a missing business unit while the gate is on.
func TestLinkedIn_NamingPreflight(t *testing.T) {
	d := NewLinkedInDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, tc := range []struct {
		name, gate, naming, want string
	}{
		{"gate off, absent", "", "", ""},
		{"gate off, valid", "", `{"region":"APAC","audienceIntent":"Intent","businessUnit":"LF Events"}`, ""},
		{"gate off, no business unit is fine", "", `{"region":"EMEA"}`, ""},
		{"gate off, bad region", "", `{"region":"LinkedIn"}`, "region"},
		{"gate off, bad intent", "", `{"audienceIntent":"Lookalike"}`, "audienceIntent"},
		{"gate off, slug as business unit", "", `{"businessUnit":"proj-kubecon-na-2026"}`, "businessUnit"},
		{"unknown key", "", `{"regoin":"APAC"}`, "must be an object"},
		{"null", "", `null`, "must be an object"},
		{"not an object", "", `"APAC"`, "must be an object"},
		{"non-string value", "", `{"region":1}`, "must be an object"},
		{"gate on, valid", "true", liCNCF, ""},
		{"gate on, absent", "true", "", "businessUnit is required"},
		{"gate on, no business unit", "true", `{"region":"NA"}`, "businessUnit is required"},
		{"gate typo is off", "TRUE", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvLinkedInAdsNamingV2Enabled, tc.gate)
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, linkedInNamingConfig(tc.naming))
			if tc.want == "" {
				if !errors.Is(err, domain.ErrPreflightNotApplicable) {
					t.Fatalf("PreflightCreate = %v, want not applicable", err)
				}
				return
			}
			msg, pre := domain.AsPreflightRefusal(err)
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !pre || !strings.Contains(msg, "linkedInConfig.naming") || !strings.Contains(msg, tc.want) {
				t.Fatalf("PreflightCreate = %v, want a pre-flight 400 containing %q", err, tc.want)
			}
		})
	}
}

// The gate decides the name: off, the legacy name even when naming is sent; on, the v2 name.
func TestLinkedInDispatch_NamingGateSelectsTheName(t *testing.T) {
	for _, tc := range []struct{ gate, want string }{{"", liLegacyName}, {"true", liV2Name}} {
		t.Run("gate="+tc.gate, func(t *testing.T) {
			t.Setenv(constants.EnvLinkedInAdsNamingV2Enabled, tc.gate)
			w := &liWire{}
			camp, err := w.dispatcher(t).Dispatch(slotCtx(model.FirstSlotVersion, &model.Campaign{}), testBrief(), model.ProviderLinkedInAds, linkedInNamingConfig(liCNCF))
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			creates, _, _ := w.snapshot()
			if camp.CampaignName != tc.want || strings.Join(creates, ",") != tc.want {
				t.Fatalf("CampaignName = %q, creates = %q; want %q", camp.CampaignName, creates, tc.want)
			}
		})
	}
}

// #587 retry safety under v2, end to end: the event's group holds the campaign a legacy-named
// attempt created. Whether the slot's row still records that name (frozen) or is gone (nothing
// frozen), turning v2 on reuses that campaign under its legacy name and creates nothing.
func TestLinkedInDispatch_NamingV2NeverDuplicatesALegacyCampaign(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsNamingV2Enabled, "true")
	for _, frozen := range []string{liLegacyName, ""} {
		t.Run("frozen="+frozen, func(t *testing.T) {
			w := &liWire{
				groups:    []fakeCampaign{{"100", "Events | KubeCon NA 2026 | cncf"}},
				campaigns: []fakeCampaign{{"200", liLegacyName}},
			}
			ctx := model.WithDispatchCampaignName(slotCtx(model.FirstSlotVersion, &model.Campaign{}), frozen)
			camp, err := w.dispatcher(t).Dispatch(ctx, testBrief(), model.ProviderLinkedInAds, linkedInNamingConfig(liCNCF))
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			creates, _, _ := w.snapshot()
			if camp.PlatformCampaignID != "200" || len(creates) != 0 || camp.CampaignName != liLegacyName {
				t.Fatalf("campaign = %q (%q), creates = %q; want the legacy 200 reused under its name and nothing created", camp.PlatformCampaignID, camp.CampaignName, creates)
			}
		})
	}
}
