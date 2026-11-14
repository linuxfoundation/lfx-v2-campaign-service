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

// metaNamingConfig is metaMultiCfg plus a metaConfig.naming value (raw JSON; "" omits the key).
func metaNamingConfig(naming string) json.RawMessage {
	extra := ""
	if naming != "" {
		extra = `,"naming":` + naming
	}
	return json.RawMessage(`{"metaConfig":{"budget":2500,"startDate":"2099-01-01","endDate":"2099-02-01","objective":"traffic","geoTargets":["US"],"currencyOffset":100,
		"variants":[{"headline":"KubeCon 2099","primaryText":"Join us","description":"Cloud native event"}]` + extra + `}}`)
}

const (
	metaLegacyDispatchName = "Events | KubeCon NA 2026 | NA | Traffic | Intent | Social | cncf | MoFU"
	metaV2DispatchName     = "Events | KubeCon NA 2026 | NA | Traffic | Intent | Static | Cloud Native Computing Foundation (CNCF) | MoFU"
	namingCNCF             = `{"businessUnit":"Cloud Native Computing Foundation (CNCF)"}`
)

// metaConfig.naming (#587) is a synchronous 400 for a wrong shape or a value outside its closed
// list whatever the gate says, and for a missing business unit while the gate is on.
func TestMeta_NamingPreflight(t *testing.T) {
	d := NewMetaDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, tc := range []struct {
		name, gate, naming, want string
	}{
		{"gate off, absent", "", "", ""},
		{"gate off, valid", "", `{"region":"APAC","audienceIntent":"Retargeting","businessUnit":"PyTorch Project"}`, ""},
		{"gate off, no business unit is fine", "", `{"region":"EMEA"}`, ""},
		{"gate off, bad region", "", `{"region":"Meta"}`, "region"},
		{"gate off, bad intent", "", `{"audienceIntent":"Lookalike"}`, "audienceIntent"},
		{"gate off, slug as business unit", "", `{"businessUnit":"cncf"}`, "businessUnit"},
		{"unknown key", "", `{"regoin":"APAC"}`, "must be an object"},
		{"null", "", `null`, "must be an object"},
		{"non-string value", "", `{"region":1}`, "must be an object"},
		{"gate on, valid", "true", namingCNCF, ""},
		{"gate on, absent", "true", "", "businessUnit is required"},
		{"gate on, no business unit", "true", `{"region":"NA"}`, "businessUnit is required"},
		{"gate typo is off", "TRUE", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvMetaNamingV2Enabled, tc.gate)
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMetaAds, metaNamingConfig(tc.naming))
			if tc.want == "" {
				if !errors.Is(err, domain.ErrPreflightNotApplicable) {
					t.Fatalf("PreflightCreate = %v, want not applicable", err)
				}
				return
			}
			msg, pre := domain.AsPreflightRefusal(err)
			if !errors.Is(err, domain.ErrCreateConfigInvalid) || !pre || !strings.Contains(msg, "metaConfig.naming") || !strings.Contains(msg, tc.want) {
				t.Fatalf("PreflightCreate = %v, want a pre-flight 400 containing %q", err, tc.want)
			}
		})
	}
}

// Dispatch re-checks the same rules before any request (a path that skipped PreflightCreate).
func TestMetaDispatch_NamingRefusedBeforeAnyRequest(t *testing.T) {
	for _, tc := range []struct{ gate, naming string }{
		{"true", ""}, {"true", `{"region":"Meta","businessUnit":"LF Events"}`}, {"true", `null`},
		{"", `{"regoin":"APAC"}`}, {"", `{"region":"Meta"}`},
	} {
		t.Setenv(constants.EnvMetaNamingV2Enabled, tc.gate)
		naming := tc.naming
		w := &adWire{}
		camp, err := w.metaDispatcher(t).Dispatch(slotCtx(model.FirstSlotVersion, &model.Campaign{}), testBrief(), model.ProviderMetaAds, metaNamingConfig(naming))
		if _, pre := domain.AsPreflightRefusal(err); camp != nil || !pre || !errors.Is(err, domain.ErrCreateConfigInvalid) {
			t.Errorf("naming %s: Dispatch = %+v, %v; want a not-created config refusal", naming, camp, err)
		}
		if _, n := w.counts(); n != 0 {
			t.Errorf("naming %s: %d request(s) before the refusal; want 0", naming, n)
		}
	}
}

// The gate decides the name: off, the legacy name even when naming is sent; on, the v2 name.
func TestMetaDispatch_NamingGateSelectsTheName(t *testing.T) {
	for _, tc := range []struct{ gate, want string }{{"", metaLegacyDispatchName}, {"true", metaV2DispatchName}} {
		t.Run("gate="+tc.gate, func(t *testing.T) {
			t.Setenv(constants.EnvMetaNamingV2Enabled, tc.gate)
			w := &adWire{}
			camp, err := w.metaDispatcher(t).Dispatch(slotCtx(model.FirstSlotVersion, &model.Campaign{}), testBrief(), model.ProviderMetaAds, metaNamingConfig(namingCNCF))
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
func TestMetaDispatch_FrozenNameWinsOverNamingV2(t *testing.T) {
	t.Setenv(constants.EnvMetaNamingV2Enabled, "true")
	w := &adWire{}
	ctx := model.WithDispatchCampaignName(slotCtx(model.FirstSlotVersion, &model.Campaign{}), metaLegacyDispatchName)
	camp, err := w.metaDispatcher(t).Dispatch(ctx, testBrief(), model.ProviderMetaAds, metaNamingConfig(namingCNCF))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if sent := w.names(); camp.CampaignName != metaLegacyDispatchName || len(sent) != 1 || sent[0] != metaLegacyDispatchName {
		t.Fatalf("CampaignName = %q, sent = %q; want the frozen %q", camp.CampaignName, sent, metaLegacyDispatchName)
	}
}
