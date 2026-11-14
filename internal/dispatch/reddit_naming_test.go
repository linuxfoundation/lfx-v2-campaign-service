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

// redditNamingConfig is redditMultiCfg plus a redditConfig.naming value (raw JSON; "" omits the key).
func redditNamingConfig(naming string) json.RawMessage {
	extra := ""
	if naming != "" {
		extra = `,"naming":` + naming
	}
	return json.RawMessage(`{"redditConfig":{"budgetUsd":50,"startDate":"2099-08-01","endDate":"2099-08-31","objective":"traffic","subreddits":["kubernetes"]` + extra + `}}`)
}

const (
	redditLegacyDispatchName = "Events | KubeCon NA 2026 | NA | Traffic | Intent | Social | cncf | ToFU"
	redditV2DispatchName     = "Events | KubeCon NA 2026 | NA | Traffic | Intent | Static | Cloud Native Computing Foundation (CNCF) | ToFU"
)

// redditConfig.naming (#587) is a synchronous 400 for a wrong shape or a value outside its closed
// list whatever the gate says, and for a missing business unit while the gate is on.
func TestReddit_NamingPreflight(t *testing.T) {
	d := NewRedditDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, tc := range []struct {
		name, gate, naming, want string
	}{
		{"gate off, absent", "", "", ""},
		{"gate off, valid", "", `{"region":"LATAM","audienceIntent":"Retargeting","businessUnit":"Valkey"}`, ""},
		{"gate off, no business unit is fine", "", `{"region":"EMEA"}`, ""},
		{"gate off, bad region", "", `{"region":"Reddit"}`, "region"},
		{"gate off, slug as business unit", "", `{"businessUnit":"cncf"}`, "businessUnit"},
		{"unknown key", "", `{"regoin":"APAC"}`, "must be an object"},
		{"null", "", `null`, "must be an object"},
		{"non-string value", "", `{"businessUnit":true}`, "must be an object"},
		{"gate on, valid", "true", namingCNCF, ""},
		{"gate on, absent", "true", "", "businessUnit is required"},
		{"gate typo is off", "1", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvRedditNamingV2Enabled, tc.gate)
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, redditNamingConfig(tc.naming))
			if tc.want == "" {
				if !errors.Is(err, domain.ErrPreflightNotApplicable) {
					t.Fatalf("PreflightCreate = %v, want not applicable", err)
				}
				return
			}
			msg, pre := domain.AsPreflightRefusal(err)
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !pre || !strings.Contains(msg, "redditConfig.naming") || !strings.Contains(msg, tc.want) {
				t.Fatalf("PreflightCreate = %v, want a pre-flight 400 containing %q", err, tc.want)
			}
		})
	}
}

// Dispatch re-checks the same rules before any request (a path that skipped PreflightCreate).
func TestRedditDispatch_NamingRefusedBeforeAnyRequest(t *testing.T) {
	for _, tc := range []struct{ gate, naming string }{
		{"true", ""}, {"true", `{"region":"Reddit","businessUnit":"LF Events"}`},
		{"", `{"regoin":"APAC"}`}, {"", `{"region":"Reddit"}`},
	} {
		t.Setenv(constants.EnvRedditNamingV2Enabled, tc.gate)
		w := &adWire{}
		camp, err := w.redditDispatcher(t).Dispatch(slotCtx(model.FirstSlotVersion, &model.Campaign{}), testBrief(), model.ProviderRedditAds, redditNamingConfig(tc.naming))
		if _, pre := domain.AsPreflightRefusal(err); camp != nil || !pre || !errors.Is(err, domain.ErrCreateConfigInvalid) {
			t.Errorf("gate %q naming %s: Dispatch = %+v, %v; want a not-created config refusal", tc.gate, tc.naming, camp, err)
		}
		if names := w.names(); len(names) != 0 {
			t.Errorf("gate %q naming %s: campaigns created %q; want none", tc.gate, tc.naming, names)
		}
	}
}

// The gate decides the name: off, the legacy name even when naming is sent; on, the v2 name.
func TestRedditDispatch_NamingGateSelectsTheName(t *testing.T) {
	for _, tc := range []struct{ gate, want string }{{"", redditLegacyDispatchName}, {"true", redditV2DispatchName}} {
		t.Run("gate="+tc.gate, func(t *testing.T) {
			t.Setenv(constants.EnvRedditNamingV2Enabled, tc.gate)
			w := &adWire{}
			camp, err := w.redditDispatcher(t).Dispatch(slotCtx(model.FirstSlotVersion, &model.Campaign{}), testBrief(), model.ProviderRedditAds, redditNamingConfig(namingCNCF))
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if sent := w.names(); camp.CampaignName != tc.want || len(sent) != 1 || sent[0] != tc.want {
				t.Fatalf("CampaignName = %q, sent = %q; want %q", camp.CampaignName, sent, tc.want)
			}
		})
	}
}

// #587 retry safety: the name the claimed row records is sent verbatim, whatever the gate.
func TestRedditDispatch_FrozenNameWinsOverNamingV2(t *testing.T) {
	t.Setenv(constants.EnvRedditNamingV2Enabled, "true")
	w := &adWire{}
	ctx := model.WithDispatchCampaignName(slotCtx(model.FirstSlotVersion, &model.Campaign{}), redditLegacyDispatchName)
	camp, err := w.redditDispatcher(t).Dispatch(ctx, testBrief(), model.ProviderRedditAds, redditNamingConfig(namingCNCF))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if sent := w.names(); camp.CampaignName != redditLegacyDispatchName || len(sent) != 1 || sent[0] != redditLegacyDispatchName {
		t.Fatalf("CampaignName = %q, sent = %q; want the frozen %q", camp.CampaignName, sent, redditLegacyDispatchName)
	}
}
