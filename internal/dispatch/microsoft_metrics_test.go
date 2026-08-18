// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// The Microsoft reporting contract follows Microsoft's published v13 docs but has never
// been exercised against a live Bing account, so the read is gated OFF by default. The
// gate is the only thing standing between an unverified contract and numbers a dashboard
// would render as authoritative — which makes an untested gate the one most worth pinning.

// TestMicrosoftReadMetrics_DisabledGateRefusesTheRead pins the default-OFF behaviour: a
// disabled read must answer ErrMetricsUnsupported, never an empty-but-successful result.
// A zero returned here is indistinguishable, to every consumer, from a measured zero.
func TestMicrosoftReadMetrics_DisabledGateRefusesTheRead(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "")
	d := NewMicrosoftDispatcher(nil, identityEncryptor{})

	got, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMicrosoftAds,
		&model.Campaign{Platform: model.ProviderMicrosoftAds, PlatformCampaignID: "999"},
		model.MetricsWindowLast7Days)

	if !errors.Is(err, domain.ErrMetricsUnsupported) {
		t.Errorf("want ErrMetricsUnsupported, got %v", err)
	}
	if got != nil {
		t.Errorf("a refused read must return no metrics, got %+v", got)
	}
}

// TestMicrosoftReadMetrics_GateFailsClosedOnEveryNonTrueValue pins that the gate accepts
// ONLY the literal "true". Anything else — including values an operator might reasonably
// expect to work — must leave the unverified read disabled.
func TestMicrosoftReadMetrics_GateFailsClosedOnEveryNonTrueValue(t *testing.T) {
	for _, v := range []string{"", "TRUE", "True", "1", "yes", "on", " true", "true "} {
		t.Run("value="+v, func(t *testing.T) {
			t.Setenv(constants.EnvMicrosoftMetricsEnabled, v)
			d := NewMicrosoftDispatcher(nil, identityEncryptor{})

			_, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMicrosoftAds,
				&model.Campaign{Platform: model.ProviderMicrosoftAds, PlatformCampaignID: "999"},
				model.MetricsWindowLast7Days)

			if !errors.Is(err, domain.ErrMetricsUnsupported) {
				t.Errorf("%q must leave the read disabled, got %v", v, err)
			}
		})
	}
}

// TestMicrosoftReadMetrics_EnabledGatePassesTheGate proves the gate is a gate and not an
// unconditional refusal: with it on, the call proceeds past the gate and fails later, on
// credential resolution. Without this the disabled-path tests above would still pass
// against an implementation that refused every read.
func TestMicrosoftReadMetrics_EnabledGatePassesTheGate(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	d := NewMicrosoftDispatcher(fakeConnReader{err: domain.ErrNotFound}, identityEncryptor{})

	_, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMicrosoftAds,
		&model.Campaign{Platform: model.ProviderMicrosoftAds, PlatformCampaignID: "999"},
		model.MetricsWindowLast7Days)

	if err == nil {
		t.Fatal("expected an error from credential resolution")
	}
	if errors.Is(err, domain.ErrMetricsUnsupported) {
		t.Errorf("the gate was ON, so the read must not answer ErrMetricsUnsupported: %v", err)
	}
}

// TestMicrosoftReadMetrics_MissingPlatformCampaignIDIsRejected pins that the id check sits
// AFTER the gate — a campaign never provisioned upstream has nothing to read, and asking
// Microsoft for an empty id would be a request we know cannot succeed.
func TestMicrosoftReadMetrics_MissingPlatformCampaignIDIsRejected(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftMetricsEnabled, "true")
	d := NewMicrosoftDispatcher(nil, identityEncryptor{})

	for name, camp := range map[string]*model.Campaign{
		"nil campaign": nil,
		"empty id":     {Platform: model.ProviderMicrosoftAds},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := d.ReadMetrics(context.Background(), "proj", model.ProviderMicrosoftAds, camp,
				model.MetricsWindowLast7Days)
			if err == nil {
				t.Fatal("expected an error")
			}
			if errors.Is(err, domain.ErrMetricsUnsupported) {
				t.Errorf("this is a missing-id error, not an unsupported-platform one: %v", err)
			}
		})
	}
}
