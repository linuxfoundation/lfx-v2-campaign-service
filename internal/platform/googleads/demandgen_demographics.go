// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Demand Gen demographics inside the Audience (#498).
//
// A Demand Gen ad group is audience-grouped, so Google refuses age, gender, income and
// parental-status criteria on it (demandgen_targeting.go). The demographics go INSIDE a
// Google Ads Audience instead, as AudienceDimension entries, and the ad group references
// that Audience with its one `audience` criterion. The live KubeCon NA 2026 Demand Gen
// campaign is built that way: its Audience "CNCF - KubeCon NA - High Intent" holds
// {age: {ageRanges: [{minAge: 25, maxAge: 54}], includeUndetermined: true}} beside its
// segments (read-only GAQL, 2026-11-05).
//
// CampaignInput.DemandGenDemographics makes that Audience. Before the budget mutate the
// cascade creates ONE Audience (audiences:mutate) holding the demographics, and optionally
// existing user lists, then attaches it to every Demand Gen ad group it creates, exactly as
// DemandGenAudience is attached. Since #518 a DemandGenAdGroups entry may set Demographics of
// its own: that group gets an Audience of its own, built by the same buildDemographicPlan and
// created the same way, which overrides the campaign-level one for that group (the live
// KubeCon NA campaign has a different Audience on each group).
//
// Every shape here was accepted by a validateOnly probe on 2026-11-05 (v23, account
// 8666746580): all four demographic dimensions together, the same plus an audienceSegments
// dimension holding a user list, an age segment with no maxAge (65+), two non-adjacent age
// segments, and a temporary Audience attached to a Demand Gen ad group in the same request.
// #516 added custom audiences to the audienceSegments dimension; a 2026-11-06 validateOnly
// probe accepted the exact bodies this file sends: age 25-54 with three user lists and a custom
// audience (the live High Intent Audience's shape), a custom audience alone, and the first of
// those attached by temporary id to a Demand Gen ad group.
// The same probes refused, and so this file refuses before anything upstream:
//   - UNDETERMINED as a list VALUE (INVALID_ENUM_VALUE): it is the dimension's
//     includeUndetermined flag instead;
//   - a dimension with includeUndetermined and no value (DIMENSION_INVALID);
//   - an age segment off Google's bracket bounds, e.g. 30-50 (DIMENSION_INVALID);
//   - a name longer than 255 characters (TOO_LONG);
//   - a name already used in the account (NAME_ALREADY_IN_USE), which is how a retry lands.
// #519's real create and GAQL read-back (2026-11-06) showed Google DROPS a dimension naming every
// value and undetermined, since it restricts nobody, so this file leaves one out too: see
// coversEveryone.
// ---------------------------------------------------------------------------

// DemandGenDemographics is the demographic targeting of a Demand Gen campaign, built into
// a new Audience that every Demand Gen ad group of the campaign references, or, as
// DemandGenAdGroup.Demographics (#518), into one Audience for that ad group alone.
//
// Each list uses the vocabulary of its excluded* counterpart (ExcludedAgeRanges,
// ExcludedGenders, ExcludedIncomeRanges, ExcludedParentalStatuses): case-insensitive,
// deduplicated, at most maxDemographicExclusions entries. UNDETERMINED means "include the
// users Google could not classify" and needs at least one real value beside it. An empty
// list leaves that dimension out, so it does not restrict.
type DemandGenDemographics struct {
	AgeRanges        []string
	Genders          []string
	IncomeRanges     []string
	ParentalStatuses []string
	// AudienceSegments are EXISTING user lists on this campaign's own account
	// (customers/<id>/userLists/<id>) put in the same Audience, beside the demographics.
	// Optional. They name account resources, so the dispatch adapter and the create
	// service gate them as they gate DemandGenAudience.
	AudienceSegments []string
	// CustomAudiences are EXISTING custom audiences on this campaign's own account
	// (customers/<id>/customAudiences/<id>), put in the same audienceSegments dimension as
	// AudienceSegments, after them (#516). The live "CNCF - KubeCon NA - High Intent" Audience
	// holds its custom-intent segment that way. Optional, and gated as AudienceSegments is.
	CustomAudiences []string
}

func (d *DemandGenDemographics) empty() bool {
	return d == nil || len(d.AgeRanges)+len(d.Genders)+len(d.IncomeRanges)+len(d.ParentalStatuses)+len(d.AudienceSegments)+len(d.CustomAudiences) == 0
}

// The Audience wire shapes, Google's AudienceDimension one-of. Exactly one arm is set per
// dimension. includeUndetermined is always sent, false included, so the request states
// whether unclassified users are in.
type audienceDimension struct {
	Age              *ageDimension              `json:"age,omitempty"`
	Gender           *genderDimension           `json:"gender,omitempty"`
	HouseholdIncome  *householdIncomeDimension  `json:"householdIncome,omitempty"`
	ParentalStatus   *parentalStatusDimension   `json:"parentalStatus,omitempty"`
	AudienceSegments *audienceSegmentsDimension `json:"audienceSegments,omitempty"`
}

// ageSegment is AgeSegment: minAge, and maxAge unless the segment runs to 65+.
type ageSegment struct {
	MinAge int  `json:"minAge"`
	MaxAge *int `json:"maxAge,omitempty"`
}

type ageDimension struct {
	AgeRanges           []ageSegment `json:"ageRanges"`
	IncludeUndetermined bool         `json:"includeUndetermined"`
}

type genderDimension struct {
	Genders             []string `json:"genders"`
	IncludeUndetermined bool     `json:"includeUndetermined"`
}

type householdIncomeDimension struct {
	IncomeRanges        []string `json:"incomeRanges"`
	IncludeUndetermined bool     `json:"includeUndetermined"`
}

type parentalStatusDimension struct {
	ParentalStatuses    []string `json:"parentalStatuses"`
	IncludeUndetermined bool     `json:"includeUndetermined"`
}

type audienceSegmentsDimension struct {
	Segments []audienceSegment `json:"segments"`
}

// audienceSegment is AudienceSegment with the two arms this client sends: userList and
// customAudience (#516). Exactly one is set.
type audienceSegment struct {
	UserList       *userListInfo       `json:"userList,omitempty"`
	CustomAudience *customAudienceInfo `json:"customAudience,omitempty"`
}

// audienceCreate is the audiences:mutate create payload.
type audienceCreate struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Dimensions  []audienceDimension `json:"dimensions"`
}

// demandGenDemographicPlan is the validated Audience the cascade creates or reuses.
type demandGenDemographicPlan struct {
	create  audienceCreate
	summary string
	// attachTo is the step line's target: every Demand Gen ad group for the campaign-level
	// Audience, or the one ad group whose Demographics it holds (#518).
	attachTo string
}

const (
	// maxAudienceNameRunes is Audience.name's limit: 255 characters was accepted and 256
	// refused with TOO_LONG (validateOnly, 2026-11-05), multibyte characters counting one.
	maxAudienceNameRunes = 255
	// demographicAudienceNameInfix sits between the campaign name and the content hash.
	demographicAudienceNameInfix = " | Demographics "
	// errCodeAudienceNameInUse is AudienceError NAME_ALREADY_IN_USE.
	errCodeAudienceNameInUse = "NAME_ALREADY_IN_USE"
	audienceStatusEnabled    = "ENABLED"
	audienceScopeCustomer    = "CUSTOMER"
)

// ageBrackets is each AGE_RANGE_* enum as Google's AgeSegment bounds, in age order. These are
// the only bounds Google accepts; anything else is DIMENSION_INVALID.
var ageBrackets = []struct {
	enum     string
	min, max int // max 0: no upper bound (65+)
}{
	{"AGE_RANGE_18_24", 18, 24}, {"AGE_RANGE_25_34", 25, 34}, {"AGE_RANGE_35_44", 35, 44},
	{"AGE_RANGE_45_54", 45, 54}, {"AGE_RANGE_55_64", 55, 64}, {"AGE_RANGE_65_UP", 65, 0},
}

// genderTargetTypes maps the gender vocabulary to the GenderType enum.
var genderTargetTypes = map[string]string{"MALE": "MALE", "FEMALE": "FEMALE", "UNDETERMINED": "UNDETERMINED"}

// The value order of each dimension in the request, Google's enum order, so the same choice
// in any order sends the same bytes and hashes to the same Audience name.
var (
	demographicGenderOrder   = []string{"MALE", "FEMALE"}
	demographicParentalOrder = []string{"PARENT", "NOT_A_PARENT"}
)

// validateDemandGenDemographics resolves CampaignInput.DemandGenDemographics for kind.
// Pure, and run in preflightCampaignKind, so every refusal lands before the budget. Returns
// nil when the field is absent.
func validateDemandGenDemographics(kind, customerID, campaignName string, in CampaignInput) (*demandGenDemographicPlan, error) {
	d := in.DemandGenDemographics
	if d == nil {
		return nil, nil
	}
	if kind != campaignKindDemandGen {
		return nil, fmt.Errorf("google-ads: Demand Gen demographics are not supported on %s (this client builds them into the Audience of a Demand Gen ad group only); omit DemandGenDemographics; %s", kind, demographicFieldsHint(kind))
	}
	if d.empty() {
		return nil, fmt.Errorf("google-ads: DemandGenDemographics is set but empty; set at least one of AgeRanges, Genders, IncomeRanges, ParentalStatuses, AudienceSegments or CustomAudiences, or omit it")
	}
	// One Audience per ad group, and an existing Audience is not edited, so the
	// demographics cannot be merged into one the caller names.
	if in.DemandGenAudience != "" {
		return nil, fmt.Errorf("google-ads: DemandGenDemographics and DemandGenAudience cannot both be set (a Demand Gen ad group takes one Audience, and this client does not edit an existing one); put the demographics inside that Audience in the Google Ads UI, or omit DemandGenAudience and list its user lists in DemandGenDemographics.AudienceSegments")
	}
	for i, g := range in.DemandGenAdGroups {
		if g.Audience != "" {
			return nil, fmt.Errorf("google-ads: DemandGenDemographics cannot be combined with an Audience on Demand Gen ad group %d (every ad group gets the demographic Audience, and an ad group takes only one); omit that group's Audience, or omit DemandGenDemographics", i+1)
		}
	}
	// A group's own Demographics (#518) override the campaign-level ones for that group, so
	// when every group sets its own the campaign-level Audience would be attached to nothing.
	if n := len(in.DemandGenAdGroups); n > 0 && !slices.ContainsFunc(in.DemandGenAdGroups, func(g DemandGenAdGroup) bool { return g.Demographics == nil }) {
		return nil, fmt.Errorf("google-ads: DemandGenDemographics would be attached to no ad group: each of the %d Demand Gen ad groups sets its own Demographics, which replace the campaign-level ones for that group; omit DemandGenDemographics, or leave a group without its own", n)
	}
	return buildDemographicPlan(campaignDemographicOwner, customerID, campaignName, campaignName, d)
}

// demographicOwner says whose demographics a plan is built from, so one builder serves the
// campaign-level DemandGenDemographics (#498) and each DemandGenAdGroups entry's Demographics
// (#518) and its refusals name the field the caller set.
type demographicOwner struct {
	// field is the Go-side name of the object ("DemandGenDemographics"); its lists are
	// named field+".AudienceSegments" and so on.
	field string
	// group is the full ad group name of a DemandGenAdGroups entry, "" for the campaign.
	group string
	// useAudience is the remedy for an Audience resource named inside the object.
	useAudience string
}

var campaignDemographicOwner = demographicOwner{
	field:       "DemandGenDemographics",
	useAudience: "set DemandGenAudience to it and omit DemandGenDemographics",
}

func groupDemographicOwner(name string) demographicOwner {
	return demographicOwner{
		field:       fmt.Sprintf("the Demographics of Demand Gen ad group %q", name),
		group:       name,
		useAudience: "set that ad group's Audience to it and omit its Demographics",
	}
}

// list names one of the object's lists in a refusal.
func (o demographicOwner) list(name string) string {
	if o.group == "" {
		return o.field + "." + name
	}
	return fmt.Sprintf("Demographics.%s of Demand Gen ad group %q", name, o.group)
}

// wrap adds the ad group to a refusal that does not name its field (a value-vocabulary
// refusal), so the caller can tell which group's list it is about. The campaign-level
// refusals are returned unchanged.
func (o demographicOwner) wrap(err error) error {
	if err == nil || o.group == "" {
		return err
	}
	return fmt.Errorf("%w (in the Demographics of Demand Gen ad group %q)", err, o.group)
}

// attachTo is the step line's "to be attached to ..." target.
func (o demographicOwner) attachTo() string {
	if o.group == "" {
		return "every Demand Gen ad group"
	}
	return fmt.Sprintf("Demand Gen ad group %q", o.group)
}

// validateDemandGenGroupDemographics resolves each DemandGenAdGroups entry's Demographics
// (#518) onto its plan in groups (validateDemandGenAdGroups' output, in entry order): a new
// Audience of that group's own, built and named by the same rules as the campaign-level one.
// Pure, and run in preflightCampaignKind after validateDemandGenDemographics.
func validateDemandGenGroupDemographics(customerID, campaignName string, in CampaignInput, groups []demandGenGroupPlan) error {
	if len(in.DemandGenAdGroups) == 0 {
		return nil
	}
	if len(groups) != len(in.DemandGenAdGroups) {
		return fmt.Errorf("google-ads: internal error: %d Demand Gen ad group plans for %d entries", len(groups), len(in.DemandGenAdGroups))
	}
	for i, g := range in.DemandGenAdGroups {
		d := g.Demographics
		if d == nil {
			continue
		}
		name := groups[i].name
		owner := groupDemographicOwner(name)
		if d.empty() {
			return fmt.Errorf("google-ads: %s is set but empty; set at least one of AgeRanges, Genders, IncomeRanges, ParentalStatuses, AudienceSegments or CustomAudiences, or omit it", owner.field)
		}
		if g.Audience != "" {
			return fmt.Errorf("google-ads: Demand Gen ad group %q sets both Audience and Demographics (an ad group takes one Audience, and this client does not edit an existing one); put the demographics inside that Audience in the Google Ads UI, or omit its Audience and list the Audience's user lists in its Demographics.AudienceSegments", name)
		}
		// The group's label joins the campaign name in the Audience name, so each group's
		// Audience has a name of its own even when two hold the same dimensions.
		plan, err := buildDemographicPlan(owner, customerID, campaignName+" | "+sanitizeNamePart(g.Name), campaignName, d)
		if err != nil {
			return err
		}
		groups[i].demographics = plan
	}
	return nil
}

// buildDemographicPlan builds the Audience for one demographics object: its dimensions, in
// Google's order, and its deterministic name "<namePrefix> | Demographics <hash>". owner names
// the object in every refusal.
func buildDemographicPlan(owner demographicOwner, customerID, namePrefix, campaignName string, d *DemandGenDemographics) (*demandGenDemographicPlan, error) {
	var dims []audienceDimension
	var summary []string

	ages, ageUndetermined, err := demographicTargets("Demand Gen age range", "Demand Gen age ranges", d.AgeRanges, ageRangeTypes,
		"18-24, 25-34, 35-44, 45-54, 55-64, 65+ or UNDETERMINED (Google's AGE_RANGE_* enum names are accepted too)", "AGE_RANGE_UNDETERMINED")
	if err != nil {
		return nil, owner.wrap(err)
	}
	// A dimension naming every value AND UNDETERMINED restricts nobody, and Google drops it
	// from the Audience it stores (#519, read back from a real create on 2026-11-06), so it is
	// left out of the request too: the planned dimensions must be the ones Google holds, or a
	// retry's NAME_ALREADY_IN_USE reuse finds them different and dead-ends.
	var everyone []string
	switch {
	case coversEveryone(ages, ageUndetermined, len(ageBrackets)):
		everyone = append(everyone, "age")
	case len(ages) > 0:
		dims = append(dims, audienceDimension{Age: &ageDimension{AgeRanges: ageSegments(ages), IncludeUndetermined: ageUndetermined}})
		summary = append(summary, fmt.Sprintf("%d age range(s)", len(ages)))
	}
	genders, genderUndetermined, err := demographicTargets("Demand Gen gender", "Demand Gen genders", d.Genders, genderTargetTypes,
		"MALE, FEMALE or UNDETERMINED", "UNDETERMINED")
	if err != nil {
		return nil, owner.wrap(err)
	}
	switch {
	case coversEveryone(genders, genderUndetermined, len(demographicGenderOrder)):
		everyone = append(everyone, "gender")
	case len(genders) > 0:
		dims = append(dims, audienceDimension{Gender: &genderDimension{Genders: inOrder(demographicGenderOrder, genders), IncludeUndetermined: genderUndetermined}})
		summary = append(summary, fmt.Sprintf("%d gender(s)", len(genders)))
	}
	incomes, incomeUndetermined, err := demographicTargets("Demand Gen income range", "Demand Gen income ranges", d.IncomeRanges, incomeRangeTypes,
		"INCOME_RANGE_0_50, INCOME_RANGE_50_60, INCOME_RANGE_60_70, INCOME_RANGE_70_80, INCOME_RANGE_80_90, INCOME_RANGE_90_UP or UNDETERMINED", "INCOME_RANGE_UNDETERMINED")
	if err != nil {
		return nil, owner.wrap(err)
	}
	switch {
	case coversEveryone(incomes, incomeUndetermined, len(allIncomeRanges)-1): // allIncomeRanges ends with UNDETERMINED
		everyone = append(everyone, "income")
	case len(incomes) > 0:
		dims = append(dims, audienceDimension{HouseholdIncome: &householdIncomeDimension{IncomeRanges: inOrder(allIncomeRanges, incomes), IncludeUndetermined: incomeUndetermined}})
		summary = append(summary, fmt.Sprintf("%d income range(s)", len(incomes)))
	}
	parents, parentUndetermined, err := demographicTargets("Demand Gen parental status", "Demand Gen parental statuses", d.ParentalStatuses, parentalStatusTypes,
		"PARENT, NOT_A_PARENT or UNDETERMINED", "UNDETERMINED")
	if err != nil {
		return nil, owner.wrap(err)
	}
	switch {
	case coversEveryone(parents, parentUndetermined, len(demographicParentalOrder)):
		everyone = append(everyone, "parental status")
	case len(parents) > 0:
		dims = append(dims, audienceDimension{ParentalStatus: &parentalStatusDimension{ParentalStatuses: inOrder(demographicParentalOrder, parents), IncludeUndetermined: parentUndetermined}})
		summary = append(summary, fmt.Sprintf("%d parental status(es)", len(parents)))
	}
	lists, err := validateDemographicUserLists(owner, customerID, d.AudienceSegments)
	if err != nil {
		return nil, err
	}
	customs, err := validateDemographicCustomAudiences(owner, customerID, d.CustomAudiences)
	if err != nil {
		return nil, err
	}
	if len(lists)+len(customs) > 0 {
		// One audienceSegments dimension: the user lists, then the custom audiences (#516),
		// each sorted, so the same segments in another order are the same Audience. A plan with
		// user lists only sends the same bytes as before #516, so its name hash is unchanged.
		slices.Sort(lists)
		slices.Sort(customs)
		segs := make([]audienceSegment, 0, len(lists)+len(customs))
		for _, l := range lists {
			segs = append(segs, audienceSegment{UserList: &userListInfo{UserList: l}})
		}
		for _, ca := range customs {
			segs = append(segs, audienceSegment{CustomAudience: &customAudienceInfo{CustomAudience: ca}})
		}
		dims = append(dims, audienceDimension{AudienceSegments: &audienceSegmentsDimension{Segments: segs}})
		if len(lists) > 0 {
			summary = append(summary, fmt.Sprintf("%d user list(s)", len(lists)))
		}
		if len(customs) > 0 {
			summary = append(summary, fmt.Sprintf("%d custom audience(s)", len(customs)))
		}
	}

	if len(dims) == 0 {
		return nil, fmt.Errorf("google-ads: %s restricts nobody: each dimension it sets (%s) names every value and UNDETERMINED, and it names no user list or custom audience, so Google would store an Audience with no dimensions; narrow a dimension, add a segment, or omit %s", owner.field, strings.Join(everyone, ", "), owner.field)
	}
	if len(everyone) > 0 {
		summary = append(summary, "every "+strings.Join(everyone, ", ")+" (no restriction, left out)")
	}

	return &demandGenDemographicPlan{
		create: audienceCreate{
			Name:        demographicAudienceName(namePrefix, dims),
			Description: "Created by LFX for " + campaignName,
			Dimensions:  dims,
		},
		summary:  strings.Join(summary, ", "),
		attachTo: owner.attachTo(),
	}, nil
}

// demographicFieldsHint names the demographic fields a non-Demand Gen channel does accept, so
// the refusal never points at a field the next check refuses.
func demographicFieldsHint(kind string) string {
	switch kind {
	case campaignKindSearch:
		return "use ExcludedAgeRanges, ExcludedGenders, ExcludedIncomeRanges or ObservedDemographics instead"
	case campaignKindDisplay:
		return "use ExcludedAgeRanges, ExcludedGenders, ExcludedIncomeRanges or ExcludedParentalStatuses instead"
	case campaignKindVideo:
		return "use ExcludedAgeRanges or ExcludedGenders instead"
	default:
		return "this channel takes no demographic field here"
	}
}

// demographicTargets resolves one demographic list: the real values (without UNDETERMINED)
// and whether UNDETERMINED was named. Google takes UNDETERMINED only as the dimension's
// includeUndetermined flag, and refuses that flag on a dimension with no value, so a list
// holding UNDETERMINED alone is refused here.
func demographicTargets(noun, plural string, values []string, allowed map[string]string, choices, undetermined string) ([]string, bool, error) {
	enums, err := validateEnumExclusions(noun, plural, values, allowed, choices)
	if err != nil {
		return nil, false, err
	}
	out := make([]string, 0, len(enums))
	include := false
	for _, e := range enums {
		if e == undetermined {
			include = true
			continue
		}
		out = append(out, e)
	}
	if include && len(out) == 0 {
		return nil, false, fmt.Errorf("google-ads: %s list holds only UNDETERMINED; Google needs at least one real value in the dimension (UNDETERMINED only adds the users it could not classify), so add one or omit the list", strings.TrimPrefix(noun, "Demand Gen "))
	}
	return out, include, nil
}

// coversEveryone reports whether a resolved dimension names every one of its n values and
// UNDETERMINED, which is no restriction at all.
func coversEveryone(values []string, undetermined bool, n int) bool {
	return undetermined && len(values) == n
}

// inOrder returns the members of set in the order of order.
func inOrder(order, set []string) []string {
	out := make([]string, 0, len(set))
	for _, v := range order {
		if slices.Contains(set, v) {
			out = append(out, v)
		}
	}
	return out
}

// ageSegments turns AGE_RANGE_* enums into AgeSegments, merging adjacent brackets into one
// segment as Google stores them (the live 25-54 Audience is one segment, not three).
func ageSegments(enums []string) []ageSegment {
	var out []ageSegment
	prev := -2
	for i, b := range ageBrackets {
		if !slices.Contains(enums, b.enum) {
			continue
		}
		var maxAge *int
		if b.max != 0 {
			m := b.max
			maxAge = &m
		}
		if i == prev+1 && len(out) > 0 {
			out[len(out)-1].MaxAge = maxAge
		} else {
			out = append(out, ageSegment{MinAge: b.min, MaxAge: maxAge})
		}
		prev = i
	}
	return out
}

// validateDemographicUserLists checks DemandGenDemographics.AudienceSegments: full
// customers/<this account>/userLists/<id> names, canonical ids, deduplicated, at most
// maxAudienceSegments.
func validateDemographicUserLists(owner demographicOwner, customerID string, names []string) ([]string, error) {
	field := owner.list("AudienceSegments")
	if len(names) == 0 {
		return nil, nil
	}
	if len(names) > maxAudienceSegments {
		return nil, fmt.Errorf("google-ads: at most %d %s entries are supported, got %d", maxAudienceSegments, field, len(names))
	}
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, fmt.Errorf("google-ads: %s entry must not be empty", field)
		}
		parts := strings.Split(name, "/")
		if len(parts) == 4 && parts[0] == "customers" && parts[2] == "audiences" {
			return nil, fmt.Errorf("google-ads: %s entry %q is an Audience, which cannot go inside another Audience; name its user lists here, or %s", field, capForError(name), owner.useAudience)
		}
		if len(parts) == 4 && parts[0] == "customers" && parts[2] == "customAudiences" {
			return nil, fmt.Errorf("google-ads: %s entry %q is a custom audience; AudienceSegments takes user lists only, so move it to %s", field, capForError(name), owner.list("CustomAudiences"))
		}
		if len(parts) != 4 || parts[0] != "customers" || parts[2] != "userLists" || !numericID(parts[1]) || !numericID(parts[3]) {
			return nil, fmt.Errorf("google-ads: %s entry %q is not a customers/<id>/userLists/<id> user list resource name", field, capForError(name))
		}
		if parts[1] != customerID {
			return nil, fmt.Errorf("google-ads: %s entry %q is on customer %q, not this campaign's account %q; name a user list on the campaign's own account", field, capForError(name), parts[1], customerID)
		}
		if canonicalCampaignID(parts[3]) == "" {
			return nil, fmt.Errorf("google-ads: %s entry %q has the id %q, which is not the canonical base-10 spelling of a positive id (no leading zeros)", field, capForError(name), parts[3])
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

// validateDemographicCustomAudiences checks DemandGenDemographics.CustomAudiences (#516): full
// customers/<this account>/customAudiences/<id> names, canonical ids, deduplicated, at most
// maxAudienceSegments. A resource of another kind is refused with the field it belongs in, or,
// for the kinds an Audience cannot hold, with why: Google refuses a legacy customInterests
// resource inside an Audience (RESOURCE_NAME_MALFORMED, validateOnly probe 2026-11-06).
func validateDemographicCustomAudiences(owner demographicOwner, customerID string, names []string) ([]string, error) {
	field := owner.list("CustomAudiences")
	if len(names) == 0 {
		return nil, nil
	}
	if len(names) > maxAudienceSegments {
		return nil, fmt.Errorf("google-ads: at most %d %s entries are supported, got %d", maxAudienceSegments, field, len(names))
	}
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, fmt.Errorf("google-ads: %s entry must not be empty", field)
		}
		parts := strings.Split(name, "/")
		if len(parts) == 4 && parts[0] == "customers" {
			switch parts[2] {
			case "userLists":
				return nil, fmt.Errorf("google-ads: %s entry %q is a user list; move it to %s", field, capForError(name), owner.list("AudienceSegments"))
			case "audiences":
				return nil, fmt.Errorf("google-ads: %s entry %q is an Audience, which cannot go inside another Audience; name its custom audiences here, or %s", field, capForError(name), owner.useAudience)
			case "customInterests":
				return nil, fmt.Errorf("google-ads: %s entry %q is a legacy custom interest, which Google refuses inside an Audience; name a customers/<id>/customAudiences/<id> custom audience instead", field, capForError(name))
			case "userInterests":
				return nil, fmt.Errorf("google-ads: %s entry %q is a user interest, not a custom audience; name a customers/<id>/customAudiences/<id> custom audience", field, capForError(name))
			}
		}
		if len(parts) != 4 || parts[0] != "customers" || parts[2] != "customAudiences" || !numericID(parts[1]) || !numericID(parts[3]) {
			return nil, fmt.Errorf("google-ads: %s entry %q is not a customers/<id>/customAudiences/<id> custom audience resource name", field, capForError(name))
		}
		if parts[1] != customerID {
			return nil, fmt.Errorf("google-ads: %s entry %q is on customer %q, not this campaign's account %q; name a custom audience on the campaign's own account", field, capForError(name), parts[1], customerID)
		}
		if canonicalCampaignID(parts[3]) == "" {
			return nil, fmt.Errorf("google-ads: %s entry %q has the id %q, which is not the canonical base-10 spelling of a positive id (no leading zeros)", field, capForError(name), parts[3])
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

// demographicAudienceName is "<campaign name> | Demographics <hash>". The hash is the first
// 16 hex characters of the SHA-256 of the campaign name and the dimensions, so a retry of
// the same create composes the same name (and lands on NAME_ALREADY_IN_USE, then reuses the
// Audience), while a changed choice gets a new one. The campaign name is shortened, never
// the hash, when the whole would pass Google's 255-character limit; the hash still covers
// the full campaign name.
func demographicAudienceName(campaignName string, dims []audienceDimension) string {
	body, _ := json.Marshal(dims) // plain structs of strings, ints and bools: cannot fail
	sum := sha256.Sum256(append([]byte(campaignName+"\n"), body...))
	suffix := demographicAudienceNameInfix + hex.EncodeToString(sum[:8])
	prefix := campaignName
	if room := maxAudienceNameRunes - utf8.RuneCountInString(suffix); utf8.RuneCountInString(prefix) > room {
		prefix = strings.TrimRight(string([]rune(prefix)[:room]), " ")
	}
	return prefix + suffix
}

// gaqlAudienceRow is one audience GAQL row.
type gaqlAudienceRow struct {
	Audience struct {
		ResourceName string            `json:"resourceName"`
		Name         string            `json:"name"`
		Status       string            `json:"status"`
		Scope        string            `json:"scope"`
		Dimensions   []json.RawMessage `json:"dimensions"`
		// ExclusionDimension is any exclusion added to the Audience; this client never
		// sends one, so a non-empty value means the Audience is not the one it planned.
		ExclusionDimension json.RawMessage `json:"exclusionDimension"`
	} `json:"audience"`
}

// ensureDemandGenDemographicAudience creates the plan's Audience, or reuses the one an
// earlier attempt created under the same name, and returns its resource name and "created"
// or "reused". It runs BEFORE the budget mutate, so no failure leaves a campaign. unconfirmed
// is true when the create may have landed (an ambiguous failure, or a 2xx naming no usable
// Audience): the caller must then return a partial naming the planned Audience, not a
// pre-create failure. An Audience found under the name is used only when it is ENABLED, of
// CUSTOMER scope, has no exclusions, and holds exactly the planned dimensions; it is never
// edited.
func (c *Client) ensureDemandGenDemographicAudience(ctx context.Context, plan *demandGenDemographicPlan) (resource, how string, unconfirmed bool, err error) {
	name := plan.create.Name
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", "", false, fmt.Errorf("google-ads demand gen demographic Audience %q not created (context already done; nothing was created): %w", name, ctxErr)
	}
	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("audiences:mutate"),
		mutateRequest{Operations: []mutateOperation{{Create: plan.create}}}, false)
	if err != nil {
		var ae *apiError
		switch {
		case errors.As(err, &ae) && isDefiniteClientError(ae) && ae.hasErrorCode(errCodeAudienceNameInUse):
			rn, lookupErr := c.demographicAudienceNamed(ctx, plan)
			if lookupErr != nil {
				return "", "", false, fmt.Errorf("google-ads demand gen demographic Audience %q already exists (NAME_ALREADY_IN_USE) and could not be reused: %w; no budget or campaign was created", name, lookupErr)
			}
			return rn, "reused", false, nil
		case createOutcomeAmbiguous(err):
			return "", "", true, fmt.Errorf("google-ads demand gen demographic Audience creation UNCONFIRMED (%q may exist; no budget or campaign was created, and a retry reuses an Audience of that name): %w", name, err)
		default:
			return "", "", false, fmt.Errorf("google-ads demand gen demographic Audience creation failed (no budget or campaign was created): %w", err)
		}
	}
	rn, _, err := firstResourceName(resp)
	if err == nil {
		err = c.validateResourceKind("audiences", rn, true)
	}
	if err != nil {
		return "", "", true, fmt.Errorf("google-ads demand gen demographic Audience creation UNCONFIRMED (%q may exist; no budget or campaign was created, and a retry reuses an Audience of that name): %w", name, err)
	}
	return rn, "created", false, nil
}

// demographicAudienceNamed finds the Audience holding plan's name, read-only, and returns
// it when it is safe to attach: exactly one row, of this account, with that exact name,
// ENABLED, and holding exactly the planned dimensions.
func (c *Client) demographicAudienceNamed(ctx context.Context, plan *demandGenDemographicPlan) (string, error) {
	name := plan.create.Name
	literal, err := gaqlNameLiteral("audience", name)
	if err != nil {
		return "", err
	}
	rows, err := c.gaqlSearch(ctx, "SELECT audience.resource_name, audience.name, audience.status, audience.scope, audience.dimensions, audience.exclusion_dimension FROM audience WHERE audience.name = "+literal)
	if err != nil {
		return "", fmt.Errorf("look up the Audience by name: %w", err)
	}
	if len(rows) != 1 {
		return "", fmt.Errorf("the lookup by name returned %d Audiences, want exactly 1", len(rows))
	}
	var row gaqlAudienceRow
	if err := json.Unmarshal(rows[0], &row); err != nil {
		return "", fmt.Errorf("decode the Audience row: %w", err)
	}
	a := row.Audience
	if err := c.validateResourceKind("audiences", a.ResourceName, true); err != nil {
		return "", err
	}
	if a.Name != name {
		return "", fmt.Errorf("the Audience %s is named %q, not %q", a.ResourceName, capForError(a.Name), name)
	}
	if a.Status != audienceStatusEnabled {
		return "", fmt.Errorf("the Audience %s is %s, not ENABLED; rename it in Google Ads, or change the demographics", a.ResourceName, a.Status)
	}
	if a.Scope != audienceScopeCustomer {
		return "", fmt.Errorf("the Audience %s has scope %q, not CUSTOMER, and is left as it is; rename it in Google Ads, or change the demographics", a.ResourceName, a.Scope)
	}
	if ex := bytes.TrimSpace(a.ExclusionDimension); len(ex) > 0 && !bytes.Equal(ex, []byte("null")) && !bytes.Equal(ex, []byte("{}")) {
		return "", fmt.Errorf("the Audience %s carries exclusions this client never set and is left as it is; rename it in Google Ads, or change the demographics", a.ResourceName)
	}
	if !sameAudienceDimensions(a.Dimensions, plan.create.Dimensions) {
		return "", fmt.Errorf("the Audience %s holds dimensions that differ from the planned ones and is left as it is; rename it in Google Ads, or change the demographics", a.ResourceName)
	}
	return a.ResourceName, nil
}

// sameAudienceDimensions reports whether the dimensions Google returned hold exactly want,
// regardless of order: the dimensions, their value lists and the segments are compared as
// sets, because how Google orders a persisted Audience on read-back has not been verified
// live (only validateOnly creates were probed). A false includeUndetermined that Google omits
// decodes as false. Every planned dimension carries exactly one arm, so a returned dimension
// or segment of a kind this client never sends (a user interest, say) differs: it decodes
// with no arm set, which no planned segment matches.
func sameAudienceDimensions(got []json.RawMessage, want []audienceDimension) bool {
	if len(got) != len(want) {
		return false
	}
	decoded := make([]audienceDimension, 0, len(got))
	for _, raw := range got {
		var d audienceDimension
		if err := json.Unmarshal(raw, &d); err != nil {
			return false
		}
		decoded = append(decoded, d)
	}
	return slices.Equal(canonicalDimensions(decoded), canonicalDimensions(want))
}

// canonicalDimensions renders each dimension with its lists sorted, then sorts the renderings,
// so two dimension lists holding the same content compare equal.
func canonicalDimensions(dims []audienceDimension) []string {
	out := make([]string, 0, len(dims))
	for _, d := range dims {
		c := audienceDimension{}
		if d.Age != nil {
			segs := slices.Clone(d.Age.AgeRanges)
			slices.SortFunc(segs, func(x, y ageSegment) int { return x.MinAge - y.MinAge })
			c.Age = &ageDimension{AgeRanges: segs, IncludeUndetermined: d.Age.IncludeUndetermined}
		}
		if d.Gender != nil {
			c.Gender = &genderDimension{Genders: sortedCopy(d.Gender.Genders), IncludeUndetermined: d.Gender.IncludeUndetermined}
		}
		if d.HouseholdIncome != nil {
			c.HouseholdIncome = &householdIncomeDimension{IncomeRanges: sortedCopy(d.HouseholdIncome.IncomeRanges), IncludeUndetermined: d.HouseholdIncome.IncludeUndetermined}
		}
		if d.ParentalStatus != nil {
			c.ParentalStatus = &parentalStatusDimension{ParentalStatuses: sortedCopy(d.ParentalStatus.ParentalStatuses), IncludeUndetermined: d.ParentalStatus.IncludeUndetermined}
		}
		if d.AudienceSegments != nil {
			segs := slices.Clone(d.AudienceSegments.Segments)
			slices.SortFunc(segs, func(x, y audienceSegment) int { return strings.Compare(segmentKey(x), segmentKey(y)) })
			c.AudienceSegments = &audienceSegmentsDimension{Segments: segs}
		}
		body, _ := json.Marshal(c) // plain structs: cannot fail
		out = append(out, string(body))
	}
	slices.Sort(out)
	return out
}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// segmentKey orders segments for comparison. The two arms' resource names never collide
// (userLists vs customAudiences), and an armless segment sorts first.
func segmentKey(s audienceSegment) string {
	switch {
	case s.UserList != nil:
		return s.UserList.UserList
	case s.CustomAudience != nil:
		return s.CustomAudience.CustomAudience
	}
	return ""
}

// demographicAudienceStep is the step line for the Audience.
func demographicAudienceStep(how, resource string, plan *demandGenDemographicPlan) string {
	return fmt.Sprintf("Demographic Audience %s: %s (%s), to be attached to %s", how, resource, plan.summary, plan.attachTo)
}

// DemandGenAdGroupAudience is one Demand Gen ad group's own demographic Audience (#518) in
// CampaignResult.DemandGenAdGroupAudiences.
type DemandGenAdGroupAudience struct {
	// AdGroupName is the full ad group name the Audience is attached to.
	AdGroupName string `json:"adGroupName"`
	// DemographicAudience is the Audience's resource name, empty when its create is
	// UNCONFIRMED.
	DemographicAudience string `json:"demographicAudience,omitempty"`
	// DemographicAudienceName is the planned deterministic name, the key to find the
	// Audience by when DemographicAudience is empty.
	DemographicAudienceName string `json:"demographicAudienceName"`
}

// demographicAudiencesInPlace reports whether result names any demographic Audience this
// attempt created, reused or may have created before the budget: the campaign-level one or any
// ad group's (#518).
func demographicAudiencesInPlace(result *CampaignResult) bool {
	return result.DemandGenDemographicAudience != "" || len(result.DemandGenAdGroupAudiences) > 0
}

// demographicAudienceResources lists the resource names of the demographic Audiences result
// names, campaign-level first, for an error sentence.
func demographicAudienceResources(result *CampaignResult) []string {
	var out []string
	if result.DemandGenDemographicAudience != "" {
		out = append(out, result.DemandGenDemographicAudience)
	}
	for _, a := range result.DemandGenAdGroupAudiences {
		if a.DemographicAudience != "" {
			out = append(out, a.DemographicAudience)
		}
	}
	return out
}
