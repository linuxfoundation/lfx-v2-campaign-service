// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package audience

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDiscoveryHidden uses the names the live AgntCon Japan discovery offered.
func TestDiscoveryHidden(t *testing.T) {
	hidden := map[string]string{
		"LF Global Opt-Outs":                                                  "suppression",
		"24Q1 - LF Events - GDPR Suppression":                                 "suppression",
		"23Q1 - LF - Master Exclusion List":                                   "suppression",
		"23Q3 - LF Events - Suppression List":                                 "suppression",
		"26Q3 - CNCF - AGNTCon + MCPCon Japan - Master":                       "master",
		"26Q3 - AGNTCon + MCPCon Japan - APAC Sibling Registrants [psh-test]": "test",
		"[E2E test] 26Q4 - AGNTCon + MCPCon Japan - Web Visitors":             "test",
		"Mar26_lfstaff - lf_staff.csv":                                        "staff",
		"25Q4 - LF Internal Staff Emails Marketing contact":                   "staff",
	}
	for name, want := range hidden {
		got, ok := DiscoveryHidden(name, false)
		assert.True(t, ok, name)
		assert.Equal(t, want, got, name)
	}
	_, ok := DiscoveryHidden("Japan_final1.xlsx", true)
	assert.True(t, ok, "an unclassified upload is hidden")
	_, ok = DiscoveryHidden("Japan_final1.xlsx", false)
	assert.False(t, ok, "a classified upload carries evidence and stays")
	_, ok = DiscoveryHidden("26Q3 - AGNTCon + MCPCon Japan 2026 Registrants", false)
	assert.False(t, ok)
}

func TestDominantBrand(t *testing.T) {
	assert.Equal(t, "AAIF", DominantBrand([]string{
		"26Q3 - AAIF - AGNTCon + MCPCon Japan 2026 - Event Registered Japan",
		"26Q3 - AAIF - AGNTCon + MCPCon Japan 2026 - APAC Event Registrants",
		"26Q3 - CNCF - AGNTCon + MCPCon Japan - Master",
		"26Q3 - AGNTCon + MCPCon Japan 2026 Registrants",
		"26Q3 - AGNTCon + MCPCon Japan - APAC Sibling Registrants",
		"26Q3 - AGNTCon + MCPCon Japan - Web Visitors",
		"26Q3 - AGNTCon + MCPCon Japan - Speakers",
	}, "AGNTCon + MCPCon Japan"), "the event's own name in the brand slot is not its brand (seen live)")
	assert.Empty(t, DominantBrand([]string{"26Q3 - AAIF - X - y", "26Q3 - CNCF - X - y"}, ""), "a tie is not a brand")
	assert.Empty(t, DominantBrand([]string{"26Q3 - AAIF - X - y"}, ""), "one name is not agreement")
	assert.Empty(t, DominantBrand([]string{"26Q3 - LF Events - A - b", "26Q3 - LF Events - C - d"}, ""), "a generic segment is not a brand")
}

func TestGuardReuse_TheLiveTokyoRanking(t *testing.T) {
	c := CustomCriteria{Cities: []string{"Tokyo"}, ContactLocation: true, Label: "AGNTCon + MCPCon Japan"}
	out := GuardReuse([]CustomSimilarList{
		{ListRow: ListRow{ListID: "1", Name: "25Q3 - Events - Open Compliance Summit (OCS) Tokyo 2025"}, Recommendation: CustomReuseAsIs},
		{ListRow: ListRow{ListID: "2", Name: "26Q3 - Tokyo + 3hr Shinkansen Catchment - Marketable"}, Recommendation: CustomReuseAsIs},
		{ListRow: ListRow{ListID: "3", Name: "23Q2 - Hyperledger - Meetup - Tokyo"}, Recommendation: CustomReuseUpdate},
		{ListRow: ListRow{ListID: "4", Name: "26Q3 - AGNTCon + MCPCon Japan 2026 Registrants"}, Recommendation: CustomReuseUpdate},
	}, c)
	got := map[string]CustomReuseRecommendation{}
	for _, s := range out {
		got[s.ListID] = s.Recommendation
	}
	assert.Equal(t, CustomReuseNone, got["1"], "another event's list is not this audience")
	assert.Equal(t, CustomReuseAsIs, got["2"], "a general audience list for the place stands")
	assert.Equal(t, CustomReuseNone, got["3"])
	assert.Equal(t, CustomReuseUpdate, got["4"], "a list of this event stands")
	assert.Equal(t, "2", out[0].ListID, "reuse sorts first")
}

// A restricted audience is narrower than any list whose name does not show the
// restriction, so the model's "reuse" of a general list is held to "update".
func TestGuardReuse_ARestrictedAudienceNeverReusesABroaderList(t *testing.T) {
	general := CustomSimilarList{ListRow: ListRow{ListID: "2", Name: "26Q3 - Seoul - Mailable Contacts"}, Recommendation: CustomReuseAsIs}
	speakers := CustomSimilarList{ListRow: ListRow{ListID: "5", Name: "26Q3 - Seoul - Speakers - Current - OSS Korea"}, Recommendation: CustomReuseAsIs}
	base := CustomCriteria{Cities: []string{"Seoul"}, ContactLocation: true, Label: "OSS Korea"}

	got := func(c CustomCriteria, s CustomSimilarList) CustomReuseRecommendation {
		return GuardReuse([]CustomSimilarList{s}, c)[0].Recommendation
	}
	assert.Equal(t, CustomReuseAsIs, got(base, general), "an unrestricted audience keeps the model's reuse")

	withSpeakers := base
	withSpeakers.Speakers = &CustomSpeakers{Scope: SpeakerScopeCurrent}
	assert.Equal(t, CustomReuseUpdate, got(withSpeakers, general), "a general list is broader than a speakers audience")
	assert.Equal(t, CustomReuseAsIs, got(withSpeakers, speakers), "a speakers list carries the restriction")

	withAmbassadors := base
	withAmbassadors.Ambassadors = true
	assert.Equal(t, CustomReuseUpdate, got(withAmbassadors, general))

	withFilter := base
	withFilter.PropertyFilters = []CustomPropertyFilter{{Property: "jobtitle", Operator: CustomOpContains, Values: []string{"CTO"}}}
	assert.Equal(t, CustomReuseUpdate, got(withFilter, general), "a property filter never shows in a name")

	// The canonical location draft carries course topics as its brand fit; they are
	// no restriction, or this service would never reuse a list it built itself.
	canonical := base
	canonical.EducationEnrolled = true
	canonical.EducationTopics = []string{"Kubernetes"}
	assert.Equal(t, CustomReuseAsIs, got(canonical, general), "topics do not block reuse")
}

func TestSettleQuestions(t *testing.T) {
	qs := []CustomQuestion{{ID: "mailability"}, {ID: "topics"}}
	assert.Len(t, SettleQuestions(qs, []CustomAnswer{{QuestionID: "mailability"}}), 1, "an answered question is not asked again")
	assert.Empty(t, SettleQuestions(qs, make([]CustomAnswer, customMaxAnswerRounds)), "after enough rounds the draft is final")
}

func TestWithEventYear(t *testing.T) {
	assert.Equal(t, "AGNTCon + MCPCon Japan 2026", WithEventYear("AGNTCon + MCPCon Japan", []string{"2026-09-10"}))
	assert.Equal(t, "KubeCon 2025", WithEventYear("KubeCon 2025", []string{"2026-03-01"}), "a name with a year keeps it")
	assert.Equal(t, "KubeCon", WithEventYear("KubeCon", nil))
}
