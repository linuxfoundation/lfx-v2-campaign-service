// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// TestGetCampaignSettingsWithTargeting_AsksForAndReadsTheTargeting: the ad set read adds targeting
// and adset_schedule, and the regions, cities and schedule are read from them; GetCampaignSettings
// asks for neither and leaves Targeting nil.
func TestGetCampaignSettingsWithTargeting_AsksForAndReadsTheTargeting(t *testing.T) {
	adSet := strings.TrimSuffix(metaSettingsAdSet, "}") + `,"targeting":{"geo_locations":{"regions":[{"key":"3847"}],"cities":[{"key":2420605,"radius":10}]}},"adset_schedule":[{"start_minute":480,"end_minute":1020,"days":[1,5],"timezone_type":"ADVERTISER"}]}`
	routes := map[string]settingsReply{"/555": {http.StatusOK, metaSettingsCampaign}, "/888": {http.StatusOK, adSet}}

	c, seen := settingsTestClient(t, routes)
	got, err := c.GetCampaignSettingsWithTargeting(context.Background(), "555", "888")
	if err != nil {
		t.Fatalf("GetCampaignSettingsWithTargeting: %v", err)
	}
	want := &AdSetTargetingSettings{Regions: []string{"3847"}, Cities: []string{"2420605"},
		Schedule: []AdScheduleBlock{{Days: []int{1, 5}, StartMinute: 480, EndMinute: 1020, TimezoneType: "ADVERTISER"}}}
	if !reflect.DeepEqual(got.AdSet.Targeting, want) {
		t.Errorf("Targeting = %+v, want %+v", got.AdSet.Targeting, want)
	}
	if reqs := seen(); len(reqs) != 2 || !strings.Contains(reqs[1], "fields="+settingsAdSetFields+settingsAdSetTargetingFields) {
		t.Errorf("requests = %v, want the ad set asked for targeting and adset_schedule", reqs)
	}

	c, seen = settingsTestClient(t, routes)
	plain, err := c.GetCampaignSettings(context.Background(), "555", "888")
	if err != nil || plain.AdSet == nil || plain.AdSet.Targeting != nil {
		t.Fatalf("GetCampaignSettings = %+v, %v; want the ad set without targeting", plain, err)
	}
	if reqs := seen(); strings.Contains(reqs[1], "targeting") || strings.Contains(reqs[1], "adset_schedule") {
		t.Errorf("plain read asked for targeting: %v", reqs)
	}
}

// TestDecodeAdSetTargeting: absent lists are none, unreadable ones unknown, a missing or non-object
// targeting nothing at all.
func TestDecodeAdSetTargeting(t *testing.T) {
	for _, tc := range []struct {
		name, targeting, schedule string
		want                      *AdSetTargetingSettings
	}{
		{"no targeting", ``, ``, nil},
		{"null targeting", `null`, ``, nil},
		{"array targeting", `[1]`, ``, nil},
		{"string targeting", `"x"`, ``, nil},
		{"no geo, no schedule", `{}`, ``, &AdSetTargetingSettings{Regions: []string{}, Cities: []string{}, Schedule: []AdScheduleBlock{}}},
		{"countries only, null schedule", `{"geo_locations":{"countries":["US"]}}`, `null`, &AdSetTargetingSettings{Regions: []string{}, Cities: []string{}, Schedule: []AdScheduleBlock{}}},
		{"geo of the wrong shape", `{"geo_locations":{"regions":"CA"}}`, `[]`, &AdSetTargetingSettings{GeoUnreadable: true, Schedule: []AdScheduleBlock{}}},
		{"a non-numeric city key", `{"geo_locations":{"cities":[{"key":"SF"}]}}`, ``, &AdSetTargetingSettings{GeoUnreadable: true, Schedule: []AdScheduleBlock{}}},
		{"a non-numeric region key", `{"geo_locations":{"regions":[{"key":"CA"}],"cities":[{"key":"1"}]}}`, ``, &AdSetTargetingSettings{GeoUnreadable: true, Schedule: []AdScheduleBlock{}}},
		{"schedule not a list", `{}`, `{"days":[1]}`, &AdSetTargetingSettings{Regions: []string{}, Cities: []string{}, ScheduleUnreadable: true}},
		{"schedule block without minutes", `{}`, `[{"days":[1]}]`, &AdSetTargetingSettings{Regions: []string{}, Cities: []string{}, ScheduleUnreadable: true}},
		{"schedule block without days", `{}`, `[{"days":[],"start_minute":0,"end_minute":60}]`, &AdSetTargetingSettings{Regions: []string{}, Cities: []string{}, ScheduleUnreadable: true}},
	} {
		got := decodeAdSetTargeting(json.RawMessage(tc.targeting), json.RawMessage(tc.schedule))
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
