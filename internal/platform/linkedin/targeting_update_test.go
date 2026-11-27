// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func skills(ids ...string) map[string][]string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = "urn:li:skill:" + id
	}
	return map[string][]string{"skills": out}
}

func TestPrepareTargetingUpdate_AddsTheCreateLocaleOnlyWhenNoneIsNamed(t *testing.T) {
	u, err := PrepareTargetingUpdate(&Targeting{Include: []map[string][]string{skills("17")}}, []string{"urn:li:geo:103644278"}, nil)
	if err != nil {
		t.Fatalf("PrepareTargetingUpdate: %v", err)
	}
	if !u.LocaleAdded || len(u.Targeting.Include) != 2 || u.Targeting.Include[1][FacetInterfaceLocales][0] != UpdateLocale {
		t.Fatalf("include = %v, added = %v; want the skills clause then {interfaceLocales: [en_US]}", u.Targeting.Include, u.LocaleAdded)
	}
	// Idempotent: preparing the output again adds nothing.
	again, err := PrepareTargetingUpdate(u.Targeting, u.GeoURNs, nil)
	if err != nil || again.LocaleAdded || len(again.Targeting.Include) != 2 {
		t.Fatalf("re-prepare: %v, %+v", err, again)
	}

	own := map[string][]string{"skills": {"urn:li:skill:17"}, FacetInterfaceLocales: {"urn:li:locale:de_DE"}}
	u, err = PrepareTargetingUpdate(&Targeting{Include: []map[string][]string{own}}, []string{"urn:li:geo:1"}, nil)
	if err != nil || u.LocaleAdded || len(u.Targeting.Include) != 1 {
		t.Fatalf("a caller's own interfaceLocales must be kept as is: %v, %+v", err, u)
	}
}

func TestPrepareTargetingUpdate_Refusals(t *testing.T) {
	ten := make([]map[string][]string, MaxTargetingClauses)
	for i := range ten {
		ten[i] = skills("1" + strings.Repeat("0", i))
	}
	geo := []string{"urn:li:geo:1"}
	for _, tc := range []struct {
		name string
		t    *Targeting
		geo  []string
		want string
	}{
		{"nil targeting", nil, geo, "targeting is required"},
		{"no geo", &Targeting{Include: []map[string][]string{skills("1")}}, nil, "geo_targets needs at least one"},
		{"malformed geo", &Targeting{Include: []map[string][]string{skills("1")}}, []string{"urn:li:geo:01"}, "is not urn:li:geo:<id>"},
		{"padded geo", &Targeting{Include: []map[string][]string{skills("1")}}, []string{" urn:li:geo:1"}, "is not urn:li:geo:<id>"},
		{"empty include", &Targeting{}, geo, "include needs at least one clause"},
		{"invalid combination", &Targeting{Include: []map[string][]string{{"titles": {"urn:li:title:1"}, "seniorities": {"urn:li:seniority:1"}}}}, geo, "titles"},
		{"ten clauses, no locale", &Targeting{Include: ten}, geo, "names no interfaceLocales"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PrepareTargetingUpdate(tc.t, tc.geo, nil)
			if !errors.Is(err, ErrTargetingInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrTargetingInvalid naming %q", err, tc.want)
			}
			if reason, ok := TargetingUpdateReason(err); !ok || strings.HasPrefix(reason, ErrTargetingInvalid.Error()) {
				t.Errorf("reason = %q, %v", reason, ok)
			}
		})
	}
	// 101 distinct locations pass LinkedIn's per-facet cap; duplicates do not count.
	many := make([]string, 0, 102)
	for i := 1; i <= 101; i++ {
		many = append(many, "urn:li:geo:"+itoa(i))
	}
	if _, err := PrepareTargetingUpdate(&Targeting{Include: []map[string][]string{skills("1")}}, many, nil); !errors.Is(err, ErrTargetingInvalid) {
		t.Errorf("101 locations accepted: %v", err)
	}
	if u, err := PrepareTargetingUpdate(&Targeting{Include: []map[string][]string{skills("1")}}, []string{"urn:li:geo:1", "urn:li:geo:1"}, nil); err != nil || len(u.GeoURNs) != 1 {
		t.Errorf("duplicate locations: %v, %+v", err, u)
	}
}

// The write is one PARTIAL_UPDATE whose $set carries targetingCriteria and nothing else: the geo
// clause first, the caller's clause, the added locale clause, and the row's employer exclusions
// merged into exclude.
func TestUpdateCampaignTargeting_WritesTheWholeCriteriaOnly(t *testing.T) {
	s, c := newLeverStub(t)
	u, err := PrepareTargetingUpdate(&Targeting{
		Include: []map[string][]string{skills("17")},
		Exclude: map[string][]string{"employers": {"urn:li:organization:5"}},
	}, []string{"urn:li:geo:103644278"}, []string{"urn:li:organization:208777"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.UpdateCampaignTargeting(context.Background(), "777", u); err != nil {
		t.Fatalf("UpdateCampaignTargeting: %v", err)
	}
	reqs := s.requests()
	if len(reqs) != 1 || reqs[0].Method != http.MethodPost || reqs[0].Path != "/adAccounts/509430019/adCampaigns/777" || reqs[0].RestliMethod != "PARTIAL_UPDATE" {
		t.Fatalf("requests = %+v", reqs)
	}
	var body struct {
		Patch struct {
			Set map[string]json.RawMessage `json:"$set"`
		} `json:"patch"`
	}
	if err := json.Unmarshal([]byte(reqs[0].Body), &body); err != nil {
		t.Fatalf("body %s: %v", reqs[0].Body, err)
	}
	if len(body.Patch.Set) != 1 || body.Patch.Set["targetingCriteria"] == nil {
		t.Fatalf("$set = %s, want targetingCriteria alone", reqs[0].Body)
	}
	var crit struct {
		Include struct {
			And []struct {
				Or map[string][]string `json:"or"`
			} `json:"and"`
		} `json:"include"`
		Exclude struct {
			Or map[string][]string `json:"or"`
		} `json:"exclude"`
	}
	if err := json.Unmarshal(body.Patch.Set["targetingCriteria"], &crit); err != nil {
		t.Fatal(err)
	}
	and := crit.Include.And
	if len(and) != 3 ||
		and[0].Or["urn:li:adTargetingFacet:locations"][0] != "urn:li:geo:103644278" ||
		and[1].Or["urn:li:adTargetingFacet:skills"][0] != "urn:li:skill:17" ||
		and[2].Or["urn:li:adTargetingFacet:interfaceLocales"][0] != UpdateLocale {
		t.Errorf("include.and = %+v", and)
	}
	if got := crit.Exclude.Or["urn:li:adTargetingFacet:employers"]; len(got) != 2 || got[1] != "urn:li:organization:208777" {
		t.Errorf("exclude employers = %v, want the caller's then the row's", got)
	}
	for _, never := range []string{"status", "dailyBudget", "unitCost", "offsiteDeliveryEnabled"} {
		if strings.Contains(reqs[0].Body, never) {
			t.Errorf("the targeting write sent %s", never)
		}
	}
}

func TestUpdateCampaignTargeting_Classification(t *testing.T) {
	for _, tc := range []struct {
		name            string
		code            int
		wantRefused     bool
		wantUnconfirmed bool
	}{
		{"204 confirmed", 204, false, false},
		{"400 refused", 400, true, false},
		{"422 refused", 422, true, false},
		{"403 definite", 403, false, false},
		{"429 unconfirmed", 429, false, true},
		{"503 unconfirmed", 503, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, c := newLeverStub(t)
			s.writeCode = tc.code
			s.writeBody = `{"status":` + itoa(tc.code) + `,"message":"upstream text"}`
			u, _ := PrepareTargetingUpdate(&Targeting{Include: []map[string][]string{skills("17")}}, []string{"urn:li:geo:1"}, nil)
			err := c.UpdateCampaignTargeting(context.Background(), "777", u)
			if tc.code == 204 {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if got := errors.Is(err, ErrTargetingUpdateRefused); got != tc.wantRefused {
				t.Errorf("refused = %v (%v)", got, err)
			}
			if got := IsOutcomeUnconfirmed(err); got != tc.wantUnconfirmed {
				t.Errorf("unconfirmed = %v (%v)", got, err)
			}
			if tc.wantRefused {
				if reason, ok := TargetingUpdateReason(err); !ok || strings.Contains(reason, "upstream text") {
					t.Errorf("reason = %q, %v", reason, ok)
				}
			}
			if n := len(s.requests()); n != 1 {
				t.Errorf("sent %d times, want once (never retried in-call)", n)
			}
		})
	}
}

func TestUpdateCampaignTargeting_RefusesBeforeSending(t *testing.T) {
	s, c := newLeverStub(t)
	bad := &TargetingUpdate{Targeting: &Targeting{Include: []map[string][]string{skills("17")}}}
	if err := c.UpdateCampaignTargeting(context.Background(), "777", bad); !errors.Is(err, ErrTargetingInvalid) {
		t.Errorf("an update without locations: %v", err)
	}
	if err := c.UpdateCampaignTargeting(context.Background(), " 777", &TargetingUpdate{}); err == nil {
		t.Error("a padded campaign id was accepted")
	}
	if err := c.UpdateCampaignTargeting(context.Background(), "777", nil); err == nil {
		t.Error("a nil update was accepted")
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("%d request(s) sent for refusals decided locally", n)
	}
}
