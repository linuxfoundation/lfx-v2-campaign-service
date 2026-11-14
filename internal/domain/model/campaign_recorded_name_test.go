// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"context"
	"encoding/json"
	"testing"
)

// RecordedCampaignName reads the name a row already records (#587): the column first, then the
// Result blob a retained partial persists. A row with neither records nothing.
func TestRecordedCampaignName(t *testing.T) {
	const name = "Events | KubeCon | LinkedIn | Traffic | Prospecting | Static | cncf | MoFU"
	for _, tc := range []struct {
		desc string
		row  *Campaign
		want string
	}{
		{"nil row", nil, ""},
		{"fresh claim", &Campaign{}, ""},
		{"column", &Campaign{CampaignName: "  " + name + " "}, name},
		{"column wins over the blob", &Campaign{CampaignName: name, Result: json.RawMessage(`{"campaignName":"other"}`)}, name},
		{"blob when the column is empty", &Campaign{Result: json.RawMessage(`{"campaignName":"` + name + `","campaignId":""}`)}, name},
		{"blob without a name", &Campaign{Result: json.RawMessage(`{"campaignId":"1"}`)}, ""},
		{"undecodable blob", &Campaign{Result: json.RawMessage(`not json`)}, ""},
	} {
		if got := RecordedCampaignName(tc.row); got != tc.want {
			t.Errorf("%s: RecordedCampaignName = %q, want %q", tc.desc, got, tc.want)
		}
	}
}

func TestDispatchCampaignName(t *testing.T) {
	if got := DispatchCampaignName(context.Background()); got != "" {
		t.Errorf("unset: %q, want empty", got)
	}
	if got := DispatchCampaignName(WithDispatchCampaignName(context.Background(), " n ")); got != "n" {
		t.Errorf("set: %q, want trimmed n", got)
	}
}
