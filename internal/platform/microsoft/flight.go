// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"regexp"
	"strings"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Flight dates (start / end of serving).
//
// Microsoft Advertising schedules delivery on the AD GROUP, not the campaign: the v13 AdGroup
// carries optional StartDate and EndDate elements, each a Date object ({Month,Day,Year}) — not
// an ISO-8601 string (see msDate in campaign.go). The Campaign object has no flight dates at
// all, which is why the campaign create sends none and the settings readback reports none.
//
// Both are optional on Add and keep Microsoft's own defaults when omitted: no StartDate means
// the ad group is eligible to serve from today, and no EndDate means it runs until a human
// stops it. Omitting both is exactly the request this client sent before flight dates existed.
//
// The input format and rules mirror the Meta client's (meta.Client.CreateCampaign): YYYY-MM-DD,
// calendar-valid, an end strictly after the start, and a start that is not in the past by the
// UTC calendar day. They are checked BEFORE the first mutating call, because the ad group is
// created only after the campaign: a date Microsoft refused at AddAdGroups would otherwise
// leave a campaign with no ad group behind.

// flightDateLayout is the YYYY-MM-DD layout every flight date is given in.
const flightDateLayout = "2006-01-02"

// flightDateRE pins the exact YYYY-MM-DD shape before time.Parse runs, as the Meta client's
// dateRE does, so a malformed string and a well-formed impossible date get distinct refusals.
var flightDateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// flight is the validated flight window sent on the ad group create. A nil field is omitted
// from the request, which leaves Microsoft's default for it in place.
type flight struct {
	start *msDate
	end   *msDate
}

// set reports whether either flight date was requested.
func (f flight) set() bool { return f.start != nil || f.end != nil }

// String renders the window for a step, e.g. "start 2026-11-01, end 2026-11-30"; an omitted
// side reads as Microsoft's default for it.
func (f flight) String() string {
	start, end := "today (Microsoft default)", "none (runs until stopped)"
	if f.start != nil {
		start = f.start.String()
	}
	if f.end != nil {
		end = f.end.String()
	}
	return "start " + start + ", end " + end
}

// String renders a Date object back as YYYY-MM-DD.
func (d msDate) String() string {
	return time.Date(d.Year, time.Month(d.Month), d.Day, 0, 0, 0, 0, time.UTC).Format(flightDateLayout)
}

// validateFlightDates checks the optional flight window and converts it to Microsoft's Date
// objects. Each date is independent: either, both or neither may be supplied. now is the
// client's clock; "today" is its UTC calendar day, as in the Meta client.
//
//   - a supplied date must be YYYY-MM-DD and a real calendar date;
//   - startDate must not be before today;
//   - endDate must be after startDate, or — with no startDate, when Microsoft starts the ad
//     group today — after today.
//
// Every refusal is a PreflightRefusal carrying the caller's value through domain.CallerValue,
// exactly as Meta's date refusals are.
func validateFlightDates(startDate, endDate string, now time.Time) (flight, error) {
	startDate = strings.TrimSpace(startDate)
	endDate = strings.TrimSpace(endDate)
	if startDate == "" && endDate == "" {
		return flight{}, nil
	}
	today := now.UTC().Truncate(24 * time.Hour)

	var f flight
	effectiveStart := today
	if startDate != "" {
		start, err := parseFlightDate("start", startDate)
		if err != nil {
			return flight{}, err
		}
		if start.Before(today) {
			return flight{}, domain.PreflightRefusalf("microsoft-ads start date %s is in the past", domain.CallerValue(startDate))
		}
		d := toMSDate(start)
		f.start = &d
		effectiveStart = start
	}
	if endDate != "" {
		end, err := parseFlightDate("end", endDate)
		if err != nil {
			return flight{}, err
		}
		if !end.After(effectiveStart) {
			if startDate != "" {
				return flight{}, domain.PreflightRefusalf("microsoft-ads end date %s must be after start date %s", domain.CallerValue(endDate), domain.CallerValue(startDate))
			}
			return flight{}, domain.PreflightRefusalf("microsoft-ads end date %s must be after today (%s) when no start date is given", domain.CallerValue(endDate), today.Format(flightDateLayout))
		}
		d := toMSDate(end)
		f.end = &d
	}
	return f, nil
}

// parseFlightDate parses one YYYY-MM-DD flight date. which is "start" or "end", for the
// refusal text. The format check and the calendar check are separate refusals, as in the
// Meta client: 2026-13-40 is well-formed but not a date.
func parseFlightDate(which, s string) (time.Time, error) {
	if !flightDateRE.MatchString(s) {
		return time.Time{}, domain.PreflightRefusalf("microsoft-ads invalid %s date format: %s — expected YYYY-MM-DD", which, domain.CallerValue(s))
	}
	t, err := time.Parse(flightDateLayout, s)
	if err != nil {
		return time.Time{}, domain.PreflightRefusalf("microsoft-ads invalid %s date %s — expected a real calendar date in YYYY-MM-DD format", which, domain.CallerValue(s))
	}
	return t, nil
}
