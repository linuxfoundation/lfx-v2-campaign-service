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
// SEARCH ONLY. Every one of these is refused on the Demand Gen path at preflight.
// Demand Gen attaches its targeting at the AD GROUP level (geo.go has the same
// split) and this client has verified none of these criteria there. Refusing
// locally is free; discovering it after a paid campaign exists is not, and
// silently dropping the caller's intent is the defect LFXV2-3283 already fixed
// once for countries. Lift this per criterion as each is confirmed live.
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

// maxDeviceBidModifiers bounds the device list at one entry per device type this
// client accepts. A longer list is necessarily a duplicate, which Google rejects
// with a criterion conflict AFTER the campaign exists.
const maxDeviceBidModifiers = 4

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

// deviceTypes is the Device enum this client accepts. OTHER is deliberately
// absent: it is a reporting bucket rather than something a campaign bids on.
var deviceTypes = map[string]struct{}{
	"MOBILE": {}, "DESKTOP": {}, "TABLET": {}, "CONNECTED_TV": {},
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
	asked := len(in.Languages) + len(in.AdSchedules) + len(in.DeviceBidModifiers) +
		len(in.ExcludedAgeRanges) + len(in.ExcludedGenders)
	if asked > 0 && kind == campaignKindDemandGen {
		return criteriaPlan{}, fmt.Errorf("google-ads: language, ad schedule, device and demographic criteria are not supported on %s (Demand Gen attaches targeting at the ad group level, where this client has not verified these criteria); create a Search campaign for them", kind)
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

	devices, deviceMods, err := validateDeviceBidModifiers(in.DeviceBidModifiers)
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
// There is deliberately NO overlap check between intervals. Google rejects a true
// overlap itself, and an overlap test written here would have to decide whether
// 09:00-12:00 and 12:00-17:00 touch — they do not, the window is half-open — and
// a wrong answer refuses a perfectly ordinary split-day schedule. Under-refusal
// costs an upstream error message; over-refusal costs a campaign that cannot be
// created at all.
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
		seen[key] = len(out)
		out = append(out, info)
		mods = append(mods, s.BidModifier)
	}
	return out, mods, nil
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

// validateDeviceBidModifiers bounds the device list, refusing a duplicate device:
// two criteria for the same device are a conflict Google rejects AFTER the
// campaign exists, and the caller plainly meant one of the two values.
func validateDeviceBidModifiers(devices []DeviceBidModifier) ([]deviceInfo, []float64, error) {
	if len(devices) == 0 {
		return nil, nil, nil
	}
	if len(devices) > maxDeviceBidModifiers {
		return nil, nil, fmt.Errorf("google-ads: at most %d device bid modifiers are supported, got %d", maxDeviceBidModifiers, len(devices))
	}
	seen := make(map[string]struct{}, len(devices))
	out := make([]deviceInfo, 0, len(devices))
	mods := make([]float64, 0, len(devices))
	for i, d := range devices {
		device := strings.ToUpper(strings.TrimSpace(d.Device))
		if _, ok := deviceTypes[device]; !ok {
			return nil, nil, fmt.Errorf("google-ads: device bid modifier %d device %q must be one of MOBILE, DESKTOP, TABLET, CONNECTED_TV", i, d.Device)
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

// createCampaignTargetingCriteria attaches the language, ad schedule, device and
// demographic criteria to a just-created SEARCH campaign as a single
// campaignCriteria:mutate.
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
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("google-ads campaign targeting criteria aborted before any request (context already done; campaign %s has no language/schedule/device/demographic criteria yet): %w", campaignID, ctxErr)
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
			return nil, fmt.Errorf("google-ads campaign targeting criteria UNCONFIRMED (campaign %s; language/schedule/device/demographic criteria may exist — verify in Google Ads before retrying): %w", campaignID, err)
		}
		return nil, fmt.Errorf("google-ads campaign targeting criteria failed (campaign %s created; it has NO language, schedule, device or demographic criteria and would serve in every language, around the clock, on every device if enabled): %w", campaignID, err)
	}

	var mr mutateResponse
	if uErr := json.Unmarshal(resp, &mr); uErr != nil || len(mr.Results) != len(ops) {
		return nil, fmt.Errorf("google-ads campaign targeting criteria UNCONFIRMED (campaign %s; 2xx with a malformed/short mutate response — criteria may exist — verify in Google Ads before retrying)", campaignID)
	}

	ids := make([]string, 0, len(ops))
	for i, r := range mr.Results {
		returnedCampaignID, critID := c.campaignCriterionID(r.ResourceName)
		if critID == "" || returnedCampaignID == "" {
			return nil, fmt.Errorf("google-ads campaign targeting criteria UNCONFIRMED (campaign %s; malformed/wrong-kind/wrong-account criterion resource name %q at index %d — verify in Google Ads before retrying)", campaignID, r.ResourceName, i)
		}
		if returnedCampaignID != campaignID {
			return nil, fmt.Errorf("google-ads campaign targeting criteria UNCONFIRMED (campaign %s; campaignCriterion resource name %q reports a different campaign id %q — verify in Google Ads before retrying)", campaignID, r.ResourceName, returnedCampaignID)
		}
		ids = append(ids, critID)
	}
	return ids, nil
}
