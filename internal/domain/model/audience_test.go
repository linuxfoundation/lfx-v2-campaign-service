// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"errors"
	"testing"
)

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

func TestCampaignAudience_Validate_BuiltNeedsMasterList(t *testing.T) {
	cases := []struct {
		name    string
		a       CampaignAudience
		wantErr bool
	}{
		{"built with master list ok", CampaignAudience{Status: AudienceBuilt, PlatformMasterListID: "m1"}, false},
		{"built without master list", CampaignAudience{Status: AudienceBuilt}, true},
		{"built with whitespace master list", CampaignAudience{Status: AudienceBuilt, PlatformMasterListID: "  "}, true},
		{"building without master list ok", CampaignAudience{Status: AudienceBuilding}, false},
		{"failed without master list ok", CampaignAudience{Status: AudienceFailed}, false},
		{"empty status (→building) without master list ok", CampaignAudience{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.a.Validate()
			if tc.wantErr {
				if !errors.Is(err, ErrAudienceBuiltNeedsMasterList) {
					t.Errorf("want ErrAudienceBuiltNeedsMasterList, got %v", err)
				}
				return
			}
			if err != nil {
				t.Errorf("want no error, got %v", err)
			}
		})
	}
}
