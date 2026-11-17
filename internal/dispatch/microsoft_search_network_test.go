// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// msSearchPartnersOff is the extra config that sets searchPartners to false.
const msSearchPartnersOff = `,"searchPartners":false`

// Off (the default): searchPartners, false included, is a synchronous 400 naming the flag, and
// Dispatch refuses it again before any Microsoft call. null is absent.
func TestMicrosoftSearchNetwork_GateOffRefusesBothPaths(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftSearchNetworkEnabled, "")
	api := &msAudAPI{}
	d := newMSAudDispatcher(t, api)
	for _, extra := range []string{`,"searchPartners":false`, `,"searchPartners":true`, `,"budget":"x","searchPartners":false`} {
		err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra))
		requireConfigInvalid(t, err, constants.EnvMicrosoftSearchNetworkEnabled)
	}
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msSearchPartnersOff))
	requirePreCreate(t, err)
	if !strings.Contains(err.Error(), constants.EnvMicrosoftSearchNetworkEnabled) {
		t.Errorf("Dispatch refusal must name the flag: %v", err)
	}
	if n := len(api.snapshot()); n != 0 {
		t.Errorf("%d Microsoft calls before the gate refused; want 0", n)
	}
	for _, extra := range []string{``, `,"searchPartners":null`} {
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Errorf("%q: PreflightCreate = %v, want not applicable", extra, err)
		}
	}
}

// A non-boolean is the synchronous 400 naming the field, on or off.
func TestMicrosoftSearchNetwork_WrongTypeRefused(t *testing.T) {
	for _, flag := range []string{"", "true"} {
		t.Setenv(constants.EnvMicrosoftSearchNetworkEnabled, flag)
		d := newMSAudDispatcher(t, &msAudAPI{})
		for _, extra := range []string{`,"searchPartners":"false"`, `,"SEARCHPARTNERS":0`, `,"searchPartners":[]`} {
			err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(extra))
			requireConfigInvalid(t, err, "microsoftConfig.searchPartners: must be a boolean")
		}
	}
}

// On: false reaches the new ad group as OwnedAndOperatedOnly.
func TestMicrosoftSearchNetwork_DispatchSendsIt(t *testing.T) {
	t.Setenv(constants.EnvMicrosoftSearchNetworkEnabled, "true")
	api := &msAudAPI{}
	d := newMSAudDispatcher(t, api)
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderMicrosoftAds, msNegConfig(msSearchPartnersOff)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("PreflightCreate = %v, want not applicable", err)
	}
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMicrosoftAds, msNegConfig(msSearchPartnersOff)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	calls := api.snapshot()
	api.mu.Lock()
	body := api.bodies[slices.Index(calls, "AdGroups")]
	api.mu.Unlock()
	if !strings.Contains(body, `"Network":"OwnedAndOperatedOnly"`) {
		t.Errorf("ad group body = %s", body)
	}
}
