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

func ip(v int) *int { return &v }

func window(sd, sh, ed, eh int) ScheduleWindow {
	return ScheduleWindow{StartDay: ip(sd), StartHour: ip(sh), EndDay: ip(ed), EndHour: ip(eh)}
}

func TestNormalizeDemographicTargeting(t *testing.T) {
	str := func(s string) *string { return &s }
	many := make([]ScheduleWindow, MaxScheduleWindows+1)
	for i := range many {
		many[i] = window(i%7, i%24, (i+1)%7, i%24)
	}
	for _, tc := range []struct {
		name     string
		gender   *string
		langs    []string
		schedule []ScheduleWindow
		want     string // refusal substring; "" = accepted
	}{
		{"nothing", nil, nil, nil, ""},
		{"female", str(" female "), nil, nil, ""},
		{"blank gender", str(" "), nil, nil, "gender: an empty value is refused"},
		{"unknown gender", str("other"), nil, nil, "gender: \"other\" is not one of FEMALE, MALE"},
		{"languages", nil, []string{"en", "DE"}, nil, ""},
		{"blank language", nil, []string{"EN", ""}, nil, "languages: entry 2 is blank"},
		{"unknown language", nil, []string{"xx"}, nil, "languages: entry 1, \"xx\", is not one of"},
		{"window", nil, nil, []ScheduleWindow{window(1, 9, 5, 17)}, ""},
		{"wrapping week", nil, nil, []ScheduleWindow{window(6, 22, 0, 6)}, ""},
		{"missing field", nil, nil, []ScheduleWindow{{StartDay: ip(1), StartHour: ip(9), EndDay: ip(5)}}, "schedule: entry 1 needs"},
		{"day 7", nil, nil, []ScheduleWindow{window(1, 9, 7, 17)}, "entry 1: days are 0"},
		{"hour 24", nil, nil, []ScheduleWindow{window(1, 24, 5, 17)}, "entry 1: hours are 0 to 23"},
		{"negative", nil, nil, []ScheduleWindow{window(-1, 9, 5, 17)}, "entry 1: days are 0"},
		{"zero length", nil, nil, []ScheduleWindow{window(1, 9, 5, 17), window(2, 8, 2, 8)}, "entry 2 starts where it ends"},
		{"repeat", nil, nil, []ScheduleWindow{window(1, 9, 5, 17), window(1, 9, 5, 17)}, "entry 2 repeats"},
		{"over the cap", nil, nil, many, "holds 51 windows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeDemographicTargeting(tc.gender, tc.langs, tc.schedule)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("want accepted, got %v", err)
				}
				return
			}
			var de *DemographicError
			if !errors.As(err, &de) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want a DemographicError containing %q", err, tc.want)
			}
		})
	}
	got, err := NormalizeDemographicTargeting(str("male"), []string{"fr", "en", "FR"}, []ScheduleWindow{window(1, 9, 5, 17)})
	if err != nil {
		t.Fatal(err)
	}
	want := DemographicTargeting{Gender: "MALE", Languages: []string{"EN", "FR"}, Schedule: []map[string]int{{"start_day": 1, "start_hour": 9, "end_day": 5, "end_hour": 17}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// Omitted, neither the campaign nor the ad group carries any of the three.
func TestCreateCampaign_DemographicDefaultsUnchanged(t *testing.T) {
	c, bodies, cleanup := newBodyCaptureServers(t)
	defer cleanup()
	if _, err := c.CreateCampaign(context.Background(), validRedditInput()); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	campaign, adGroup := bodies()
	targeting, _ := adGroup["targeting"].(map[string]any)
	for _, k := range []string{"gender", "languages"} {
		if _, ok := targeting[k]; ok {
			t.Errorf("a default create sent targeting.%s", k)
		}
	}
	if _, ok := campaign["schedule"]; ok {
		t.Error("a default create sent a campaign schedule")
	}
}

// Gender and languages go on the ad group's targeting; the schedule on the campaign.
func TestCreateCampaign_SendsDemographicChoices(t *testing.T) {
	c, bodies, cleanup := newBodyCaptureServers(t)
	defer cleanup()
	in := validRedditInput()
	g := "female"
	in.Gender = &g
	in.Languages = []string{"es", "en"}
	in.Schedule = []ScheduleWindow{window(1, 9, 5, 17)}
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	campaign, adGroup := bodies()
	targeting, _ := adGroup["targeting"].(map[string]any)
	if targeting["gender"] != "FEMALE" {
		t.Errorf("gender = %#v, want FEMALE", targeting["gender"])
	}
	if got, want := targeting["languages"], []any{"EN", "ES"}; !reflect.DeepEqual(got, want) {
		t.Errorf("languages = %#v, want %#v", got, want)
	}
	want := []any{map[string]any{"start_day": float64(1), "start_hour": float64(9), "end_day": float64(5), "end_hour": float64(17)}}
	if !reflect.DeepEqual(campaign["schedule"], want) {
		t.Errorf("campaign schedule = %#v, want %#v", campaign["schedule"], want)
	}
	if _, ok := adGroup["schedule"]; ok {
		t.Error("the schedule belongs on the budget-optimized campaign, not the ad group")
	}
	if joined := strings.Join(res.Steps, " | "); !strings.Contains(joined, "gender FEMALE, languages EN+ES, 1 schedule windows (campaign)") {
		t.Errorf("the Targeting step must report the choices; steps: %s", joined)
	}
}

// The communities/interests 400 retry keeps gender and languages.
func TestCreateCampaign_RetryKeepsDemographicChoices(t *testing.T) {
	c, bodies, cleanup := interestRejectingServers(t, "interests")
	defer cleanup()
	in := interestInput()
	g := "MALE"
	in.Gender = &g
	in.Languages = []string{"EN"}
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	sent := bodies()
	if len(sent) != 2 {
		t.Fatalf("expected the rejected attempt and the retry, got %d ad-group POSTs", len(sent))
	}
	for i, b := range sent {
		targeting, _ := b["targeting"].(map[string]any)
		if targeting["gender"] != "MALE" || !reflect.DeepEqual(targeting["languages"], []any{"EN"}) {
			t.Errorf("attempt %d lost the choice: %#v", i+1, targeting)
		}
	}
}

func TestCreateCampaign_RefusesBadDemographicsBeforeAnyCall(t *testing.T) {
	for name, mutate := range map[string]func(*CampaignInput){
		"gender":   func(in *CampaignInput) { g := "x"; in.Gender = &g },
		"language": func(in *CampaignInput) { in.Languages = []string{"klingon"} },
		"schedule": func(in *CampaignInput) { in.Schedule = []ScheduleWindow{window(1, 9, 1, 9)} },
	} {
		t.Run(name, func(t *testing.T) {
			c, calls, cleanup := newCountingServers(t)
			defer cleanup()
			in := validRedditInput()
			mutate(&in)
			res, err := c.CreateCampaign(context.Background(), in)
			var refusal *domain.PreflightRefusal
			if res != nil || !errors.As(err, &refusal) {
				t.Fatalf("want a PreflightRefusal and no result, got res=%v err=%v", res, err)
			}
			if n := calls(); n != 0 {
				t.Errorf("%d upstream calls before the refusal; want 0", n)
			}
		})
	}
}
