// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"encoding/json"
	"errors"
	"slices"
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

// TestCampaignAudience_SendListIDs pins which lists a send targets: every recorded include list
// (trimmed, blanks and duplicates dropped, order kept) when there are any, otherwise the master
// alone, and nothing when neither names a list. Corrupt include ids are an error, never a silent
// fall back to the master -- that would send to a subset of the recorded audience.
func TestCampaignAudience_SendListIDs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		a       CampaignAudience
		want    []string
		wantErr bool
	}{
		{"master only", CampaignAudience{PlatformMasterListID: " 31027 "}, []string{"31027"}, false},
		{"null include falls back to master", CampaignAudience{PlatformMasterListID: "31027", IncludeListIDs: json.RawMessage(`null`)}, []string{"31027"}, false},
		{"empty include falls back to master", CampaignAudience{PlatformMasterListID: "31027", IncludeListIDs: json.RawMessage(`[]`)}, []string{"31027"}, false},
		{"blank-only include falls back to master", CampaignAudience{PlatformMasterListID: "31027", IncludeListIDs: json.RawMessage(`[" ",""]`)}, []string{"31027"}, false},
		{"include wins, cleaned", CampaignAudience{PlatformMasterListID: "31027", IncludeListIDs: json.RawMessage(`[" 31027 ","31028","","31027","31029"]`)}, []string{"31027", "31028", "31029"}, false},
		{"nothing", CampaignAudience{}, nil, false},
		{"corrupt include", CampaignAudience{PlatformMasterListID: "31027", IncludeListIDs: json.RawMessage(`{"a":1}`)}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.a.SendListIDs()
			if tc.wantErr {
				if !errors.Is(err, ErrAudienceIncludeListIDsUnreadable) {
					t.Fatalf("err = %v, want ErrAudienceIncludeListIDsUnreadable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("SendListIDs: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("SendListIDs = %v, want %v", got, tc.want)
			}
		})
	}
}
