// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// utcMs is 00:00 UTC (eod=false) or 23:59:59.999 UTC (eod=true) of a YYYY-MM-DD day, the instants
// toMs sends.
func utcMs(t *testing.T, day string, eod bool) int64 {
	t.Helper()
	d, err := time.ParseInLocation("2006-01-02", day, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if eod {
		d = d.Add(24*time.Hour - time.Millisecond)
	}
	return d.UnixMilli()
}

// TestValidateFixedScheduleLifetime pins #633: an uncapped lifetime budget on a fixed schedule
// must cover max($100, $10 × days), days counted start through end inclusive in UTC, against the
// rounded wire value.
func TestValidateFixedScheduleLifetime(t *testing.T) {
	for _, tc := range []struct {
		name       string
		total      float64
		start, end string
		wantReason string
	}{
		// Prod campaign 930195766: $100 over 18 Nov–18 Dec derived $3.23/day.
		{name: "930195766: $100 over 31 days", total: 100, start: "2026-11-18", end: "2026-12-18", wantReason: "lifetime budget 100.00 is below LinkedIn's minimum of $310 for a 31-day flight from 2026-11-18 through 2026-12-18 (UTC)"},
		{name: "31 days at $309", total: 309, start: "2026-11-18", end: "2026-12-18", wantReason: "minimum of $310 for a 31-day flight"},
		{name: "31 days at $310", total: 310, start: "2026-11-18", end: "2026-12-18"},
		{name: "rounds up to the floor", total: 309.996, start: "2026-11-18", end: "2026-12-18"},
		{name: "rounds down below the floor", total: 309.994, start: "2026-11-18", end: "2026-12-18", wantReason: "lifetime budget 309.99 is below"},
		// One day: $10 × 1 is under the flat lifetime minimum, which binds and is named.
		{name: "one-day flight at $100", total: 100, start: "2026-11-18", end: "2026-11-18"},
		{name: "one-day flight under $100", total: 99, start: "2026-11-18", end: "2026-11-18", wantReason: "minimum of $100 for a 1-day flight"},
		{name: "ten days at $100", total: 100, start: "2026-11-01", end: "2026-11-10"},
		{name: "eleven days at $100", total: 100, start: "2026-11-01", end: "2026-11-11", wantReason: "minimum of $110 for a 11-day flight"},
		{name: "across a month boundary and DST", total: 589, start: "2026-02-01", end: "2026-03-31", wantReason: "minimum of $590"},
		{name: "a non-finite total is left to ValidateBudgetAmount", total: math.NaN(), start: "2026-11-18", end: "2026-12-18"},
		{name: "a zero total is left to ValidateBudgetAmount", total: 0, start: "2026-11-18", end: "2026-12-18"},
		{name: "a reversed schedule is left to validateSchedule", total: 1, start: "2026-12-18", end: "2026-11-18"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateFixedScheduleLifetime(tc.total, utcMs(t, tc.start, false), utcMs(t, tc.end, true))
			if tc.wantReason == "" {
				if err != nil {
					t.Fatalf("ValidateFixedScheduleLifetime = %v, want accepted", err)
				}
				return
			}
			reason, ok := BudgetAmountReason(err)
			if !ok || !errors.Is(err, ErrBudgetAmountInvalid) || !strings.Contains(reason, tc.wantReason) {
				t.Fatalf("err = %v (reason %q); want a budget-amount refusal containing %q", err, reason, tc.wantReason)
			}
		})
	}
}

// TestFlightDaysUTC pins the day count: a start nudged into the day still counts that day, and
// the end day counts whole.
func TestFlightDaysUTC(t *testing.T) {
	nudged := time.Date(2026, 11, 18, 22, 30, 0, 0, time.UTC).UnixMilli()
	if got := flightDaysUTC(nudged, utcMs(t, "2026-12-18", true)); got != 31 {
		t.Errorf("nudged start: %d days, want 31", got)
	}
	if got := flightDaysUTC(utcMs(t, "2026-11-18", false), utcMs(t, "2026-11-18", true)); got != 1 {
		t.Errorf("same day: %d days, want 1", got)
	}
}

// TestValidateFixedScheduleLifetimeDates pins the preflight form: a start already past counts
// from today, as toMs will send it.
func TestValidateFixedScheduleLifetimeDates(t *testing.T) {
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	if err := ValidateFixedScheduleLifetimeDates(309, "2026-11-18", "2026-12-18", now); err == nil {
		t.Error("$309 over 31 days was accepted")
	}
	if err := ValidateFixedScheduleLifetimeDates(310, "2026-11-18", "2026-12-18", now); err != nil {
		t.Errorf("$310 over 31 days: %v", err)
	}
	// Started on 1 Nov: LinkedIn receives a start of today, so 18 Nov–18 Dec is 31 days, not 48.
	if err := ValidateFixedScheduleLifetimeDates(310, "2026-11-01", "2026-12-18", now); err != nil {
		t.Errorf("a past start must count from today: %v", err)
	}
	// Just before UTC midnight the nudged start (now+startTimeBuffer) lands tomorrow, so the
	// flight is 19 Nov–18 Dec, 30 days, and $300 covers it.
	late := time.Date(2026, 11, 18, 23, 55, 0, 0, time.UTC)
	if err := ValidateFixedScheduleLifetimeDates(300, "2026-11-18", "2026-12-18", late); err != nil {
		t.Errorf("a start nudged past midnight must count from tomorrow: %v", err)
	}
	for _, bad := range [][2]string{{"nope", "2026-12-18"}, {"2026-11-18", "2026-13-40"}, {"2026-11-18", "2026-11-01"}} {
		if err := ValidateFixedScheduleLifetimeDates(1, bad[0], bad[1], now); err != nil {
			t.Errorf("dates %v are validateSchedule's to refuse, got %v", bad, err)
		}
	}
}

// TestCreateCampaign_RefusesLifetimeUnderFlightFloor pins the client: an uncapped fixed-schedule
// lifetime total under the floor is a pre-flight refusal with nothing POSTed, while the same total
// with a #584 daily cap (continuous schedule) is not held to it.
func TestCreateCampaign_RefusesLifetimeUnderFlightFloor(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			return
		}
		posts.Add(1)
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			_, _ = io.WriteString(w, `{"id":"200"}`)
		case strings.Contains(r.URL.Path, "posts"):
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"400"}`)
		}
	}))
	defer srv.Close()
	input := func(total, capUSD float64) CampaignInput {
		return CampaignInput{
			EventName: "E", Project: "tlf", RegistrationURL: "https://x.org",
			BudgetUSD: total, LifetimeBudget: true, DailyCapUSD: capUSD,
			StartDate: "2099-01-01", EndDate: "2099-01-31", TargetingProfile: "cloud-native",
			GeoTargets: []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
			Variants:   []CreativeVariant{{IntroText: "a", Headline: "b"}},
		}
	}
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))

	_, err := c.CreateCampaign(context.Background(), input(309, 0))
	if msg, ok := domain.AsPreflightRefusal(err); !ok || !strings.Contains(msg, "minimum of $310 for a 31-day flight") {
		t.Fatalf("CreateCampaign($309 over 31 days) err = %v, want a pre-flight refusal naming $310", err)
	}
	if n := posts.Load(); n != 0 {
		t.Fatalf("%d POSTs for a refused budget; nothing may be created", n)
	}
	if _, err := c.CreateCampaign(context.Background(), input(310, 0)); err != nil {
		t.Fatalf("CreateCampaign($310 over 31 days): %v", err)
	}
	if _, err := c.CreateCampaign(context.Background(), input(150, 10)); err != nil {
		t.Fatalf("a capped lifetime budget must not be held to the fixed-schedule floor: %v", err)
	}
}
