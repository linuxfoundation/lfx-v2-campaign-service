// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"fmt"
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
)

// metaGenderCodes maps the request's gender names to Meta's targeting codes.
var metaGenderCodes = map[string]int{"male": 1, "female": 2}

// IsZero reports whether t asks for nothing, in which case the ad set's targeting is unchanged.
func (t *AudienceTargeting) IsZero() bool {
	return t == nil || (t.AgeMin == nil && t.AgeMax == nil && len(t.Genders) == 0 &&
		len(t.CustomAudiences) == 0 && len(t.ExcludedCustomAudiences) == 0 && t.AdvantageAudience == nil)
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
	var out []string
	seen := map[string]bool{}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if !numericIDRE.MatchString(id) {
			return nil, targetingInvalid("%s entry %s is not a Meta custom audience id (a numeric string)", field, domain.CallerValue(raw))
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) > MaxCustomAudiences {
		return nil, targetingInvalid("%s names %d audiences; at most %d are allowed", field, len(out), MaxCustomAudiences)
	}
	return out, nil
}

// inclusion reports whether t narrows WHO is reached — a non-default age, a single gender or an
// included custom audience. These are the fields Meta's v23.0 rule names: setting one without an
// explicit targeting_automation.advantage_audience is an error on ad-set create. Exclusions are
// not inclusions: they are a hard constraint either way.
func (t *AudienceTargeting) inclusion() bool {
	return (t.AgeMin != nil && *t.AgeMin != MinTargetAge) ||
		(t.AgeMax != nil && *t.AgeMax != MaxTargetAge) ||
		len(t.Genders) == 1 ||
		len(t.CustomAudiences) > 0
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
