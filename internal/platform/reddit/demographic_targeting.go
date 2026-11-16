// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Gender, language and dayparting at create time (#549 gap 13). From Reddit's public v3 OpenAPI
// document (https://ads-api.reddit.com/api/v3/openapi.json, fetched 2026-11-14):
//
//   - components_schema_targeting.gender: "FEMALE" | "MALE" | null (null, or omitted, is every
//     gender). Sent as the ad group's targeting.gender.
//   - components_schema_targeting.languages: an array of components_enum_targeting_language_codes
//     (ISO 639-1, upper-case, 21 codes). Sent as the ad group's targeting.languages.
//   - schedule: an array of components_schema_time_block {start_day, start_hour, end_day,
//     end_hour}, days 0 (Sunday) to 6, hours 0-23. The CAMPAIGN's schedule is "only available when
//     is_campaign_budget_optimization is true, in which case it replaces the schedule of every ad
//     group", and every campaign this client creates is budget-optimized, so it is sent on the
//     campaign create. The hours are in the campaign's time zone.
//
// None of it has been sent live, which is why the dispatcher keeps the three behind
// REDDIT_GENDER_LANGUAGE_DAYPARTING_ENABLED. Two things the document does not say: whether a
// window's end hour is inclusive, and whether overlapping windows are accepted. This client sends
// windows exactly as given and refuses only what is certainly wrong (a field out of range or
// missing, a window that starts where it ends, an exact repeat).

const (
	// FieldGender, FieldLanguages and FieldSchedule are the redditConfig keys a DemographicError
	// names.
	FieldGender    = "gender"
	FieldLanguages = "languages"
	FieldSchedule  = "schedule"

	// MaxScheduleWindows bounds redditConfig.schedule. Reddit publishes no maxItems; 50 is this
	// service's bound, far more than a weekly daypart needs.
	MaxScheduleWindows = 50
)

var (
	// genderValues are the accepted CampaignInput.Gender values (components_schema_targeting).
	genderValues = []string{"FEMALE", "MALE"}
	// languageValues are components_enum_targeting_language_codes, verbatim.
	languageValues = []string{"AR", "CS", "DA", "DE", "EL", "EN", "ES", "FI", "FR", "HU", "IT", "JA", "KO", "NL", "NO", "PL", "PT", "RO", "SV", "VI", "ZH"}
)

// Genders returns the accepted gender values.
func Genders() []string { return slices.Clone(genderValues) }

// Languages returns the accepted language codes, in Reddit's enum order.
func Languages() []string { return slices.Clone(languageValues) }

// ScheduleWindow is one recurring weekly delivery window, as redditConfig.schedule carries it.
// Every field is a pointer so a missing one is refused rather than read as 0 (Sunday, midnight).
type ScheduleWindow struct {
	StartDay  *int `json:"startDay"`
	StartHour *int `json:"startHour"`
	EndDay    *int `json:"endDay"`
	EndHour   *int `json:"endHour"`
}

// DemographicError is a refused gender, languages or schedule input. Field is the redditConfig
// key and Msg the service-authored reason; any caller value in Msg went through
// domain.CallerValue.
type DemographicError struct {
	Field string
	Msg   string
}

func (e *DemographicError) Error() string { return e.Field + ": " + e.Msg }

// DemographicTargeting is what to send: Gender and Languages on the ad group's targeting,
// Schedule on the campaign. Zero values mean "not set" and send nothing.
type DemographicTargeting struct {
	Gender    string
	Languages []string
	Schedule  []map[string]int
}

// IsZero reports whether nothing is set, i.e. the create sends exactly what it did before gap 13.
func (t DemographicTargeting) IsZero() bool {
	return t.Gender == "" && len(t.Languages) == 0 && len(t.Schedule) == 0
}

// NormalizeDemographicTargeting validates the three choices and returns what to send. It is the
// ONE rule set: CreateCampaign applies it before any upstream call, and the dispatcher applies it
// in PreflightCreate so a bad value is a synchronous 400.
//
//   - gender: nil means every gender; otherwise trimmed and uppercased, it must be FEMALE or MALE.
//     A blank value is refused (omit the field for every gender).
//   - languages: trimmed and uppercased; each must be one of Languages(); a blank entry is
//     refused; duplicates are dropped; returned in Reddit's enum order. Empty means every language.
//   - schedule: at most MaxScheduleWindows windows; every field present, days 0-6, hours 0-23; a
//     window whose start equals its end is refused (it reads as both empty and the whole week),
//     and so is an exact repeat. Sent in the caller's order. Empty means any time.
func NormalizeDemographicTargeting(gender *string, languages []string, schedule []ScheduleWindow) (DemographicTargeting, error) {
	var out DemographicTargeting
	if gender != nil {
		g := strings.ToUpper(strings.TrimSpace(*gender))
		if g == "" {
			return DemographicTargeting{}, &DemographicError{Field: FieldGender, Msg: "an empty value is refused; omit the field to target every gender"}
		}
		if !slices.Contains(genderValues, g) {
			return DemographicTargeting{}, &DemographicError{Field: FieldGender, Msg: fmt.Sprintf("%s is not one of %s", domain.CallerValue(*gender), strings.Join(genderValues, ", "))}
		}
		out.Gender = g
	}
	langs, err := normalizeEnumList(FieldLanguages, languages, languageValues)
	if err != nil {
		var pe *PlacementError
		if errors.As(err, &pe) {
			return DemographicTargeting{}, &DemographicError{Field: pe.Field, Msg: pe.Msg}
		}
		return DemographicTargeting{}, err
	}
	out.Languages = langs
	if n := len(schedule); n > MaxScheduleWindows {
		return DemographicTargeting{}, &DemographicError{Field: FieldSchedule, Msg: fmt.Sprintf("holds %d windows; at most %d are accepted", n, MaxScheduleWindows)}
	}
	seen := make(map[[4]int]bool, len(schedule))
	for i, w := range schedule {
		if w.StartDay == nil || w.StartHour == nil || w.EndDay == nil || w.EndHour == nil {
			return DemographicTargeting{}, &DemographicError{Field: FieldSchedule, Msg: fmt.Sprintf("entry %d needs startDay, startHour, endDay and endHour", i+1)}
		}
		k := [4]int{*w.StartDay, *w.StartHour, *w.EndDay, *w.EndHour}
		if k[0] < 0 || k[0] > 6 || k[2] < 0 || k[2] > 6 {
			return DemographicTargeting{}, &DemographicError{Field: FieldSchedule, Msg: fmt.Sprintf("entry %d: days are 0 (Sunday) to 6 (Saturday)", i+1)}
		}
		if k[1] < 0 || k[1] > 23 || k[3] < 0 || k[3] > 23 {
			return DemographicTargeting{}, &DemographicError{Field: FieldSchedule, Msg: fmt.Sprintf("entry %d: hours are 0 to 23", i+1)}
		}
		if k[0] == k[2] && k[1] == k[3] {
			return DemographicTargeting{}, &DemographicError{Field: FieldSchedule, Msg: fmt.Sprintf("entry %d starts where it ends; give a window with a different end", i+1)}
		}
		if seen[k] {
			return DemographicTargeting{}, &DemographicError{Field: FieldSchedule, Msg: fmt.Sprintf("entry %d repeats an earlier window", i+1)}
		}
		seen[k] = true
		out.Schedule = append(out.Schedule, map[string]int{"start_day": k[0], "start_hour": k[1], "end_day": k[2], "end_hour": k[3]})
	}
	return out, nil
}
