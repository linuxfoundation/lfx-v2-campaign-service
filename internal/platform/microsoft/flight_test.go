// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// The fixed test clock (fixedClock) is 2026-01-01T00:00:00Z, so "today" is 2026-01-01.

// captureAdGroupBody returns a client whose AddAdGroups POST body is recorded raw, plus a
// pointer to that body, with every other route served by the shared campaignsAPI script.
func captureAdGroupBody(t *testing.T, api *campaignsAPI) (*Client, *string) {
	t.Helper()
	c, adGroup, _ := captureCreateBodies(t, api)
	return c, adGroup
}

// captureCreateBodies is captureAdGroupBody that also records the raw AddCampaigns body.
func captureCreateBodies(t *testing.T, api *campaignsAPI) (c *Client, adGroup, campaign *string) {
	t.Helper()
	var raw, campRaw string
	base := api.handler(t)
	c = newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Campaigns") {
			b, _ := io.ReadAll(r.Body)
			campRaw = string(b)
			r.Body = io.NopCloser(strings.NewReader(campRaw))
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/AdGroups") {
			b, _ := io.ReadAll(r.Body)
			raw = string(b)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"AdGroupIds":[654],"PartialErrors":[]}`)
			return
		}
		base(w, r)
	})
	return c, &raw, &campRaw
}

// TestCreateCampaign_FlightDatesSentOnAdGroup pins the wire mapping: startDate/endDate reach
// the AddAdGroups body as AdGroup.StartDate/EndDate in Microsoft's {Month,Day,Year} object
// form — not as ISO strings, and not on the campaign.
func TestCreateCampaign_FlightDatesSentOnAdGroup(t *testing.T) {
	c, raw, campRaw := captureCreateBodies(t, &campaignsAPI{})
	in := validInput()
	in.StartDate = "2026-02-03"
	in.EndDate = "2026-03-31"
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	for _, want := range []string{
		`"StartDate":{"Month":2,"Day":3,"Year":2026}`,
		`"EndDate":{"Month":3,"Day":31,"Year":2026}`,
	} {
		if !strings.Contains(*raw, want) {
			t.Errorf("AddAdGroups body lacks %s: %s", want, *raw)
		}
	}
	// The campaign create is unchanged: Microsoft campaigns carry no flight dates.
	if *campRaw == "" {
		t.Fatal("campaign create body not captured")
	}
	if strings.Contains(*campRaw, "StartDate") || strings.Contains(*campRaw, "EndDate") {
		t.Errorf("campaign create carries flight dates; they belong on the ad group: %s", *campRaw)
	}
	if !stepsContain(res.Steps, "Ad group flight dates set: start 2026-02-03, end 2026-03-31") {
		t.Errorf("steps do not record the flight window: %v", res.Steps)
	}
}

// TestCreateCampaign_OneFlightDateOnly sends only the supplied side; the other stays omitted
// so Microsoft's default for it applies.
func TestCreateCampaign_OneFlightDateOnly(t *testing.T) {
	cases := map[string]struct {
		start, end      string
		present, absent string
	}{
		"start only": {start: "2026-01-01", present: `"StartDate":{"Month":1,"Day":1,"Year":2026}`, absent: `"EndDate"`},
		"end only":   {end: "2026-01-02", present: `"EndDate":{"Month":1,"Day":2,"Year":2026}`, absent: `"StartDate"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, raw := captureAdGroupBody(t, &campaignsAPI{})
			in := validInput()
			in.StartDate, in.EndDate = tc.start, tc.end
			if _, err := c.CreateCampaign(context.Background(), in); err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			if !strings.Contains(*raw, tc.present) {
				t.Errorf("AddAdGroups body lacks %s: %s", tc.present, *raw)
			}
			if strings.Contains(*raw, tc.absent) {
				t.Errorf("AddAdGroups body carries %s, which was not requested: %s", tc.absent, *raw)
			}
		})
	}
}

// TestCreateCampaign_NoFlightDatesKeepsTodaysBody is the absent-dates guarantee: with neither
// date the ad group body carries no date field at all and no flight step is written, so the
// create is byte-for-byte the request sent before flight dates existed.
func TestCreateCampaign_NoFlightDatesKeepsTodaysBody(t *testing.T) {
	c, raw := captureAdGroupBody(t, &campaignsAPI{})
	res, err := c.CreateCampaign(context.Background(), validInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if strings.Contains(*raw, "StartDate") || strings.Contains(*raw, "EndDate") {
		t.Errorf("AddAdGroups body carries a flight date nobody asked for: %s", *raw)
	}
	if stepsContain(res.Steps, "flight dates") {
		t.Errorf("steps mention flight dates on a create without any: %v", res.Steps)
	}
}

// TestCreateCampaign_InvalidFlightDatesRefusedBeforeAnyRequest: every invalid date is a
// PreflightRefusal returned with a nil result and NO request sent — the ad group is created
// after the campaign, so a date discovered bad there would orphan the campaign.
func TestCreateCampaign_InvalidFlightDatesRefusedBeforeAnyRequest(t *testing.T) {
	cases := map[string]struct {
		start, end, wantErr string
	}{
		"bad start format":       {start: "2026/02/03", wantErr: "invalid start date format"},
		"bad end format":         {end: "Feb 3 2026", wantErr: "invalid end date format"},
		"impossible start":       {start: "2026-13-40", wantErr: "invalid start date"},
		"impossible end":         {start: "2026-02-01", end: "2026-02-30", wantErr: "invalid end date"},
		"start in the past":      {start: "2025-12-31", wantErr: "is in the past"},
		"end before start":       {start: "2026-03-01", end: "2026-02-01", wantErr: "must be after start date"},
		"end equal to start":     {start: "2026-03-01", end: "2026-03-01", wantErr: "must be after start date"},
		"end only, today":        {end: "2026-01-01", wantErr: "must be after today"},
		"end only, in the past":  {end: "2025-06-01", wantErr: "must be after today"},
		"past start, future end": {start: "2025-12-01", end: "2026-02-01", wantErr: "is in the past"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var reached reachFlag
			api := &campaignsAPI{}
			base := api.handler(t)
			c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
				reached.mark()
				base(w, r)
			})
			in := validInput()
			in.StartDate, in.EndDate = tc.start, tc.end
			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil {
				t.Fatalf("expected a flight-date refusal")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
			if _, ok := domain.AsPreflightRefusal(err); !ok {
				t.Errorf("error is not a PreflightRefusal: %v", err)
			}
			if res != nil {
				t.Errorf("an invalid date must fail cleanly (nil result), got %+v", res)
			}
			if reached.hit() {
				t.Error("a request was sent although the flight dates were invalid up front")
			}
		})
	}
}

// TestCreateCampaign_ReusedAdGroupKeepsItsSchedule: a retry that finds the ad group by name
// sends no ad-group create (so no dates) and says the dates were NOT applied.
func TestCreateCampaign_ReusedAdGroupKeepsItsSchedule(t *testing.T) {
	in := validInput()
	in.StartDate = "2026-02-03"
	in.EndDate = "2026-03-31"
	api := &campaignsAPI{
		adGroupGetBody: `{"AdGroups":[{"Id":111,"Name":` + jsonString(composeAdGroupName(in)) + `}]}`,
	}
	c, raw := captureAdGroupBody(t, api)
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if *raw != "" {
		t.Errorf("ad group create sent despite an existing ad group: %s", *raw)
	}
	if !stepsContain(res.Steps, "flight dates NOT applied to reused ad group 111") {
		t.Errorf("steps do not warn that the reused ad group kept its schedule: %v", res.Steps)
	}
}

func TestValidateFlightDates_TodayIsTheUTCDay(t *testing.T) {
	// 23:30 on 2026-01-01 in UTC-8 is already 2026-01-02 in UTC, so 2026-01-01 is past.
	now := time.Date(2026, 1, 1, 23, 30, 0, 0, time.FixedZone("PST", -8*3600))
	if _, err := validateFlightDates("2026-01-01", "", now); err == nil {
		t.Error("start 2026-01-01 accepted although the UTC day is 2026-01-02")
	}
	f, err := validateFlightDates(" 2026-01-02 ", "", now)
	if err != nil {
		t.Fatalf("start equal to the UTC day refused: %v", err)
	}
	if f.start == nil || *f.start != (msDate{Year: 2026, Month: 1, Day: 2}) || f.end != nil {
		t.Errorf("flight = %+v, want start 2026-01-02 and no end", f)
	}
}

func stepsContain(steps []string, sub string) bool {
	for _, s := range steps {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
