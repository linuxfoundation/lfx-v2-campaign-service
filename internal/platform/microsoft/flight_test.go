// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// The fixed test clock (fixedClock) is 2026-01-01T00:00:00Z, which is still 2025-12-31 at UTC-12,
// so the lenient "today" (earliestZone) is 2025-12-31.

// flightBodies records the raw AddCampaigns and AddAdGroups bodies. The handler writes them on
// the server goroutine, so every read goes through the mutex (the test-hygiene pattern
// httptest-handler-state-needs-synchronized-handoff).
type flightBodies struct {
	mu                 sync.Mutex
	adGroupB, campaign string
}

func (b *flightBodies) adGroup() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.adGroupB
}

func (b *flightBodies) campaignBody() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.campaign
}

// captureCreateBodies returns a client whose AddCampaigns and AddAdGroups POST bodies are
// recorded raw, with every route served by the shared campaignsAPI script (the ad group create
// answered with id 654).
func captureCreateBodies(t *testing.T, api *campaignsAPI) (*Client, *flightBodies) {
	t.Helper()
	bodies := &flightBodies{}
	base := api.handler(t)
	c := newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Campaigns") {
			b, _ := io.ReadAll(r.Body)
			bodies.mu.Lock()
			bodies.campaign = string(b)
			bodies.mu.Unlock()
			r.Body = io.NopCloser(strings.NewReader(string(b)))
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/AdGroups") {
			b, _ := io.ReadAll(r.Body)
			bodies.mu.Lock()
			bodies.adGroupB = string(b)
			bodies.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"AdGroupIds":[654],"PartialErrors":[]}`)
			return
		}
		base(w, r)
	})
	return c, bodies
}

// TestCreateCampaign_FlightDatesSentOnAdGroup pins the wire mapping: startDate/endDate reach
// the AddAdGroups body as AdGroup.StartDate/EndDate in Microsoft's {Month,Day,Year} object
// form — not as ISO strings, and not on the campaign.
func TestCreateCampaign_FlightDatesSentOnAdGroup(t *testing.T) {
	c, bodies := captureCreateBodies(t, &campaignsAPI{})
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
		if !strings.Contains(bodies.adGroup(), want) {
			t.Errorf("AddAdGroups body lacks %s: %s", want, bodies.adGroup())
		}
	}
	// The campaign create is unchanged: Microsoft campaigns carry no flight dates.
	if bodies.campaignBody() == "" {
		t.Fatal("campaign create body not captured")
	}
	if strings.Contains(bodies.campaignBody(), "StartDate") || strings.Contains(bodies.campaignBody(), "EndDate") {
		t.Errorf("campaign create carries flight dates; they belong on the ad group: %s", bodies.campaignBody())
	}
	if !res.FlightApplied {
		t.Error("FlightApplied = false on a run that created the ad group with dates")
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
			c, bodies := captureCreateBodies(t, &campaignsAPI{})
			in := validInput()
			in.StartDate, in.EndDate = tc.start, tc.end
			if _, err := c.CreateCampaign(context.Background(), in); err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			if !strings.Contains(bodies.adGroup(), tc.present) {
				t.Errorf("AddAdGroups body lacks %s: %s", tc.present, bodies.adGroup())
			}
			if strings.Contains(bodies.adGroup(), tc.absent) {
				t.Errorf("AddAdGroups body carries %s, which was not requested: %s", tc.absent, bodies.adGroup())
			}
		})
	}
}

// TestCreateCampaign_NoFlightDatesKeepsTodaysBody is the absent-dates guarantee: with neither
// date the ad group body carries no date field at all and no flight step is written, so the
// create is byte-for-byte the request sent before flight dates existed.
func TestCreateCampaign_NoFlightDatesKeepsTodaysBody(t *testing.T) {
	c, bodies := captureCreateBodies(t, &campaignsAPI{})
	res, err := c.CreateCampaign(context.Background(), validInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if strings.Contains(bodies.adGroup(), "StartDate") || strings.Contains(bodies.adGroup(), "EndDate") {
		t.Errorf("AddAdGroups body carries a flight date nobody asked for: %s", bodies.adGroup())
	}
	if res.FlightApplied {
		t.Error("FlightApplied = true on a create without dates")
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
		"start in the past":      {start: "2025-12-30", wantErr: "is in the past"},
		"end before start":       {start: "2026-03-01", end: "2026-02-01", wantErr: "must be after start date"},
		"end equal to start":     {start: "2026-03-01", end: "2026-03-01", wantErr: "must be after start date"},
		"end only, today":        {end: "2025-12-31", wantErr: "must be after today"},
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
	c, bodies := captureCreateBodies(t, api)
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if bodies.adGroup() != "" {
		t.Errorf("ad group create sent despite an existing ad group: %s", bodies.adGroup())
	}
	if res.FlightApplied {
		t.Error("FlightApplied = true although the ad group was reused and sent no dates")
	}
	if !stepsContain(res.Steps, "flight dates NOT applied to reused ad group 111") {
		t.Errorf("steps do not warn that the reused ad group kept its schedule: %v", res.Steps)
	}
}

// TestValidateFlightDates_TodayIsTheEarliestZonesDay: "today" is the calendar day at UTC-12, so
// a date is refused only once it is past in every zone. A pre-flight check must never refuse a
// date Microsoft would accept; Microsoft judges the campaign's own zone.
func TestValidateFlightDates_TodayIsTheEarliestZonesDay(t *testing.T) {
	// 2026-01-02 10:00 UTC: already 2026-01-02 in UTC, Pacific (02:00) and Tokyo (19:00), but
	// still 2026-01-01 at UTC-12 (22:00 the previous day).
	now := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	f, err := validateFlightDates(" 2026-01-01 ", "", now)
	if err != nil {
		t.Fatalf("start 2026-01-01 refused although it is still that day at UTC-12: %v", err)
	}
	if f.start == nil || *f.start != (msDate{Year: 2026, Month: 1, Day: 1}) || f.end != nil {
		t.Errorf("flight = %+v, want start 2026-01-01 and no end", f)
	}
	if _, err := validateFlightDates("2025-12-31", "", now); err == nil {
		t.Error("start 2025-12-31 accepted although it is past in every zone")
	}
	// End-only is judged against the same lenient today: after 2026-01-01, not after the UTC day.
	if _, err := validateFlightDates("", "2026-01-02", now); err != nil {
		t.Errorf("end-only 2026-01-02 refused although it is after today at UTC-12: %v", err)
	}
	if _, err := validateFlightDates("", "2026-01-01", now); err == nil {
		t.Error("end-only 2026-01-01 accepted although that is today at UTC-12")
	}
}

// TestCreateCampaign_TimeZoneDoesNotTightenToday: the rule is the same with or without a
// TimeZone. A US operator choosing their own today as the start — late in their evening, when
// the UTC day (and every zone east of it) has already moved on — is not refused up front.
func TestCreateCampaign_TimeZoneDoesNotTightenToday(t *testing.T) {
	for _, tz := range []string{"", "PacificTimeUSCanadaTijuana", "OsakaSapporoTokyo"} {
		t.Run("timeZone="+tz, func(t *testing.T) {
			c, bodies := captureCreateBodies(t, &campaignsAPI{})
			// 2026-01-02 06:00 UTC is 22:00 on 2026-01-01 in Pacific.
			WithClock(func() time.Time { return time.Date(2026, 1, 2, 6, 0, 0, 0, time.UTC) })(c)
			in := validInput()
			in.StartDate = "2026-01-01"
			in.TimeZone = tz
			if _, err := c.CreateCampaign(context.Background(), in); err != nil {
				t.Fatalf("CreateCampaign refused the operator's own today: %v", err)
			}
			if !strings.Contains(bodies.adGroup(), `"StartDate":{"Month":1,"Day":1,"Year":2026}`) {
				t.Errorf("AddAdGroups body lacks the start date: %s", bodies.adGroup())
			}
		})
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
