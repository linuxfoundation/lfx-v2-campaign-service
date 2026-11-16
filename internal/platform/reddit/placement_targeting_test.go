// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

func TestNormalizePlacementTargeting(t *testing.T) {
	no := false
	for _, tc := range []struct {
		name                string
		placements, plats   []string
		expand              *bool
		wantLocs, wantPlats []string
		wantExpand          bool
		errField, errHas    string
	}{
		{name: "defaults", wantLocs: []string{"FEED", "COMMENTS_PAGE"}, wantPlats: []string{"ALL"}, wantExpand: true},
		{name: "feed only, expansion off", placements: []string{" feed "}, expand: &no,
			wantLocs: []string{"FEED"}, wantPlats: []string{"ALL"}, wantExpand: false},
		{name: "fixed order, duplicates dropped", placements: []string{"comments_page", "FEED", "feed"},
			plats:    []string{"mobile_web", "DESKTOP", "desktop"},
			wantLocs: []string{"FEED", "COMMENTS_PAGE"}, wantPlats: []string{"DESKTOP", "MOBILE_WEB"}, wantExpand: true},
		{name: "blank placement", placements: []string{"FEED", " "}, errField: FieldPlacements, errHas: "entry 2 is blank"},
		{name: "unknown placement", placements: []string{"SIDEBAR"}, errField: FieldPlacements, errHas: "is not one of"},
		{name: "ALL is not an entry", plats: []string{"ALL"}, errField: FieldPlatforms, errHas: "is not one of"},
		{name: "blank platform", plats: []string{""}, errField: FieldPlatforms, errHas: "blank"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizePlacementTargeting(tc.placements, tc.plats, tc.expand)
			if tc.errField != "" {
				var pe *PlacementError
				if !errors.As(err, &pe) || pe.Field != tc.errField || !strings.Contains(pe.Msg, tc.errHas) {
					t.Fatalf("want a PlacementError on %s naming %q, got %v", tc.errField, tc.errHas, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got.Locations, tc.wantLocs) || !reflect.DeepEqual(got.Platforms, tc.wantPlats) || got.ExpandTargeting != tc.wantExpand {
				t.Fatalf("got %+v, want locations %v platforms %v expand %t", got, tc.wantLocs, tc.wantPlats, tc.wantExpand)
			}
		})
	}
}

func TestPlacementValuesAreCopies(t *testing.T) {
	p, q := Placements(), Platforms()
	p[0], q[0] = "MUTATED", "MUTATED"
	if Placements()[0] == "MUTATED" || Platforms()[0] == "MUTATED" {
		t.Fatal("Placements/Platforms must return copies")
	}
}

// Omitted, the ad group's targeting is byte-for-byte what the create always sent.
func TestCreateCampaign_PlacementDefaultsUnchanged(t *testing.T) {
	c, bodies, cleanup := newBodyCaptureServers(t)
	defer cleanup()
	res, err := c.CreateCampaign(context.Background(), validRedditInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	_, adGroup := bodies()
	targeting, _ := adGroup["targeting"].(map[string]any)
	if got, want := targeting["locations"], []any{"FEED", "COMMENTS_PAGE"}; !reflect.DeepEqual(got, want) {
		t.Errorf("locations = %#v, want %#v", got, want)
	}
	if got, want := targeting["platforms"], []any{"ALL"}; !reflect.DeepEqual(got, want) {
		t.Errorf("platforms = %#v, want %#v", got, want)
	}
	if targeting["expand_targeting"] != true {
		t.Errorf("expand_targeting = %#v, want true", targeting["expand_targeting"])
	}
	if strings.Contains(strings.Join(res.Steps, " | "), "placements ") {
		t.Errorf("a default create must not report a placement choice; steps: %v", res.Steps)
	}
}

func TestCreateCampaign_SendsPlacementChoices(t *testing.T) {
	c, bodies, cleanup := newBodyCaptureServers(t)
	defer cleanup()
	in := validRedditInput()
	no := false
	in.Placements = []string{"feed"}
	in.Platforms = []string{"mobile_native", "desktop"}
	in.ExpandTargeting = &no
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	_, adGroup := bodies()
	targeting, _ := adGroup["targeting"].(map[string]any)
	if got, want := targeting["locations"], []any{"FEED"}; !reflect.DeepEqual(got, want) {
		t.Errorf("locations = %#v, want %#v", got, want)
	}
	if got, want := targeting["platforms"], []any{"DESKTOP", "MOBILE_NATIVE"}; !reflect.DeepEqual(got, want) {
		t.Errorf("platforms = %#v, want %#v", got, want)
	}
	if targeting["expand_targeting"] != false {
		t.Errorf("expand_targeting = %#v, want false", targeting["expand_targeting"])
	}
	if joined := strings.Join(res.Steps, " | "); !strings.Contains(joined, "placements FEED, platforms DESKTOP+MOBILE_NATIVE, expansion false") {
		t.Errorf("the Targeting step must report the choice; steps: %s", joined)
	}
}

// The communities/interests 400 retry keeps the placement choice: a retry that silently turned
// expansion back on would serve beyond the communities the operator limited it to.
func TestCreateCampaign_RetryKeepsPlacementChoices(t *testing.T) {
	c, bodies, cleanup := interestRejectingServers(t, "interests")
	defer cleanup()
	in := interestInput()
	no := false
	in.Placements = []string{"COMMENTS_PAGE"}
	in.ExpandTargeting = &no
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	sent := bodies()
	if len(sent) != 2 {
		t.Fatalf("expected the rejected attempt and the retry, got %d ad-group POSTs", len(sent))
	}
	for i, b := range sent {
		targeting, _ := b["targeting"].(map[string]any)
		if targeting["expand_targeting"] != false || !reflect.DeepEqual(targeting["locations"], []any{"COMMENTS_PAGE"}) {
			t.Errorf("attempt %d lost the placement choice: %#v", i+1, targeting)
		}
	}
}

func TestCreateCampaign_RefusesBadPlacementBeforeAnyCall(t *testing.T) {
	for name, mutate := range map[string]func(*CampaignInput){
		"unknown placement": func(in *CampaignInput) { in.Placements = []string{"SIDEBAR"} },
		"unknown platform":  func(in *CampaignInput) { in.Platforms = []string{"TV"} },
	} {
		t.Run(name, func(t *testing.T) {
			c, calls, cleanup := newCountingServers(t)
			defer cleanup()
			in := validRedditInput()
			mutate(&in)
			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil || res != nil {
				t.Fatalf("want a refusal and no result, got res=%v err=%v", res, err)
			}
			var refusal *domain.PreflightRefusal
			if !errors.As(err, &refusal) {
				t.Errorf("refusal is not a domain.PreflightRefusal: %v", err)
			}
			if n := calls(); n != 0 {
				t.Errorf("%d upstream calls before the refusal; want 0", n)
			}
		})
	}
}
