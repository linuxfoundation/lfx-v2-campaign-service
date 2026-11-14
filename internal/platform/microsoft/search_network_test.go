// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"strings"
	"testing"
)

// searchPartners maps to AdGroup.Network on the ad group this run creates; nil sends no Network,
// so Microsoft's default (partners included) applies exactly as before (#563 gap 11).
func TestCreateCampaign_SearchPartnersSetsTheAdGroupNetwork(t *testing.T) {
	yes, no := true, false
	for _, tc := range []struct {
		name     string
		partners *bool
		want     string // "" = no Network key at all
	}{
		{"omitted", nil, ""},
		{"partners", &yes, `"Network":"OwnedAndOperatedAndSyndicatedSearch"`},
		{"owned only", &no, `"Network":"OwnedAndOperatedOnly"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &targetingAPI{}
			c := newTargetingClient(t, a)
			in := validInput()
			in.SearchPartners = tc.partners
			res, err := c.CreateCampaign(context.Background(), in)
			if err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			body := a.bodyOf("AdGroups", 1)
			steps := strings.Join(res.Steps, "\n")
			if tc.want == "" {
				if strings.Contains(body, `"Network"`) || strings.Contains(steps, "search network") {
					t.Errorf("no searchPartners must send no Network: body=%s steps=%s", body, steps)
				}
				return
			}
			if !strings.Contains(body, tc.want) {
				t.Errorf("ad group body = %s, want %s", body, tc.want)
			}
			if !strings.Contains(steps, "Ad group search network set: ") {
				t.Errorf("steps = %s", steps)
			}
		})
	}
}

// A reused ad group keeps its network: nothing is sent to it, and a warning step says so.
func TestCreateCampaign_SearchPartnersOnAReusedAdGroupIsReported(t *testing.T) {
	no := false
	in := validInput()
	in.SearchPartners = &no
	agName := composeAdGroupName(in)
	a := &targetingAPI{adGroupQueryBody: `{"AdGroups":[{"Id":654,"Name":` + jsonString(agName) + `}]}`}
	c := newTargetingClient(t, a)
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if b := a.bodyOf("AdGroups", 1); b != "" {
		t.Fatalf("an ad group create was sent for a reused ad group: %s", b)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "Warning: search network NOT applied to reused ad group 654") {
		t.Errorf("steps = %v", res.Steps)
	}
}
