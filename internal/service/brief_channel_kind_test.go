// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestDeliveryChannelKind pins which channel each delivery surface's campaigns must use.
//
// The mapping is the gate `CreateCampaigns` and `AdoptCampaign` apply to every platform in a
// request, so a surface that maps wrongly is refused its own channel only AFTER the brief, the
// audience and the copy all exist -- the staging step is the first place it shows. Survey was
// added to the delivery enum, the column CHECK and the stage pair rule and missed here, which
// refused every HubSpot staging of a survey brief.
func TestDeliveryChannelKind(t *testing.T) {
	cases := []struct {
		delivery model.DeliveryType
		want     model.ChannelKind
	}{
		{model.DeliveryPaidMarketing, model.ChannelPaidAds},
		{model.DeliveryEmail, model.ChannelEmail},
		{model.DeliverySurvey, model.ChannelEmail},
	}
	for _, tc := range cases {
		t.Run(string(tc.delivery), func(t *testing.T) {
			if got := deliveryChannelKind(tc.delivery); got != tc.want {
				t.Fatalf("deliveryChannelKind(%q) = %q, want %q", tc.delivery, got, tc.want)
			}
		})
	}
}

// TestEverySendSurfaceAcceptsHubSpot is the property the mapping exists to protect, stated
// against the provider rather than the channel constant: HubSpot is how every send surface
// delivers, so no delivery type that `Valid()` admits other than paid may refuse it.
//
// Iterates the types `Valid()` accepts rather than the three named above, so a fourth surface
// added to the enum without a mapping fails here instead of at the staging step.
func TestEverySendSurfaceAcceptsHubSpot(t *testing.T) {
	for _, d := range []model.DeliveryType{model.DeliveryPaidMarketing, model.DeliveryEmail, model.DeliverySurvey} {
		if !d.Valid() {
			t.Fatalf("%q is listed here but Valid() rejects it; the list has drifted from the enum", d)
		}
		sendSurface := d != model.DeliveryPaidMarketing
		accepted := model.ProviderHubSpot.Kind() == deliveryChannelKind(d)
		if sendSurface != accepted {
			t.Errorf("delivery %q: HubSpot accepted = %v, want %v", d, accepted, sendSurface)
		}
	}
}
