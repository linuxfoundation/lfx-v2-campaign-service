// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// AudienceTargeting is the optional "who sees it" part of a Meta create (metaConfig.targeting,
// issue #550): an age range, a gender, custom audiences to include and to exclude, and the
// Advantage+ audience flag. Each maps to a field of the ad set's `targeting` spec (Marketing API,
// Ad Set reference and Targeting Specs): `age_min`, `age_max`, `genders` (1 = male, 2 = female),
// `custom_audiences` and `excluded_custom_audiences` (lists of `{"id": …}`), and
// `targeting_automation.advantage_audience`. Lookalike audiences are custom audiences (subtype
// LOOKALIKE), so they are named in the same lists.
//
// The zero value targets exactly as before: nothing is added to the ad set's targeting.
type AudienceTargeting struct {
	// AgeMin and AgeMax are whole years, MinTargetAge..MaxTargetAge, AgeMin <= AgeMax. Nil (absent
	// or null) means Meta's default, 18 and 65; a supplied value, 0 included, must be in range —
	// pointers keep an explicit 0 from reading as "omitted".
	AgeMin *int `json:"ageMin"`
	AgeMax *int `json:"ageMax"`
	// Genders is a subset of "male" and "female". Both, or none, is everyone and sends nothing.
	Genders []string `json:"genders"`
	// CustomAudiences and ExcludedCustomAudiences are numeric Meta custom-audience ids of the
	// connected ad account, at most MaxCustomAudiences each, never the same id in both.
	CustomAudiences         []string `json:"customAudiences"`
	ExcludedCustomAudiences []string `json:"excludedCustomAudiences"`
	// AdvantageAudience sets targeting_automation.advantage_audience. Nil means "not said": it is
	// then sent only when an inclusion is set (see inclusion), as 0.
	AdvantageAudience *bool `json:"advantageAudience"`
	// Locales are Meta locale keys (positive integers such as 6, English (US), or 1001, English
	// (All); the Targeting Search `adlocale` type lists them), at most MaxLocales distinct (#550,
	// gap 12). Sent as `locales`. A language is a hard constraint even with Advantage+ audience on
	// (Advantage+ Audience reference), so it is not an inclusion. Gated by its own deployment flag,
	// not by the one that gates the six audience fields above.
	Locales []int `json:"locales"`
	// DevicePlatforms is a subset of "mobile" and "desktop" (#550, gap 15), sent as the ad set's
	// `device_platforms`. Both, or none, is Meta's default and sends nothing. A device is a placement
	// control, not an audience inclusion, so it sends no Advantage+ flag. Gated by its own deployment
	// flag.
	DevicePlatforms []string `json:"devicePlatforms"`
	// Interests are numeric Meta interest ids (detailed targeting; the Targeting Search
	// `adinterest` type lists them), at most MaxInterests distinct (#550, gap 7). Sent as one
	// `flexible_spec` entry, `[{"interests": [{"id": …}, …]}]`, so a person matching ANY listed
	// interest qualifies. Detailed targeting IS an inclusion under Meta's v23.0 rule, so it makes the
	// Advantage+ flag explicit (see inclusion). Gated by its own deployment flag. Each id is checked
	// with Meta's interest validation read before anything is created (checkInterests).
	Interests []string `json:"interests"`
}

const (
	// MinTargetAge and MaxTargetAge bound age_min and age_max. Meta's default range is 18-65, and
	// 65 means "65+"; ads here are for adults, so nothing under 18 is accepted.
	MinTargetAge = 18
	MaxTargetAge = 65
	// maxAdvantageAgeMin is the largest age_min Meta accepts while Advantage+ audience is on, where
	// age_max is fixed at 65 (Advantage+ Audience reference: "age_min: Range: 18-25 only",
	// "age_max: Fixed at 65").
	maxAdvantageAgeMin = 25
	// MaxCustomAudiences caps each audience list. Meta documents no lower limit than this would
	// hit; an LF campaign names a handful, and each id costs one ownership read before the create.
	MaxCustomAudiences = 25
	// MaxLocales caps the locales list. Meta documents no limit; an event targets a handful of
	// languages, and a long list is more likely a client bug than a campaign.
	MaxLocales = 50
	// MaxInterests caps the interests list, for the same reason as MaxCustomAudiences: an event
	// names a handful, and the validation read sends every id in one request URL.
	MaxInterests = 25
)

// metaGenderCodes maps the request's gender names to Meta's targeting codes.
var metaGenderCodes = map[string]int{"male": 1, "female": 2}

// metaDevicePlatforms are the Ad Set targeting `device_platforms` values (Targeting Specs,
// placement fields).
var metaDevicePlatforms = map[string]bool{"mobile": true, "desktop": true}

// IsZero reports whether t asks for nothing, in which case the ad set's targeting is unchanged.
func (t *AudienceTargeting) IsZero() bool {
	return t == nil || (t.AgeMin == nil && t.AgeMax == nil && len(t.Genders) == 0 &&
		len(t.CustomAudiences) == 0 && len(t.ExcludedCustomAudiences) == 0 && t.AdvantageAudience == nil &&
		len(t.Locales) == 0 && len(t.DevicePlatforms) == 0 && len(t.Interests) == 0)
}

// HasAudienceFields reports whether t sets any of the six fields META_AUDIENCE_TARGETING_ENABLED
// gates — age, gender, the two custom audience lists, advantageAudience — as opposed to only the
// later fields (locales, devicePlatforms, interests) that have their own deployment flags.
func (t *AudienceTargeting) HasAudienceFields() bool {
	return t != nil && (t.AgeMin != nil || t.AgeMax != nil || len(t.Genders) > 0 ||
		len(t.CustomAudiences) > 0 || len(t.ExcludedCustomAudiences) > 0 || t.AdvantageAudience != nil)
}

// NormalizeAudienceTargeting validates t and returns the copy the create sends: gender names
// lower-cased and de-duplicated, audience ids trimmed and de-duplicated in order. Every refusal is
// a domain.PreflightRefusal raised before any request, and quotes a caller value only through
// domain.CallerValue. A nil t is returned as nil.
func NormalizeAudienceTargeting(t *AudienceTargeting) (*AudienceTargeting, error) {
	if t == nil {
		return nil, nil
	}
	out := &AudienceTargeting{AgeMin: copyInt(t.AgeMin), AgeMax: copyInt(t.AgeMax), AdvantageAudience: t.AdvantageAudience}
	for _, a := range []struct {
		name string
		v    *int
	}{{"ageMin", t.AgeMin}, {"ageMax", t.AgeMax}} {
		if a.v != nil && (*a.v < MinTargetAge || *a.v > MaxTargetAge) {
			return nil, targetingInvalid("%s %d is out of range; Meta targets ages %d to %d", a.name, *a.v, MinTargetAge, MaxTargetAge)
		}
	}
	if t.AgeMin != nil && t.AgeMax != nil && *t.AgeMin > *t.AgeMax {
		return nil, targetingInvalid("ageMin %d is above ageMax %d", *t.AgeMin, *t.AgeMax)
	}
	seenGender := map[string]bool{}
	for _, g := range t.Genders {
		key := strings.ToLower(strings.TrimSpace(g))
		if _, ok := metaGenderCodes[key]; !ok {
			return nil, targetingInvalid("gender %s is not one of male, female", domain.CallerValue(g))
		}
		if !seenGender[key] {
			seenGender[key] = true
			out.Genders = append(out.Genders, key)
		}
	}
	var err error
	if out.CustomAudiences, err = normalizeAudienceIDs("customAudiences", t.CustomAudiences); err != nil {
		return nil, err
	}
	if out.ExcludedCustomAudiences, err = normalizeAudienceIDs("excludedCustomAudiences", t.ExcludedCustomAudiences); err != nil {
		return nil, err
	}
	included := make(map[string]bool, len(out.CustomAudiences))
	for _, id := range out.CustomAudiences {
		included[id] = true
	}
	for _, id := range out.ExcludedCustomAudiences {
		if included[id] {
			return nil, targetingInvalid("custom audience %s is in both customAudiences and excludedCustomAudiences", domain.CallerValue(id))
		}
	}
	if out.Locales, err = normalizeLocales(t.Locales); err != nil {
		return nil, err
	}
	if out.Interests, err = normalizeNumericIDs("interests", "Meta interest id", t.Interests, MaxInterests); err != nil {
		return nil, err
	}
	seenDevice := map[string]bool{}
	for _, d := range t.DevicePlatforms {
		key := strings.ToLower(strings.TrimSpace(d))
		if !metaDevicePlatforms[key] {
			return nil, targetingInvalid("devicePlatforms entry %s is not one of mobile, desktop", domain.CallerValue(d))
		}
		if !seenDevice[key] {
			seenDevice[key] = true
			out.DevicePlatforms = append(out.DevicePlatforms, key)
		}
	}
	if out.AdvantageAudience != nil && *out.AdvantageAudience {
		if out.AgeMin != nil && *out.AgeMin > maxAdvantageAgeMin {
			return nil, targetingInvalid("with advantageAudience true, Meta accepts an ageMin of at most %d (ageMin is %d); lower it or set advantageAudience false", maxAdvantageAgeMin, *out.AgeMin)
		}
		if out.AgeMax != nil && *out.AgeMax < MaxTargetAge {
			return nil, targetingInvalid("with advantageAudience true, Meta fixes ageMax at %d (ageMax is %d); omit it or set advantageAudience false", MaxTargetAge, *out.AgeMax)
		}
	}
	return out, nil
}

// targetingInvalid builds a refusal of the targeting block. Like publishIdentityInvalid it wraps
// domain.ErrCreateConfigInvalid, so the synchronous create path answers 400, and is a
// domain.PreflightRefusal, so the job reports it verbatim. Callers quote a caller value only
// through domain.CallerValue.
func targetingInvalid(format string, args ...any) error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: metaConfig.targeting: %s", domain.ErrCreateConfigInvalid, fmt.Sprintf(format, args...)))
}

// normalizeAudienceIDs trims and de-duplicates one audience list, refusing a non-numeric id or a
// list over MaxCustomAudiences distinct ids.
func normalizeAudienceIDs(field string, ids []string) ([]string, error) {
	return normalizeNumericIDs(field, "Meta custom audience id", ids, MaxCustomAudiences)
}

// normalizeNumericIDs trims and de-duplicates one id list, refusing an id that is not a numeric
// string (what names the kind of id) or a list over limit distinct ids.
func normalizeNumericIDs(field, what string, ids []string, limit int) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if !numericIDRE.MatchString(id) {
			return nil, targetingInvalid("%s entry %s is not a %s (a numeric string)", field, domain.CallerValue(raw), what)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) > limit {
		return nil, targetingInvalid("%s names %d ids; at most %d are allowed", field, len(out), limit)
	}
	return out, nil
}

// CheckDevicePlacements refuses a desktop-only device list together with the Audience Network
// placement, which serves in mobile apps only (Meta's placement guide), so Meta would refuse the ad
// set after the campaign exists. t must be NormalizeAudienceTargeting's output (lower-cased,
// de-duplicated); placements are the caller's, merged with the defaults here. Other placement and
// device pairings are left to Meta until a live probe says which it refuses.
func CheckDevicePlacements(t *AudienceTargeting, placements Placement) error {
	if t == nil || len(t.DevicePlatforms) != 1 || t.DevicePlatforms[0] != "desktop" {
		return nil
	}
	if deref(mergePlacements(placements).AudienceNetwork) {
		return targetingInvalid("devicePlatforms [desktop] cannot serve the audienceNetwork placement, which is mobile only; add mobile or turn audienceNetwork off")
	}
	return nil
}

// normalizeLocales de-duplicates the locale keys in order, refusing a key that is not a positive
// integer or a list over MaxLocales distinct keys.
func normalizeLocales(keys []int) ([]int, error) {
	var out []int
	seen := map[int]bool{}
	for _, k := range keys {
		if k <= 0 {
			return nil, targetingInvalid("locales entry %d is not a Meta locale key (a positive integer)", k)
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	if len(out) > MaxLocales {
		return nil, targetingInvalid("locales names %d languages; at most %d are allowed", len(out), MaxLocales)
	}
	return out, nil
}

// inclusion reports whether t narrows WHO is reached — a non-default age, a single gender, an
// included custom audience or a detailed-targeting interest. These are the fields Meta's v23.0 rule names: setting one without an
// explicit targeting_automation.advantage_audience is an error on ad-set create. Exclusions are
// not inclusions: they are a hard constraint either way.
func (t *AudienceTargeting) inclusion() bool {
	return (t.AgeMin != nil && *t.AgeMin != MinTargetAge) ||
		(t.AgeMax != nil && *t.AgeMax != MaxTargetAge) ||
		len(t.Genders) == 1 ||
		len(t.CustomAudiences) > 0 ||
		len(t.Interests) > 0
}

// apply adds t to an ad set's targeting spec. t must be NormalizeAudienceTargeting's output. The
// Advantage+ flag is sent whenever an inclusion is set — false unless the caller said true, so the
// targeting the operator chose is a hard constraint rather than a suggestion Meta may widen — and
// otherwise only when the caller set it, so a create without inclusions is unchanged.
func (t *AudienceTargeting) apply(targeting map[string]any) {
	if t.IsZero() {
		return
	}
	if t.AgeMin != nil {
		targeting["age_min"] = *t.AgeMin
	}
	if t.AgeMax != nil {
		targeting["age_max"] = *t.AgeMax
	}
	if len(t.Genders) == 1 {
		targeting["genders"] = []int{metaGenderCodes[t.Genders[0]]}
	}
	if len(t.CustomAudiences) > 0 {
		targeting["custom_audiences"] = audienceRefs(t.CustomAudiences)
	}
	if len(t.ExcludedCustomAudiences) > 0 {
		targeting["excluded_custom_audiences"] = audienceRefs(t.ExcludedCustomAudiences)
	}
	if len(t.Locales) > 0 {
		targeting["locales"] = t.Locales
	}
	if len(t.Interests) > 0 {
		targeting["flexible_spec"] = []map[string]any{{"interests": audienceRefs(t.Interests)}}
	}
	if len(t.DevicePlatforms) == 1 {
		targeting["device_platforms"] = t.DevicePlatforms
	}
	if t.inclusion() || t.AdvantageAudience != nil {
		flag := 0
		if t.AdvantageAudience != nil && *t.AdvantageAudience {
			flag = 1
		}
		targeting["targeting_automation"] = map[string]any{"advantage_audience": flag}
	}
}

func copyInt(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func audienceRefs(ids []string) []map[string]string {
	out := make([]map[string]string, len(ids))
	for i, id := range ids {
		out[i] = map[string]string{"id": id}
	}
	return out
}

// customAudienceOwner is the ownership read's answer: GET /{audience_id}?fields=id,account_id,subtype.
// subtype (CUSTOM, WEBSITE, LOOKALIKE, ENGAGEMENT, …) is a Custom Audience field and is what proves
// the node IS an audience: a campaign, ad set or ad of the same account also has an id and an
// account_id, and Graph refuses a field the node type does not have, so such an id fails the read.
// account_id is the ad account id, documented WITHOUT the act_ prefix (Custom Audience reference);
// it is compared through sameMetaAccount, which ignores the prefix on either side.
type customAudienceOwner struct {
	ID        string `json:"id"`
	AccountID string `json:"account_id"`
	Subtype   string `json:"subtype"`
}

// customAudienceFields is the ownership read's field list (see customAudienceOwner).
const customAudienceFields = "id,account_id,subtype"

// checkCustomAudiences proves, before the first mutating call, that every audience t names is a
// custom audience of accountID (act_<digits>). A foreign or mistyped id would otherwise be refused
// by Meta at the AD SET create, after the campaign already exists. A read that fails — Graph 100/33
// included, which cannot tell "absent" from "not visible to this token" — is an error and nothing
// is created; an audience reported under another account is a refusal.
func (c *Client) checkCustomAudiences(ctx context.Context, accountID string, t *AudienceTargeting) error {
	if t == nil {
		return nil
	}
	for _, list := range []struct {
		field string
		ids   []string
	}{{"customAudiences", t.CustomAudiences}, {"excludedCustomAudiences", t.ExcludedCustomAudiences}} {
		for _, id := range list.ids {
			raw, err := c.getChecked(ctx, "/"+id+"?fields="+customAudienceFields)
			if err != nil {
				return fmt.Errorf("meta targeting: %s audience %s could not be read, so nothing was created: %w", list.field, domain.CallerValue(id), err)
			}
			var owner customAudienceOwner
			if err := json.Unmarshal(raw, &owner); err != nil {
				return fmt.Errorf("meta targeting: %s audience %s read was not a custom audience, so nothing was created: %w", list.field, domain.CallerValue(id), err)
			}
			if !statusTokenRE.MatchString(owner.Subtype) {
				return domain.PreflightRefusalf("meta targeting: %s id %s is not a custom audience (the read reported no audience subtype); nothing was created", list.field, domain.CallerValue(id))
			}
			if owner.ID != id || !sameMetaAccount(owner.AccountID, accountID) {
				return domain.PreflightRefusalf("meta targeting: %s audience %s is not a custom audience of the connected ad account %s; nothing was created", list.field, domain.CallerValue(id), accountID)
			}
		}
	}
	return nil
}

// interestValidation is one row of Meta's interest validation read: GET
// /search?type=adinterestvalid&interest_fbid_list=[…] (Targeting Search reference, "Validation").
// Meta answers a row per requested id, with valid false for an id it no longer offers (detailed
// targeting options are retired regularly).
type interestValidation struct {
	ID    string `json:"id"`
	Valid *bool  `json:"valid"`
}

// checkInterests proves, before the first mutating call, that every interest t names is one Meta
// currently offers. A retired or mistyped id would otherwise be refused at the AD SET create, after
// the campaign already exists. A read that fails, or an answer without a row for every id, is an
// error and nothing is created; a row with valid false (or no valid field) is a refusal.
func (c *Client) checkInterests(ctx context.Context, t *AudienceTargeting) error {
	if t == nil || len(t.Interests) == 0 {
		return nil
	}
	list, err := json.Marshal(t.Interests)
	if err != nil {
		return fmt.Errorf("meta targeting: encode the interest list: %w", err)
	}
	path := "/search?type=adinterestvalid&interest_fbid_list=" + url.QueryEscape(string(list))
	raw, err := c.getChecked(ctx, path)
	if err != nil {
		return fmt.Errorf("meta targeting: the interests could not be validated, so nothing was created: %w", err)
	}
	var page struct {
		Data *[]interestValidation `json:"data"`
	}
	if err := json.Unmarshal(raw, &page); err != nil || page.Data == nil {
		return fmt.Errorf("meta targeting: the interest validation read returned no rows, so nothing was created")
	}
	valid := make(map[string]bool, len(*page.Data))
	answered := make(map[string]bool, len(*page.Data))
	for _, row := range *page.Data {
		answered[row.ID] = true
		valid[row.ID] = row.Valid != nil && *row.Valid
	}
	for _, id := range t.Interests {
		if !answered[id] {
			return fmt.Errorf("meta targeting: the interest validation read did not answer for interest %s, so nothing was created", domain.CallerValue(id))
		}
		if !valid[id] {
			return domain.PreflightRefusalf("meta targeting: interest %s is not a detailed targeting interest Meta currently offers; nothing was created", domain.CallerValue(id))
		}
	}
	return nil
}
