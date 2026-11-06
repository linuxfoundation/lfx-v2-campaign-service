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

// #513: googleAdsConfig.campaignAudienceSegments must reach the client. A list on another
// customer is refused by the client's preflight, before the budget, only if the mapper passed
// the field on; a dropped field would create the campaign with no lists and no error.
func TestGoogleAds_CampaignAudienceSegmentsReachTheClient(t *testing.T) {
	opts, cap := targetingServers(t)
	d := NewGoogleAdsDispatcher(fakeConnReader{conn: activeGoogleAdsConn(goodGoogleAdsCreds)}, identityEncryptor{}, opts...)
	cfg := json.RawMessage(`{"googleAdsConfig":{"budget":50,"campaignAudienceSegments":["customers/999/userLists/1"]}}`)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderGoogleAds, cfg)
	if err == nil || !strings.Contains(err.Error(), `CampaignAudienceSegments entry`) {
		t.Fatalf("err = %v, want the client's account refusal of the campaign-level list", err)
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
