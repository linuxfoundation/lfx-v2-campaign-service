// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"unicode"
)

// ---------------------------------------------------------------------------
// Campaign criteria beyond place (LFXV2-2665).
//
// A Search campaign this service created could say WHERE it serves (geo.go) and
// what it must not match (negative keywords), and nothing else: it ran in every
// language, around the clock, on every device, for every age and gender. Those
// are not exotic settings — they are the four a human sets by hand in the Google
// Ads UI before un-pausing an event campaign, which is the gap this file closes.
//
// All four become CampaignCriterion resources, so they share one
// campaignCriteria:mutate — see createCampaignTargetingCriteria for why that
// batch is atomic and why it is separate from geo's.
//
// Demand Gen splits that level; validateCriteriaPlan says which kinds each channel
// takes. It refuses a campaign-level language, so its languages go on the AD GROUP
// (#459, createDemandGenAdGroupTargeting; geo.go has the same split), while its ad
// schedules and device criteria are CAMPAIGN criteria sent here (#474). Both were
// verified by validateOnly probes on 2026-11-03. Demographics are refused there at
// preflight: Google refuses them on the audience-grouped ad group, so they belong in
// the Audience. Refusing locally is free; discovering it after a paid campaign exists
// is not, and silently dropping the caller's intent is the defect LFXV2-3283 already
// fixed once for countries. Lift this per criterion as each is confirmed live.
//
// The income-range and parental-status exclusions at the end of this file (#473) are
// another exception: they are AD GROUP criteria with their own channel fence — see
// validateAdGroupDemographicExclusions.
// ---------------------------------------------------------------------------

// languageConstants maps an ISO 639-1 language code to its Google Ads language
// constant id.
//
// Curated for the same reason geoTargetConstants is: these ids are Google's, are
// NOT derivable from the ISO code by any rule, and a wrong transcription targets
// the WRONG LANGUAGE while looking perfectly valid. The map holds the codes this
// transcription is confident in; anything else is reachable as a raw numeric
// constant id, exactly as a city is on the geo side.
var languageConstants = map[string]string{
	"EN": "1000", // English
	"DE": "1001", // German
	"FR": "1002", // French
	"ES": "1003", // Spanish
	"IT": "1004", // Italian
	"JA": "1005", // Japanese
	"DA": "1009", // Danish
	"NL": "1010", // Dutch
	"FI": "1011", // Finnish
	"KO": "1012", // Korean
	"NO": "1013", // Norwegian
	"PT": "1014", // Portuguese
	"SV": "1015", // Swedish
	"AR": "1019", // Arabic
	"BG": "1020", // Bulgarian
	"CS": "1021", // Czech
	"EL": "1022", // Greek
	"HI": "1023", // Hindi
	"HU": "1024", // Hungarian
	"ID": "1025", // Indonesian
	"IS": "1026", // Icelandic
	"HE": "1027", // Hebrew
	"LV": "1028", // Latvian
	"LT": "1029", // Lithuanian
	"PL": "1030", // Polish
	"RU": "1031", // Russian
	"RO": "1032", // Romanian
	"SK": "1033", // Slovak
	"SL": "1034", // Slovenian
	"SR": "1035", // Serbian
	"UK": "1036", // Ukrainian
	"TR": "1037", // Turkish
	"CA": "1038", // Catalan
	"HR": "1039", // Croatian
	"VI": "1040", // Vietnamese
	"UR": "1041", // Urdu
	"TL": "1042", // Filipino
	"ET": "1043", // Estonian
	"TH": "1044", // Thai
}

// maxLanguages bounds the language list. A campaign naming more than this many
// languages is targeting nothing in particular, and the cap keeps the criteria
// within one mutate. Sized well above any real event campaign for the reason the
// 2026-08-13 keyword incident gives — a cap the product's own callers exceed by
// default refuses creates Google would have accepted.
const maxLanguages = 40

// maxAdSchedules bounds the ad-schedule list. Google itself permits at most 6
// intervals per day of the week, so 42 is the upstream ceiling rather than a
// tighter broker opinion: nothing Google would accept is refused here.
const maxAdSchedules = 42

// maxAdSchedulesPerDay is the half of that ceiling the global cap cannot express.
// 42 is 6x7, so a list that satisfies it can still put seven intervals on Monday
// and none on Sunday — refused upstream, and refused only at the criteria mutate,
// after the budget and campaign are committed. The per-day limit is Google's own,
// so enforcing it here refuses nothing Google would have accepted; it only moves
// the refusal to where it costs nothing.
const maxAdSchedulesPerDay = 6

// maxDemographicExclusions bounds EACH demographic exclusion list. Both are
// closed enums far smaller than this; the cap exists so a malformed caller cannot
// push an unbounded payload into the mutate.
const maxDemographicExclusions = 20

// Bid-modifier bounds. These are Google's own documented device/schedule bid
// adjustment range (-100%..+900%), not a broker opinion: 0.1 halves-and-then-some,
// 10.0 is +900%, and exactly 0 is the "do not serve here at all" opt-out. A value
// strictly between 0 and minBidModifier is what Google rejects, so this guard
// mirrors upstream rather than narrowing it.
const (
	minBidModifier = 0.1
	maxBidModifier = 10.0
)

// adScheduleDays is the DayOfWeek enum Google accepts on an AdScheduleInfo.
var adScheduleDays = map[string]struct{}{
	"MONDAY": {}, "TUESDAY": {}, "WEDNESDAY": {}, "THURSDAY": {},
	"FRIDAY": {}, "SATURDAY": {}, "SUNDAY": {},
}

// adScheduleMinutes maps the only four minute values Google's MinuteOfHour enum
// admits to their enum names. Google models minutes as an ENUM, not an integer,
// so a schedule starting at :10 is not a near miss — it is unrepresentable, and
// saying so locally is clearer than the upstream enum-parse error.
var adScheduleMinutes = map[int]string{
	0: "ZERO", 15: "FIFTEEN", 30: "THIRTY", 45: "FORTY_FIVE",
}

// deviceTypes is the Device enum this client accepts on every channel but Demand Gen,
// which takes demandGenDeviceTypes. Two of Google's values are absent here, for
// different reasons.
//
// OTHER is a reporting bucket rather than something a campaign bids on.
//
// CONNECTED_TV is a NAMED GAP, and the reasoning behind it changed when this client gained
// the two channels Google supports it on. Google's own device-targeting documentation says
// of TV screens: "This targeting option is only available for Display and Video campaigns."
// While Search was the only kind that reached this map — device criteria were refused
// outright on Demand Gen until #474 — the omission restated an upstream rule and cost a
// caller nothing. Video and Display now reach it too, and on those two Google DOES accept
// the device, so on those two the omission is this client's limitation rather than
// Google's, and is stated as one — the same distinction displayBiddingStrategies draws
// about manual CPC, and for the same reason: a reader widening this set later needs to know
// which of the two they are arguing with.
//
// It stays closed because opening it is a live-API question, not a code-reading one.
// What is NOT established from the documentation alone is whether a CONNECTED_TV
// campaign criterion carries a BID MODIFIER on those channels, and this client has no
// way to send a criterion without one. Guessing wrong lands at the
// campaignCriteria:mutate, which runs AFTER the budget and campaign are committed —
// precisely the orphan the preflight exists to prevent — and the only reachable Google
// account is a production one, where a validateOnly mutate is still a POST. Under-
// refusing is safe; stranding a paid campaign is not.
//
// Closing it means running that validateOnly campaignCriteria:mutate for CONNECTED_TV
// on a non-production DISPLAY and VIDEO campaign and admitting the device here for
// those kinds alone — Search, which Google does not support it on, keeps refusing it.
// Demand Gen is the one channel where that probe HAS been run (#474), and it contradicts
// the quote above: Google accepted a CONNECTED_TV criterion on a Demand Gen campaign,
// which the "Display and Video campaigns only" sentence says it would not. So that
// documentation is not a reliable boundary in either direction; see demandGenDeviceTypes.
//
// A device list is capped at len() of its channel's vocabulary: a longer list must
// contain a duplicate, which Google rejects with a criterion conflict AFTER the
// campaign exists.
var deviceTypes = map[string]struct{}{
	"MOBILE": {}, "DESKTOP": {}, "TABLET": {},
}

// demandGenDeviceTypes is deviceTypes plus CONNECTED_TV, Demand Gen's vocabulary (#474).
// A validateOnly probe on 2026-11-03 accepted a Demand Gen campaign criterion
// {device: {type: CONNECTED_TV}, bidModifier: 0}, and the same with MOBILE — the TV-screen
// exclusion the live KubeCon NA 2026 Demand Gen campaigns carry. Only bid modifier 0 was
// probed, so validateCriteriaPlan refuses any other modifier on this channel.
var demandGenDeviceTypes = map[string]struct{}{
	"MOBILE": {}, "DESKTOP": {}, "TABLET": {}, "CONNECTED_TV": {},
}

// deviceTypesFor is the device vocabulary for a campaign kind.
func deviceTypesFor(kind string) map[string]struct{} {
	if kind == campaignKindDemandGen {
		return demandGenDeviceTypes
	}
	return deviceTypes
}

// ageRangeTypes maps both the friendly spelling a caller is likely to write and
// Google's own enum name to the enum. Both spellings are accepted because the
// config blob is hand-written by whoever builds a brief, and "18-24" failing
// while "AGE_RANGE_18_24" succeeds is a trap with no upside.
var ageRangeTypes = map[string]string{
	"18-24": "AGE_RANGE_18_24", "AGE_RANGE_18_24": "AGE_RANGE_18_24",
	"25-34": "AGE_RANGE_25_34", "AGE_RANGE_25_34": "AGE_RANGE_25_34",
	"35-44": "AGE_RANGE_35_44", "AGE_RANGE_35_44": "AGE_RANGE_35_44",
	"45-54": "AGE_RANGE_45_54", "AGE_RANGE_45_54": "AGE_RANGE_45_54",
	"55-64": "AGE_RANGE_55_64", "AGE_RANGE_55_64": "AGE_RANGE_55_64",
	"65+": "AGE_RANGE_65_UP", "AGE_RANGE_65_UP": "AGE_RANGE_65_UP",
	"UNDETERMINED": "AGE_RANGE_UNDETERMINED", "AGE_RANGE_UNDETERMINED": "AGE_RANGE_UNDETERMINED",
}

// genderTypes is the Gender enum. UNDETERMINED is the bucket Google puts users it
// could not classify into, and excluding it is a real choice a campaign makes —
// so it is accepted rather than treated as a non-value.
var genderTypes = map[string]struct{}{
	"MALE": {}, "FEMALE": {}, "UNDETERMINED": {},
}

// AdSchedule is one "serve during this window on this day" interval.
//
// The window is half-open — [start, end) — which is why EndHour admits 24 and
// StartHour does not: a schedule running to midnight ends at hour 24, and a
// schedule starting at midnight starts at hour 0.
type AdSchedule struct {
	// DayOfWeek is MONDAY..SUNDAY, case-insensitive.
	DayOfWeek string
	// StartHour is 0..23 and EndHour 1..24, with the minutes restricted to the four
	// quarter-hour values Google's enum admits (0, 15, 30, 45).
	StartHour   int
	StartMinute int
	EndHour     int
	EndMinute   int
	// BidModifier is a POINTER because 0 is a meaningful value here — it is the
	// -100% opt-out — so it cannot double as "unset" the way CampaignInput.CPCBid's
	// zero does. nil means the interval restricts WHEN the campaign serves without
	// changing what it bids, which is the ordinary case.
	BidModifier *float64
}

// DeviceBidModifier is a per-device bid adjustment, or a device exclusion.
//
// BidModifier is REQUIRED and plain (not a pointer): an entry exists only to
// change the bid on that device, so there is no "unset" to express — a caller
// that wants no adjustment omits the entry entirely. 0 is the opt-out, which is
// how a campaign stops serving on tablets.
type DeviceBidModifier struct {
	Device      string
	BidModifier float64
}

// languageInfo, adScheduleInfo, deviceInfo, ageRangeInfo and genderInfo are the
// criterion payloads. None of the int fields carries omitempty: hour 0 and minute
// enum ZERO are real values, and an omitted startHour is a different schedule.
type languageInfo struct {
	LanguageConstant string `json:"languageConstant"`
}

type adScheduleInfo struct {
	DayOfWeek   string `json:"dayOfWeek"`
	StartHour   int    `json:"startHour"`
	StartMinute string `json:"startMinute"`
	EndHour     int    `json:"endHour"`
	EndMinute   string `json:"endMinute"`
}

type deviceInfo struct {
	Type string `json:"type"`
}

type ageRangeInfo struct {
	Type string `json:"type"`
}

type genderInfo struct {
	Type string `json:"type"`
}

// criteriaPlan is the whole resolved non-location targeting intent for one
// campaign, in the shapes the mutate sends. One value rather than five
// parameters because all five become operations in the SAME mutate.
type criteriaPlan struct {
	languages   []languageInfo
	schedules   []adScheduleInfo
	scheduleMod []*float64
	devices     []deviceInfo
	deviceMod   []float64
	ageRanges   []ageRangeInfo
	genders     []genderInfo
}

func (p criteriaPlan) empty() bool { return p.count() == 0 }

func (p criteriaPlan) count() int {
	return len(p.languages) + len(p.schedules) + len(p.devices) + len(p.ageRanges) + len(p.genders)
}

// validateCriteriaPlan resolves and bounds every non-location criterion, for the
// given campaign KIND. Called from preflightCampaignKind, so everything here
// fails BEFORE the first budget mutate and cannot orphan a paid resource; and
// because ValidateCampaignInput runs the same preflight, the adoption path
// refuses exactly what the create path would.
//
// It mutates nothing and sends nothing — no id is looked up upstream, so a
// numeric constant that names no language is caught by Google rather than here.
// That is the same trade resolveGeoEntry documents: a lookup would make
// ValidateCampaignInput send a request, which its contract forbids.
func validateCriteriaPlan(kind string, in CampaignInput) (criteriaPlan, error) {
	// Demand Gen takes four of the five kinds, at two levels, each verified by a
	// validateOnly probe on 2026-11-03. Languages go on its AD GROUP (#459): the ad group
	// accepts `language` criteria next to its audience and the campaign refuses them, so
	// the cascade attaches plan.languages there (createDemandGenAdGroupTargeting). Ad
	// schedules and devices go on the CAMPAIGN (#474), through
	// createCampaignTargetingCriteria with the languages left out.
	//
	// Only the probed shapes pass. The schedule probe carried no bid modifier and the
	// device probe only bidModifier 0, so a schedule modifier and a non-zero device
	// modifier are refused here. Demographics are refused whole: Google refuses age,
	// gender, income and parental criteria on the audience-grouped ad group
	// (CANNOT_ADD_AUDIENCE_SEGMENT_CRITERION_WHEN_AUDIENCE_GROUPED_IS_SET), and the
	// campaign level was not probed, so they belong inside the Audience instead.
	if kind == campaignKindDemandGen {
		if len(in.ExcludedAgeRanges)+len(in.ExcludedGenders) > 0 {
			return criteriaPlan{}, fmt.Errorf("google-ads: demographic exclusions are not supported on %s (Google refuses demographic criteria on the audience-grouped Demand Gen ad group); omit ExcludedAgeRanges and ExcludedGenders, and set the demographics inside the Audience that DemandGenAudience or the ad group's Audience references", kind)
		}
		for i, sch := range in.AdSchedules {
			if sch.BidModifier != nil {
				return criteriaPlan{}, fmt.Errorf("google-ads: ad schedule %d carries a bid modifier, which is not supported on %s (this client has verified a Demand Gen ad schedule only without one); omit its BidModifier", i, kind)
			}
		}
		for i, d := range in.DeviceBidModifiers {
			if d.BidModifier != 0 {
				return criteriaPlan{}, fmt.Errorf("google-ads: device bid modifier %d adjusts the bid by %v, which is not supported on %s (this client has verified only the device exclusion there); set it to 0 to exclude the device, or omit it", i, d.BidModifier, kind)
			}
		}
	}
	// Performance Max SPLITS this set rather than refusing it whole, which is why it has
	// its own arm rather than sharing one with Demand Gen.
	// Google documents LANGUAGE, LOCATION and AD_SCHEDULE as campaign criteria on
	// Performance Max, so refusing a language there would be the over-refusal these
	// guards exist to avoid — it would reject a campaign Google creates happily.
	//
	// Device BID MODIFIERS and campaign-level demographic exclusions are the half that
	// is genuinely absent: Performance Max sets no device bid adjustments (the
	// automated strategy owns the bid entirely), and this client attaches no demographic
	// exclusion there. Demographics can reach Performance Max as dimensions of an
	// Audience used as an asset group signal (#461), but a signal is a hint and not an
	// exclusion, so it is no substitute. Both are refused, not dropped, so an operator
	// never reads "campaign created" while an exclusion they asked for is nowhere
	// upstream.
	// Video and Display both take the UN-NARROWED set — every one of the five kinds,
	// exactly as Search does — and that is why the Demand Gen arm above names one channel
	// rather than every non-Search one. A VIDEO or DISPLAY campaign carries LANGUAGE,
	// AD_SCHEDULE, DEVICE (with a bid modifier) and campaign-level AGE_RANGE/GENDER
	// exclusions as campaign criteria, and video.go and display.go each attach all five on
	// their criteria step, so nothing accepted here is dropped. Widening the Demand Gen
	// refusals to `!= Search` would have refused a campaign Google creates happily — the
	// over-refusal these guards exist to avoid.
	// This arm is about the CHANNEL, not about automated bidding — Performance Max takes
	// no device criteria at all, not even the 0 exclusion, so it refuses the whole list.
	// The adjustment-is-ignored-under-automated-bidding rule is a different rule with a
	// different shape, it applies to Video, Display and an automated Search campaign too,
	// and it lives in validateBiddingPlan where the strategy is actually known. Do not
	// fold one into the other: widening this one to every automated channel would refuse
	// device EXCLUSIONS that Google honours.
	if kind == campaignKindPerformanceMax {
		if len(in.DeviceBidModifiers) > 0 {
			return criteriaPlan{}, fmt.Errorf("google-ads: device bid modifiers are not supported on %s (its automated bidding owns the bid and sets no device adjustment); omit DeviceBidModifiers, or create a Search campaign to bid by device", kind)
		}
		if len(in.ExcludedAgeRanges)+len(in.ExcludedGenders) > 0 {
			return criteriaPlan{}, fmt.Errorf("google-ads: campaign-level demographic exclusions are not supported on %s (this client attaches no demographic exclusion there; an audience signal can carry demographics, but as a hint, not an exclusion); omit ExcludedAgeRanges and ExcludedGenders, or create a Search campaign to exclude demographics", kind)
		}
	}

	var plan criteriaPlan

	languageIDs, err := resolveLanguageList(in.Languages)
	if err != nil {
		return criteriaPlan{}, err
	}
	for _, id := range languageIDs {
		plan.languages = append(plan.languages, languageInfo{LanguageConstant: languageResource(id)})
	}

	schedules, modifiers, err := validateAdSchedules(in.AdSchedules)
	if err != nil {
		return criteriaPlan{}, err
	}
	plan.schedules, plan.scheduleMod = schedules, modifiers

	devices, deviceMods, err := validateDeviceBidModifiers(kind, in.DeviceBidModifiers)
	if err != nil {
		return criteriaPlan{}, err
	}
	plan.devices, plan.deviceMod = devices, deviceMods

	ageRanges, err := validateExcludedAgeRanges(in.ExcludedAgeRanges)
	if err != nil {
		return criteriaPlan{}, err
	}
	plan.ageRanges = ageRanges

	genders, err := validateExcludedGenders(in.ExcludedGenders)
	if err != nil {
		return criteriaPlan{}, err
	}
	plan.genders = genders

	return plan, nil
}

// resolveLanguageList resolves ISO 639-1 codes and raw numeric language constant
// ids to constant ids, deduping by the RESOLVED id so "EN" and "1000" collapse.
// Entries are told apart by SHAPE, as on the geo side: an ISO 639-1 code is
// letters, a constant id is all digits, so no input is ambiguous — and, also as
// on the geo side, a numeric entry must be the CANONICAL spelling of its id, so
// that the dedupe cannot be defeated by a leading zero.
func resolveLanguageList(languages []string) ([]string, error) {
	if len(languages) == 0 {
		return nil, nil
	}
	if len(languages) > maxLanguages {
		return nil, fmt.Errorf("google-ads: at most %d languages are supported, got %d", maxLanguages, len(languages))
	}
	seen := make(map[string]struct{}, len(languages))
	out := make([]string, 0, len(languages))
	for _, l := range languages {
		entry := strings.ToUpper(strings.TrimSpace(l))
		if entry == "" {
			return nil, fmt.Errorf("google-ads: language must not be empty")
		}
		var id string
		switch {
		case numericID(entry):
			// Digits-only is not the id test — the same three locally-decidable faults
			// resolveGeoEntry refuses apply here: "0" names nothing, "01000" is a
			// non-canonical spelling of 1000 that would send TWO criteria for English,
			// and a 21-digit run overflows the int64 Google exposes these ids as.
			// canonicalCampaignID is reused for the same reason it is there — it is this
			// package's answer for this class of value, and collapsing every spelling to
			// one is what makes the dedupe below correct. Existence is still Google's to
			// decide: a well-formed id naming no language is refused upstream.
			if canonicalCampaignID(entry) == "" {
				return nil, fmt.Errorf("google-ads: language %q is not the canonical base-10 spelling of a positive language constant id", entry)
			}
			id = entry
		default:
			mapped, ok := languageConstants[entry]
			if !ok {
				return nil, fmt.Errorf("google-ads: language %q is neither a supported ISO 639-1 code (e.g. EN, DE, JA) nor a numeric language constant id from Google's language-constants table", entry)
			}
			id = mapped
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// languageResource renders a language constant id as the resource name a
// criterion carries.
func languageResource(id string) string {
	return "languageConstants/" + id
}

// validateAdSchedules bounds and normalises the schedule list, returning the
// payloads and their per-interval bid modifiers positionally — the two slices are
// always the same length, so index i of one belongs with index i of the other.
//
// Intervals are checked for OVERLAP within a day, and counted against Google's
// per-day limit. Both are locally decidable and both are refused upstream, so
// leaving them to Google means discovering a typo only at the criteria mutate,
// after the budget and the campaign are committed.
//
// The half-open window is what makes the overlap question decidable, and the test
// is written to match it exactly: `startA < endB && startB < endA`. 09:00-12:00
// and 12:00-17:00 therefore do NOT overlap — 12:00 is not before 12:00 — so the
// ordinary split-day schedule a naive "do they touch" test would refuse is still
// accepted. That is the over-refusal this file cares about, and it is pinned by a
// test rather than left to the reader.
//
// Comparison is on MINUTES OF THE DAY, which is also what the empty-window check
// above uses, so 24:00 is 1440 and sorts after every real end. An exact repeat is
// collapsed BEFORE this test runs: a duplicate is one criterion written twice, not
// two criteria that overlap, and reporting it as an overlap would name the wrong
// defect.
func validateAdSchedules(schedules []AdSchedule) ([]adScheduleInfo, []*float64, error) {
	if len(schedules) == 0 {
		return nil, nil, nil
	}
	if len(schedules) > maxAdSchedules {
		return nil, nil, fmt.Errorf("google-ads: at most %d ad schedules are supported, got %d", maxAdSchedules, len(schedules))
	}
	out := make([]adScheduleInfo, 0, len(schedules))
	mods := make([]*float64, 0, len(schedules))
	// Exact duplicates ARE decidable, unlike the overlap question above: the same day
	// and the same half-open window is one criterion written twice, and Google refuses
	// the second as an overlapping ad schedule AFTER the campaign exists. Handled the
	// way the rest of this file handles repeats — collapse when the two say the same
	// thing, refuse when they disagree about the bid, which is the device rule and for
	// the device reason: one of the two values would be the one silently dropped.
	seen := make(map[string]int, len(schedules))
	// Accepted intervals per normalised day, as minutes of the day. Only days a caller
	// actually names get an entry, so the common single-day schedule allocates once.
	perDay := make(map[string][]adScheduleSpan, len(schedules))
	for i, s := range schedules {
		day := strings.ToUpper(strings.TrimSpace(s.DayOfWeek))
		if _, ok := adScheduleDays[day]; !ok {
			return nil, nil, fmt.Errorf("google-ads: ad schedule %d day of week %q must be one of MONDAY..SUNDAY", i, s.DayOfWeek)
		}
		if s.StartHour < 0 || s.StartHour > 23 {
			return nil, nil, fmt.Errorf("google-ads: ad schedule %d start hour %d is outside 0..23", i, s.StartHour)
		}
		// 24 is midnight at the END of the day and is valid only as an end.
		if s.EndHour < 1 || s.EndHour > 24 {
			return nil, nil, fmt.Errorf("google-ads: ad schedule %d end hour %d is outside 1..24", i, s.EndHour)
		}
		startMinute, ok := adScheduleMinutes[s.StartMinute]
		if !ok {
			return nil, nil, fmt.Errorf("google-ads: ad schedule %d start minute %d must be 0, 15, 30 or 45 (Google models minutes as an enum)", i, s.StartMinute)
		}
		endMinute, ok := adScheduleMinutes[s.EndMinute]
		if !ok {
			return nil, nil, fmt.Errorf("google-ads: ad schedule %d end minute %d must be 0, 15, 30 or 45 (Google models minutes as an enum)", i, s.EndMinute)
		}
		// An end at or before the start is an empty or inverted window: the campaign
		// would serve for no time at all on that day, which is never what a caller who
		// bothered to write a schedule meant.
		if s.EndHour*60+s.EndMinute <= s.StartHour*60+s.StartMinute {
			return nil, nil, fmt.Errorf("google-ads: ad schedule %d ends at %02d:%02d, which is not after its start %02d:%02d", i, s.EndHour, s.EndMinute, s.StartHour, s.StartMinute)
		}
		// 24:00 is the only representation of end-of-day; 24:15 is past it.
		if s.EndHour == 24 && s.EndMinute != 0 {
			return nil, nil, fmt.Errorf("google-ads: ad schedule %d ends at 24:%02d, past the end of the day (end hour 24 admits only minute 0)", i, s.EndMinute)
		}
		if s.BidModifier != nil {
			if err := validateBidModifier(fmt.Sprintf("ad schedule %d", i), *s.BidModifier); err != nil {
				return nil, nil, err
			}
		}
		info := adScheduleInfo{
			DayOfWeek:   day,
			StartHour:   s.StartHour,
			StartMinute: startMinute,
			EndHour:     s.EndHour,
			EndMinute:   endMinute,
		}
		key := fmt.Sprintf("%s|%d:%s|%d:%s", day, s.StartHour, startMinute, s.EndHour, endMinute)
		if prev, dup := seen[key]; dup {
			if !sameBidModifier(mods[prev], s.BidModifier) {
				return nil, nil, fmt.Errorf("google-ads: ad schedule %d repeats %s %02d:%02d-%02d:%02d with a different bid modifier — Google accepts one criterion per interval, and the second value would be the one silently dropped", i, day, s.StartHour, s.StartMinute, s.EndHour, s.EndMinute)
			}
			continue
		}
		span := adScheduleSpan{start: s.StartHour*60 + s.StartMinute, end: s.EndHour*60 + s.EndMinute}
		for _, other := range perDay[day] {
			if span.start < other.end && other.start < span.end {
				return nil, nil, fmt.Errorf("google-ads: ad schedule %d (%s %02d:%02d-%02d:%02d) overlaps %s %02d:%02d-%02d:%02d — Google rejects overlapping ad schedules for the same day", i, day, s.StartHour, s.StartMinute, s.EndHour, s.EndMinute, day, other.start/60, other.start%60, other.end/60, other.end%60)
			}
		}
		if len(perDay[day]) >= maxAdSchedulesPerDay {
			return nil, nil, fmt.Errorf("google-ads: ad schedule %d is the %dth interval on %s, and Google permits at most %d per day", i, len(perDay[day])+1, day, maxAdSchedulesPerDay)
		}
		perDay[day] = append(perDay[day], span)

		seen[key] = len(out)
		out = append(out, info)
		mods = append(mods, s.BidModifier)
	}
	return out, mods, nil
}

// adScheduleSpan is one accepted interval as minutes of the day, the form both the
// overlap test and the empty-window check compare in. It exists so the per-day list
// carries the two numbers those tests need rather than the four fields a caller
// wrote them as.
type adScheduleSpan struct {
	start int
	end   int
}

// sameBidModifier compares two optional bid modifiers by VALUE, so that a repeated
// interval carrying the same adjustment written as a separate pointer still
// collapses. NaN cannot reach here — validateBidModifier refuses it before the
// duplicate check — so ordinary float equality is total over the values that do.
func sameBidModifier(a, b *float64) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// validateDeviceBidModifiers bounds the device list against the kind's vocabulary,
// refusing a duplicate device: two criteria for the same device are a conflict Google
// rejects AFTER the campaign exists, and the caller plainly meant one of the two values.
func validateDeviceBidModifiers(kind string, devices []DeviceBidModifier) ([]deviceInfo, []float64, error) {
	if len(devices) == 0 {
		return nil, nil, nil
	}
	vocabulary := deviceTypesFor(kind)
	if len(devices) > len(vocabulary) {
		return nil, nil, fmt.Errorf("google-ads: at most %d device bid modifiers are supported on %s, got %d", len(vocabulary), kind, len(devices))
	}
	seen := make(map[string]struct{}, len(devices))
	out := make([]deviceInfo, 0, len(devices))
	mods := make([]float64, 0, len(devices))
	for i, d := range devices {
		device := strings.ToUpper(strings.TrimSpace(d.Device))
		if _, ok := vocabulary[device]; !ok {
			if kind == campaignKindDemandGen {
				return nil, nil, fmt.Errorf("google-ads: device bid modifier %d device %q must be one of MOBILE, DESKTOP, TABLET, CONNECTED_TV on %s", i, d.Device, kind)
			}
			return nil, nil, fmt.Errorf("google-ads: device bid modifier %d device %q must be one of MOBILE, DESKTOP, TABLET on %s (this client sends CONNECTED_TV on Demand Gen only, the one channel where it has verified the device — see deviceTypes)", i, d.Device, kind)
		}
		if _, dup := seen[device]; dup {
			return nil, nil, fmt.Errorf("google-ads: device %s appears more than once in the device bid modifiers — Google accepts one criterion per device, and the second value would be the one silently dropped", device)
		}
		seen[device] = struct{}{}
		if err := validateBidModifier(fmt.Sprintf("device bid modifier %d", i), d.BidModifier); err != nil {
			return nil, nil, err
		}
		out = append(out, deviceInfo{Type: device})
		mods = append(mods, d.BidModifier)
	}
	return out, mods, nil
}

// validateBidModifier mirrors Google's own rule: exactly 0 (the -100% opt-out) or
// within 0.1..10.0. NaN/Inf are rejected first, because every ordered comparison
// against NaN is false and a bare range check would let them through to a paid
// campaign.
func validateBidModifier(noun string, modifier float64) error {
	if math.IsNaN(modifier) || math.IsInf(modifier, 0) {
		return fmt.Errorf("google-ads: %s bid modifier must be a finite number, got %v", noun, modifier)
	}
	if modifier == 0 {
		return nil
	}
	if modifier < minBidModifier || modifier > maxBidModifier {
		return fmt.Errorf("google-ads: %s bid modifier %v must be 0 (do not serve) or between %v and %v", noun, modifier, minBidModifier, maxBidModifier)
	}
	return nil
}

// validateExcludedAgeRanges and validateExcludedGenders resolve the demographic
// EXCLUSION lists.
//
// Exclusion-only is Google's shape, not a narrowing: campaign-level demographic
// criteria are negative. Positive demographic targeting lives on the ad group,
// which this client does not expose, so a field named "ExcludedAgeRanges" cannot
// be mistaken for one that targets.
func validateExcludedAgeRanges(ageRanges []string) ([]ageRangeInfo, error) {
	if len(ageRanges) == 0 {
		return nil, nil
	}
	if len(ageRanges) > maxDemographicExclusions {
		return nil, fmt.Errorf("google-ads: at most %d excluded age ranges are supported, got %d", maxDemographicExclusions, len(ageRanges))
	}
	seen := make(map[string]struct{}, len(ageRanges))
	out := make([]ageRangeInfo, 0, len(ageRanges))
	for _, a := range ageRanges {
		entry := strings.ToUpper(strings.TrimSpace(a))
		enum, ok := ageRangeTypes[entry]
		if !ok {
			return nil, fmt.Errorf("google-ads: excluded age range %q must be one of 18-24, 25-34, 35-44, 45-54, 55-64, 65+ or UNDETERMINED (Google's AGE_RANGE_* enum names are accepted too)", a)
		}
		if _, dup := seen[enum]; dup {
			continue
		}
		seen[enum] = struct{}{}
		out = append(out, ageRangeInfo{Type: enum})
	}
	return out, nil
}

func validateExcludedGenders(genders []string) ([]genderInfo, error) {
	if len(genders) == 0 {
		return nil, nil
	}
	if len(genders) > maxDemographicExclusions {
		return nil, fmt.Errorf("google-ads: at most %d excluded genders are supported, got %d", maxDemographicExclusions, len(genders))
	}
	seen := make(map[string]struct{}, len(genders))
	out := make([]genderInfo, 0, len(genders))
	for _, g := range genders {
		entry := strings.ToUpper(strings.TrimSpace(g))
		if _, ok := genderTypes[entry]; !ok {
			return nil, fmt.Errorf("google-ads: excluded gender %q must be one of MALE, FEMALE, UNDETERMINED", g)
		}
		if _, dup := seen[entry]; dup {
			continue
		}
		seen[entry] = struct{}{}
		out = append(out, genderInfo{Type: entry})
	}
	return out, nil
}

// incomeRangeTypes maps the spellings a caller may write to Google's IncomeRangeTypeEnum
// (google/ads/googleads/v23/enums/income_range_type.proto). The enum names are percentile
// buckets of household income, not currency amounts, so no friendly "50-60" spelling is
// offered: it would read as dollars. Bare UNDETERMINED is accepted beside the enum name,
// as ageRangeTypes accepts it.
var incomeRangeTypes = map[string]string{
	"INCOME_RANGE_0_50":  "INCOME_RANGE_0_50",
	"INCOME_RANGE_50_60": "INCOME_RANGE_50_60",
	"INCOME_RANGE_60_70": "INCOME_RANGE_60_70",
	"INCOME_RANGE_70_80": "INCOME_RANGE_70_80",
	"INCOME_RANGE_80_90": "INCOME_RANGE_80_90",
	"INCOME_RANGE_90_UP": "INCOME_RANGE_90_UP",
	"UNDETERMINED":       "INCOME_RANGE_UNDETERMINED", "INCOME_RANGE_UNDETERMINED": "INCOME_RANGE_UNDETERMINED",
}

// parentalStatusTypes is Google's ParentalStatusTypeEnum
// (google/ads/googleads/v23/enums/parental_status_type.proto), whose names are already
// what a caller would write.
var parentalStatusTypes = map[string]string{
	"PARENT": "PARENT", "NOT_A_PARENT": "NOT_A_PARENT", "UNDETERMINED": "UNDETERMINED",
}

type incomeRangeInfo struct {
	Type string `json:"type"`
}

type parentalStatusInfo struct {
	Type string `json:"type"`
}

// adGroupDemographicPlan is the resolved income-range and parental-status EXCLUSION intent
// (#473). Unlike the age and gender exclusions above, these are AD GROUP criteria: each
// was verified by a validateOnly probe on 2026-11-03 as a negative adGroupCriterion, and
// nothing was probed at the campaign level, so the cascade attaches them to every ad group
// it creates rather than to the campaign.
type adGroupDemographicPlan struct {
	incomeRanges     []incomeRangeInfo
	parentalStatuses []parentalStatusInfo
}

func (p adGroupDemographicPlan) empty() bool { return p.count() == 0 }

func (p adGroupDemographicPlan) count() int {
	return len(p.incomeRanges) + len(p.parentalStatuses)
}

// validateAdGroupDemographicExclusions resolves ExcludedIncomeRanges and
// ExcludedParentalStatuses for the given campaign KIND. Called from preflightCampaignKind,
// so a refusal costs nothing upstream.
//
// The channel fence is exactly what the 2026-11-03 validateOnly probes established, and
// no wider. An income exclusion was accepted on a Search ad group and on a Display ad
// group. A parental-status exclusion was accepted on a Display ad group and REFUSED on a
// Search ad group with OPERATION_NOT_PERMITTED_FOR_CONTEXT, so Search refuses it here,
// before the budget, rather than at the criteria mutate after the campaign exists. Demand
// Gen and Video build ad groups this client did not probe, and Performance Max has none,
// so all three refuse both lists.
func validateAdGroupDemographicExclusions(kind string, in CampaignInput) (adGroupDemographicPlan, error) {
	if len(in.ExcludedIncomeRanges) > 0 && kind != campaignKindSearch && kind != campaignKindDisplay {
		return adGroupDemographicPlan{}, fmt.Errorf("google-ads: income-range exclusions are not supported on %s (this client attaches them as ad-group criteria on the Search and Display cascades only); omit ExcludedIncomeRanges, or create a Search or Display campaign to exclude income ranges", kind)
	}
	if len(in.ExcludedParentalStatuses) > 0 && kind == campaignKindSearch {
		return adGroupDemographicPlan{}, fmt.Errorf("google-ads: parental-status exclusions are not supported on %s (Google refuses a parental-status criterion on a Search ad group with OPERATION_NOT_PERMITTED_FOR_CONTEXT); omit ExcludedParentalStatuses, or create a Display campaign to exclude parental status", kind)
	}
	if len(in.ExcludedParentalStatuses) > 0 && kind != campaignKindDisplay {
		return adGroupDemographicPlan{}, fmt.Errorf("google-ads: parental-status exclusions are not supported on %s (this client attaches them as an ad-group criterion on the Display cascade only); omit ExcludedParentalStatuses, or create a Display campaign to exclude parental status", kind)
	}
	incomes, err := validateEnumExclusions("excluded income range", "excluded income ranges",
		in.ExcludedIncomeRanges, incomeRangeTypes, "INCOME_RANGE_0_50, INCOME_RANGE_50_60, INCOME_RANGE_60_70, INCOME_RANGE_70_80, INCOME_RANGE_80_90, INCOME_RANGE_90_UP or UNDETERMINED")
	if err != nil {
		return adGroupDemographicPlan{}, err
	}
	parents, err := validateEnumExclusions("excluded parental status", "excluded parental statuses",
		in.ExcludedParentalStatuses, parentalStatusTypes, "PARENT, NOT_A_PARENT or UNDETERMINED")
	if err != nil {
		return adGroupDemographicPlan{}, err
	}
	var plan adGroupDemographicPlan
	for _, t := range incomes {
		plan.incomeRanges = append(plan.incomeRanges, incomeRangeInfo{Type: t})
	}
	for _, t := range parents {
		plan.parentalStatuses = append(plan.parentalStatuses, parentalStatusInfo{Type: t})
	}
	return plan, nil
}

// validateEnumExclusions is validateExcludedAgeRanges' shape for an enum list: trim and
// upper-case each entry, map it through allowed, collapse repeats by the resolved enum, and
// cap the list at maxDemographicExclusions. noun names one entry, plural the list.
//
// An entry carrying any non-ASCII rune is refused BEFORE strings.ToUpper, because Unicode
// case folding maps some non-ASCII letters onto ASCII ones (the dotless ı upper-cases to
// I, the long ſ to S), so "ıncome_range_0_50" would otherwise resolve to a real enum.
func validateEnumExclusions(noun, plural string, values []string, allowed map[string]string, choices string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	if len(values) > maxDemographicExclusions {
		return nil, fmt.Errorf("google-ads: at most %d %s are supported, got %d", maxDemographicExclusions, plural, len(values))
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		var enum string
		ok := !hasNonASCII(v)
		if ok {
			enum, ok = allowed[strings.ToUpper(strings.TrimSpace(v))]
		}
		if !ok {
			return nil, fmt.Errorf("google-ads: %s %q must be one of %s", noun, v, choices)
		}
		if _, dup := seen[enum]; dup {
			continue
		}
		seen[enum] = struct{}{}
		out = append(out, enum)
	}
	return out, nil
}

// hasNonASCII reports whether s carries any rune above unicode.MaxASCII.
func hasNonASCII(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII {
			return true
		}
	}
	return false
}

// adGroupDemographicStep names the requested parts of the plan for the step list, as
// criteriaStep does for the campaign-level set.
func adGroupDemographicStep(plan adGroupDemographicPlan) string {
	parts := make([]string, 0, 2)
	if n := len(plan.incomeRanges); n > 0 {
		parts = append(parts, fmt.Sprintf("%d income range exclusion(s)", n))
	}
	if n := len(plan.parentalStatuses); n > 0 {
		parts = append(parts, fmt.Sprintf("%d parental status exclusion(s)", n))
	}
	return strings.Join(parts, "; ")
}

// criteriaStep summarises the plan for the step list a human reads, naming only
// the parts that were actually requested — a clause about devices on a campaign
// with no device criteria is a lie about a paid resource.
func criteriaStep(plan criteriaPlan) string {
	parts := make([]string, 0, 5)
	if n := len(plan.languages); n > 0 {
		parts = append(parts, fmt.Sprintf("%d language(s)", n))
	}
	if n := len(plan.schedules); n > 0 {
		parts = append(parts, fmt.Sprintf("%d ad schedule(s)", n))
	}
	if n := len(plan.devices); n > 0 {
		parts = append(parts, fmt.Sprintf("%d device bid modifier(s)", n))
	}
	if n := len(plan.ageRanges); n > 0 {
		parts = append(parts, fmt.Sprintf("%d age range exclusion(s)", n))
	}
	if n := len(plan.genders); n > 0 {
		parts = append(parts, fmt.Sprintf("%d gender exclusion(s)", n))
	}
	return strings.Join(parts, "; ")
}

// criteriaFailureKinds names the criterion kinds plan carries ("schedule or device") and
// what their absence leaves the campaign doing ("around the clock, on every device"), for
// createCampaignTargetingCriteria's failure text. Built from the plan rather than fixed
// because the plan differs per channel: Demand Gen's carries no languages (#474), and a
// message claiming the campaign "would serve in every language" there would be false.
func criteriaFailureKinds(plan criteriaPlan) (kinds, exposure string) {
	var k, e []string
	if len(plan.languages) > 0 {
		k, e = append(k, "language"), append(e, "in every language")
	}
	if len(plan.schedules) > 0 {
		k, e = append(k, "schedule"), append(e, "around the clock")
	}
	if len(plan.devices) > 0 {
		k, e = append(k, "device"), append(e, "on every device")
	}
	if len(plan.ageRanges)+len(plan.genders) > 0 {
		k, e = append(k, "demographic"), append(e, "to the demographics it was told to exclude")
	}
	if n := len(k); n > 1 {
		return strings.Join(k[:n-1], ", ") + " or " + k[n-1], strings.Join(e, ", ")
	}
	return strings.Join(k, ""), strings.Join(e, "")
}

// createCampaignTargetingCriteria attaches the campaign-level criteria in plan —
// language, ad schedule, device and demographic — to a just-created campaign as a single
// campaignCriteria:mutate. Search, Display, Video and Performance Max send what their
// preflight admitted; Demand Gen sends only its ad schedules and devices (#474), its
// languages going on the ad group. The failure text names only the kinds in plan
// (criteriaFailureKinds), so it never claims a criterion this call was not sending.
//
// ONE BATCH, and it must be. The four kinds are not independent settings that
// happen to arrive together: a campaign whose languages committed and whose
// device exclusions did not is serving on exactly the devices the caller paid to
// stay off, and a campaign whose schedule committed and whose demographic
// exclusions did not is running all week in front of exactly the audience it was
// told to avoid. Either all of the non-location targeting applies or none of it
// does.
//
// It is deliberately NOT folded into the geo mutate, for the reason the negative
// keywords are not: a shared mutate makes either set's failure discard the other,
// and geo's failure sentence ("it has NO location criteria and would serve
// worldwide if enabled") would be false for a dropped language criterion.
//
// Called AFTER the campaign create, so a failure is reported as a partial result:
// the campaign exists and is PAUSED regardless, and must never be discarded by a
// (nil, err).
func (c *Client) createCampaignTargetingCriteria(ctx context.Context, campaignResource, campaignID string, plan criteriaPlan) ([]string, error) {
	if plan.empty() {
		return nil, nil
	}
	kinds, exposure := criteriaFailureKinds(plan)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("google-ads campaign targeting criteria aborted before any request (context already done; campaign %s has no %s criteria yet): %w", campaignID, kinds, ctxErr)
	}

	ops := make([]mutateOperation, 0, plan.count())
	for i := range plan.languages {
		ops = append(ops, mutateOperation{Create: campaignCriterionCreate{
			Campaign: campaignResource,
			Language: &plan.languages[i],
		}})
	}
	for i := range plan.schedules {
		ops = append(ops, mutateOperation{Create: campaignCriterionCreate{
			Campaign:    campaignResource,
			AdSchedule:  &plan.schedules[i],
			BidModifier: plan.scheduleMod[i],
		}})
	}
	for i := range plan.devices {
		// Taken by address so the zero opt-out is sent as an explicit 0 rather than
		// omitted — see campaignCriterionCreate.BidModifier.
		modifier := plan.deviceMod[i]
		ops = append(ops, mutateOperation{Create: campaignCriterionCreate{
			Campaign:    campaignResource,
			Device:      &plan.devices[i],
			BidModifier: &modifier,
		}})
	}
	// Demographics are campaign-level EXCLUSIONS — negative is the whole point of
	// the criterion, and an omitted `negative` would TARGET the age range the caller
	// asked to keep out.
	for i := range plan.ageRanges {
		ops = append(ops, mutateOperation{Create: campaignCriterionCreate{
			Campaign: campaignResource,
			Negative: true,
			AgeRange: &plan.ageRanges[i],
		}})
	}
	for i := range plan.genders {
		ops = append(ops, mutateOperation{Create: campaignCriterionCreate{
			Campaign: campaignResource,
			Negative: true,
			Gender:   &plan.genders[i],
		}})
	}

	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("campaignCriteria:mutate"), mutateRequest{Operations: ops}, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return nil, fmt.Errorf("google-ads campaign targeting criteria UNCONFIRMED (campaign %s; %s criteria may exist — verify in Google Ads before retrying): %w", campaignID, kinds, err)
		}
		return nil, fmt.Errorf("google-ads campaign targeting criteria failed (campaign %s created; it has NO %s criteria and would serve %s if enabled): %w", campaignID, kinds, exposure, err)
	}

	var mr mutateResponse
	if uErr := json.Unmarshal(resp, &mr); uErr != nil || len(mr.Results) != len(ops) {
		return nil, fmt.Errorf("google-ads campaign targeting criteria UNCONFIRMED (campaign %s; 2xx with a malformed/short mutate response — criteria may exist — verify in Google Ads before retrying)", campaignID)
	}

	ids := make([]string, 0, len(ops))
	seen := newResultIDSet(len(mr.Results))
	for i, r := range mr.Results {
		returnedCampaignID, critID := c.campaignCriterionID(r.ResourceName)
		if critID == "" || returnedCampaignID == "" {
			return nil, fmt.Errorf("google-ads campaign targeting criteria UNCONFIRMED (campaign %s; malformed/wrong-kind/wrong-account criterion resource name %q at index %d — verify in Google Ads before retrying)", campaignID, r.ResourceName, i)
		}
		if returnedCampaignID != campaignID {
			return nil, fmt.Errorf("google-ads campaign targeting criteria UNCONFIRMED (campaign %s; campaignCriterion resource name %q reports a different campaign id %q — verify in Google Ads before retrying)", campaignID, r.ResourceName, returnedCampaignID)
		}
		if dup := seen.repeat(critID, i); dup != "" {
			return nil, fmt.Errorf("google-ads campaign targeting criteria UNCONFIRMED "+
				"(campaign %s; %s — verify in Google Ads before retrying)", campaignID, dup)
		}
		ids = append(ids, critID)
	}
	return ids, nil
}
