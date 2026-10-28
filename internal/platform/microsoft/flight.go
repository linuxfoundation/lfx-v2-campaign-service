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
// calendar-valid, an end strictly after the start, and a start that is not in the past. The
// shape is checked in the CreateCampaign prologue, before any request; the not-in-the-past rule
// just before a NEW campaign is created (checkNotPast says why not on a reuse). Either way a
// date that is wrong on its face fails with nothing created.
//
// "Today" is deliberately LENIENT, and that is the one departure from Meta. Microsoft reads the
// dates in the CAMPAIGN's time zone (Pacific by default, or the caller's TimeZone), and this
// client does not map Microsoft's zone enum to an offset. A pre-flight check must never refuse a
// date Microsoft would accept, so "today" is the calendar day at the EARLIEST offset on Earth
// (UTC-12, earliestZone), with or without a TimeZone: only a date that is already past in every
// zone is refused here. Microsoft stays the final judge. A start that is past in the campaign's
// own zone but not yet at UTC-12 (for example yesterday in Tokyo, or for a few hours each day
// yesterday in Pacific) passes this check and can still be refused at AddAdGroups, AFTER the
// campaign exists — that failure is reported like any other ad-group rejection, with the
// campaign in the partial result.

// flightDateLayout is the YYYY-MM-DD layout every flight date is given in.
const flightDateLayout = "2006-01-02"

// flightDateRE pins the exact YYYY-MM-DD shape before time.Parse runs, as the Meta client's
// dateRE does, so a malformed string and a well-formed impossible date get distinct refusals.
var flightDateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// earliestZone is the time zone whose calendar day is furthest behind (UTC-12, Baker Island):
// a date before today there is past in every zone.
var earliestZone = time.FixedZone("UTC-12", -12*60*60)

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

// time returns the Date object as midnight UTC on that calendar day.
func (d msDate) time() time.Time {
	return time.Date(d.Year, time.Month(d.Month), d.Day, 0, 0, 0, 0, time.UTC)
}

// String renders a Date object back as YYYY-MM-DD.
func (d msDate) String() string { return d.time().Format(flightDateLayout) }

// validateFlightDates checks the optional flight window's SHAPE and converts it to
// Microsoft's Date objects. Each date is independent: either, both or neither may be supplied.
//
//   - a supplied date must be YYYY-MM-DD and a real calendar date;
//   - with both, endDate must be after startDate.
//
// It does not look at the clock: whether the window is already in the past is checkNotPast's
// question, asked only before a NEW campaign is created (see CreateCampaign). Every refusal is
// a PreflightRefusal carrying the caller's value through domain.CallerValue, exactly as Meta's
// date refusals are.
func validateFlightDates(startDate, endDate string) (flight, error) {
	startDate = strings.TrimSpace(startDate)
	endDate = strings.TrimSpace(endDate)
	var f flight
	var start time.Time
	if startDate != "" {
		t, err := parseFlightDate("start", startDate)
		if err != nil {
			return flight{}, err
		}
		start = t
		d := toMSDate(t)
		f.start = &d
	}
	if endDate != "" {
		end, err := parseFlightDate("end", endDate)
		if err != nil {
			return flight{}, err
		}
		if startDate != "" && !end.After(start) {
			return flight{}, domain.PreflightRefusalf("microsoft-ads end date %s must be after start date %s", domain.CallerValue(endDate), domain.CallerValue(startDate))
		}
		d := toMSDate(end)
		f.end = &d
	}
	return f, nil
}

// checkNotPast refuses a window that is already over before it could start: a start before
// today, or — with no start, when Microsoft starts the ad group today — an end not after
// today. "Today" is the calendar day in earliestZone (UTC-12), so only a date past in every
// zone is refused; see the file comment.
//
// CreateCampaign asks this only when it is about to create a NEW campaign. On a retry that
// REUSES the campaign a previous attempt created, refusing here would return "nothing created"
// and release the claim on a campaign that exists upstream; there the dates go to Microsoft
// (when the ad group still has to be created) and its own verdict comes back as an ad-group
// rejection carrying the campaign, so the claim is retained for reconcile.
func (f flight) checkNotPast(now time.Time) error {
	y, m, d := now.In(earliestZone).Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	if f.start != nil {
		if f.start.time().Before(today) {
			return domain.PreflightRefusalf("microsoft-ads start date %s is in the past", domain.CallerValue(f.start.String()))
		}
		return nil // end > start >= today
	}
	if f.end != nil && !f.end.time().After(today) {
		return domain.PreflightRefusalf("microsoft-ads end date %s must be after today (%s) when no start date is given", domain.CallerValue(f.end.String()), today.Format(flightDateLayout))
	}
	return nil
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
