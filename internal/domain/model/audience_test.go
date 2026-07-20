// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "testing"

func TestCampaignAudience_StatusOrDefault(t *testing.T) {
	// An empty status defaults to 'building' so a create never writes an empty string
	// that would violate the campaign_audiences status CHECK constraint.
	if got := (&CampaignAudience{}).StatusOrDefault(); got != AudienceBuilding {
		t.Errorf("empty status = %q, want %q", got, AudienceBuilding)
	}
	// An explicit status is preserved.
	for _, s := range []AudienceStatus{AudienceBuilding, AudienceBuilt, AudienceFailed} {
		if got := (&CampaignAudience{Status: s}).StatusOrDefault(); got != s {
			t.Errorf("StatusOrDefault(%q) = %q, want unchanged", s, got)
		}
	}
}
