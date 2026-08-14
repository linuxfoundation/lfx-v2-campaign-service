// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// Adoption files an upstream campaign into a brief SLOT, and the slot key includes the
// variant. Before this mapping existed, every adopted Google campaign was stored as
// 'default' whatever it actually was: adopting a Demand Gen campaign left the 'demand-gen'
// slot free, so the next Demand Gen dispatch for that brief saw an empty slot and created a
// SECOND paid campaign. Real money, twice, for one brief.
func TestGoogleAdsVariantForChannelType(t *testing.T) {
	cases := []struct {
		name        string
		channelType string
		want        string
	}{
		{"search maps to the default slot", "SEARCH", model.VariantDefault},
		{"demand gen maps to its own slot", "DEMAND_GEN", "demand-gen"},
		// Google's enum arrives uppercase, but a mapping that depends on the platform's
		// exact casing is one response-format change away from failing closed on a campaign
		// type it does support.
		{"casing and space are tolerated", "  demand_gen ", "demand-gen"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := googleAdsVariantForChannelType(tc.channelType)
			if err != nil {
				t.Fatalf("googleAdsVariantForChannelType(%q): unexpected error: %v", tc.channelType, err)
			}
			if got != tc.want {
				t.Errorf("googleAdsVariantForChannelType(%q) = %q, want %q", tc.channelType, got, tc.want)
			}
		})
	}
}

// The fail-closed half, and the half that matters most. A campaign type this service cannot
// CREATE has no slot to adopt into, so mapping it onto an existing slot would both mis-file
// that campaign and leave the real slot open for a duplicate. Performance Max is the live
// example -- the user has said it is coming -- but the same must hold for a type Google adds
// that nobody here has heard of yet, and for a response that omits the field entirely.
func TestGoogleAdsVariantForChannelTypeFailsClosed(t *testing.T) {
	for _, channelType := range []string{"PERFORMANCE_MAX", "VIDEO", "SHOPPING", "SOMETHING_NEW", ""} {
		t.Run("refuses "+channelType, func(t *testing.T) {
			got, err := googleAdsVariantForChannelType(channelType)
			if err == nil {
				t.Fatalf("googleAdsVariantForChannelType(%q) = %q with no error; an unmappable type must be refused, not defaulted -- defaulting is what creates the duplicate", channelType, got)
			}
			if got != "" {
				t.Errorf("googleAdsVariantForChannelType(%q) returned variant %q alongside its error; a caller that ignores the error must not receive a usable slot", channelType, got)
			}
		})
	}
}

// The refusal must never be spelled as a variant that a create could also produce. If an
// unmappable type ever resolved to 'default' or 'demand-gen', it would collide with a real
// campaign's slot -- which is the entire failure this mapping exists to prevent.
func TestGoogleAdsVariantRefusalIsNotASlot(t *testing.T) {
	_, err := googleAdsVariantForChannelType("PERFORMANCE_MAX")
	if err == nil {
		t.Fatal("expected an error for an unmappable channel type")
	}
	for _, slot := range []string{model.VariantDefault, "demand-gen"} {
		if strings.TrimSpace(err.Error()) == slot {
			t.Errorf("the refusal error reads exactly like the slot %q", slot)
		}
	}
}
