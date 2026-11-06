// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

func eventTargeting() *Targeting {
	return &Targeting{
		Include: []map[string][]string{
			{"titles": {"urn:li:title:1234"}, "skills": {"urn:li:skill:17", "urn:li:skill:17"}},
			{"industries": {"urn:li:industry:4"}},
			{"interfaceLocales": {"urn:li:locale:en_US"}},
			{FacetDynamicSegments: {"urn:li:adSegment:16182906"}},
		},
		Exclude: map[string][]string{
			FacetAudienceMatchingSegments: {"urn:li:adSegment:11462646"},
			"seniorities":                 {"urn:li:seniority:1"},
			"employers":                   {"urn:li:organization:42"},
		},
	}
}

// TestCreateCampaign_CustomTargetingReachesTheWire pins the campaign body when linkedInConfig.targeting
// is set: the geo clause first, then one include.and clause per entry with its facets ORed under
// their adTargetingFacet URNs, duplicates dropped, exclude.or from the caller's excludes plus the
// runtime config's employer exclusions — and none of the profile skills/groups nor the hard-coded
// jobFunctions and seniorityExclusions.
func TestCreateCampaign_CustomTargetingReachesTheWire(t *testing.T) {
	var mu sync.Mutex
	var campaignBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			return
		}
		b, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			_ = json.Unmarshal(b, &campaignBody)
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	cfg := testConfig()
	// A profile name that does not exist: with targeting set it must not be read.
	c := NewClient(Credentials{AccessToken: "t"}, cfg, WithBaseURL(srv.URL), WithClock(fixedClock()))
	in := CampaignInput{
		EventName: "KubeCon", RegistrationURL: "https://events.example.org/kubecon",
		BudgetUSD: 100, StartDate: "2099-01-01", EndDate: "2099-02-01",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		TargetingProfile: "no-such-profile",
		Variants:         []CreativeVariant{{IntroText: "Join us — it's great", Headline: "KubeCon 2099"}},
		Project:          "CNCF",
		Targeting:        eventTargeting(),
	}
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	got, _ := json.Marshal(campaignBody["targetingCriteria"])
	want := `{"exclude":{"or":{` +
		`"urn:li:adTargetingFacet:audienceMatchingSegments":["urn:li:adSegment:11462646"],` +
		`"urn:li:adTargetingFacet:employers":["urn:li:organization:42","urn:li:company:1111"],` +
		`"urn:li:adTargetingFacet:seniorities":["urn:li:seniority:1"]}},` +
		`"include":{"and":[` +
		`{"or":{"urn:li:adTargetingFacet:locations":["urn:li:geo:103644278"]}},` +
		`{"or":{"urn:li:adTargetingFacet:skills":["urn:li:skill:17"],"urn:li:adTargetingFacet:titles":["urn:li:title:1234"]}},` +
		`{"or":{"urn:li:adTargetingFacet:industries":["urn:li:industry:4"]}},` +
		`{"or":{"urn:li:adTargetingFacet:interfaceLocales":["urn:li:locale:en_US"]}},` +
		`{"or":{"urn:li:adTargetingFacet:dynamicSegments":["urn:li:adSegment:16182906"]}}]}}`
	if string(got) != want {
		t.Errorf("targetingCriteria:\n got %s\nwant %s", got, want)
	}
}

// TestCreateCampaign_NoTargetingKeepsTheProfileCriteria pins that without targeting the campaign
// still carries the profile clause and the hard-coded job functions and seniority exclusions.
func TestCreateCampaign_NoTargetingKeepsTheProfileCriteria(t *testing.T) {
	c := NewClient(Credentials{AccessToken: "t"}, testConfig())
	crit, err := c.buildTargetingCriteria("cloud-native", []string{"urn:li:geo:1"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(crit)
	for _, want := range []string{"urn:li:skill:1", "urn:li:function:8", "urn:li:seniority:1"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("profile criteria lost %s: %s", want, b)
		}
	}
}

// TestCreateCampaign_InvalidTargetingRefusedBeforeAnyRequest pins that a refused targeting is a
// pre-flight refusal raised before LinkedIn is called.
func TestCreateCampaign_InvalidTargetingRefusedBeforeAnyRequest(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls++ }))
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	in := CampaignInput{
		EventName: "KubeCon", RegistrationURL: "https://events.example.org/kubecon",
		BudgetUSD: 100, StartDate: "2099-01-01", EndDate: "2099-02-01",
		GeoTargets: []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		Variants:   []CreativeVariant{{IntroText: "Join us — it's great", Headline: "KubeCon 2099"}},
		Project:    "CNCF",
		Targeting:  &Targeting{Include: []map[string][]string{{"titles": {"urn:li:title:x"}}}},
	}
	_, err := c.CreateCampaign(context.Background(), in)
	if _, pre := domain.AsPreflightRefusal(err); !errors.Is(err, ErrTargetingInvalid) || !pre {
		t.Fatalf("want a pre-flight ErrTargetingInvalid, got %v", err)
	}
	if calls != 0 {
		t.Errorf("%d LinkedIn requests before the refusal", calls)
	}
}

func TestValidateTargeting(t *testing.T) {
	inc := func(clauses ...map[string][]string) *Targeting { return &Targeting{Include: clauses} }
	many := func(prefix string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = prefix + strconv.Itoa(i+1)
		}
		return out
	}
	elevenClauses := make([]map[string][]string, MaxTargetingClauses+1)
	for i := range elevenClauses {
		elevenClauses[i] = map[string][]string{"skills": {"urn:li:skill:1"}}
	}
	cases := []struct {
		name string
		in   *Targeting
		want string // "" = accepted
	}{
		{"event shape", eventTargeting(), ""},
		{"no clauses", &Targeting{}, "at least one clause"},
		{"too many clauses", &Targeting{Include: elevenClauses}, "at most 10"},
		{"empty clause", inc(map[string][]string{}), "include[0] is empty"},
		{"empty facet", inc(map[string][]string{"skills": {}}), "is empty"},
		{"unknown facet", inc(map[string][]string{"memberBehaviors": {"urn:li:memberBehavior:2"}}), "unknown facet"},
		{"wrong namespace", inc(map[string][]string{"skills": {"urn:li:title:1"}}), "not urn:li:skill:<id>"},
		{"non-numeric id", inc(map[string][]string{"industries": {"urn:li:industry:abc"}}), "not urn:li:industry:<id>"},
		{"leading zero id", inc(map[string][]string{"industries": {"urn:li:industry:04"}}), "not urn:li:industry:<id>"},
		{"blank value", inc(map[string][]string{"skills": {" "}}), "blank value"},
		{"padded value", inc(map[string][]string{"skills": {" urn:li:skill:1"}}), "surrounding whitespace"},
		{"bad staff range", inc(map[string][]string{"staffCountRanges": {"urn:li:staffCountRange:(1,2)"}}), "staffCountRange"},
		{"good staff range", inc(map[string][]string{"staffCountRanges": {"urn:li:staffCountRange:(201,500)"}}), ""},
		{"bad locale", inc(map[string][]string{"interfaceLocales": {"urn:li:locale:EN_us"}}), "urn:li:locale"},
		{"company employer", inc(map[string][]string{"employers": {"urn:li:company:1"}}), ""},
		{"segment shape", inc(map[string][]string{FacetAudienceMatchingSegments: {"urn:li:adSegment:x"}}), "urn:li:adSegment:<id>"},
		{"over facet cap", inc(map[string][]string{"skills": many("urn:li:skill:", 101)}), "at most 100"},
		{"employers up to 200", inc(map[string][]string{"employers": many("urn:li:organization:", 150)}), ""},
		{"exclude groups", &Targeting{Include: eventTargeting().Include, Exclude: map[string][]string{"groups": {"urn:li:group:1"}}}, "only under include"},
		{"exclude locale", &Targeting{Include: eventTargeting().Include, Exclude: map[string][]string{"interfaceLocales": {"urn:li:locale:en_US"}}}, "only under include"},
		{"titles with seniorities", inc(map[string][]string{"titles": {"urn:li:title:1"}}, map[string][]string{"seniorities": {"urn:li:seniority:5"}}), "titles together"},
		{"titles with functions same clause", inc(map[string][]string{"titles": {"urn:li:title:1"}, "jobFunctions": {"urn:li:function:8"}}), "titles together"},
		{"titles with excluded seniorities", &Targeting{Include: []map[string][]string{{"titles": {"urn:li:title:1"}}}, Exclude: map[string][]string{"seniorities": {"urn:li:seniority:1"}}}, ""},
		{"employers with industries", inc(map[string][]string{"employers": {"urn:li:organization:1"}}, map[string][]string{"industries": {"urn:li:industry:4"}}), "employers together"},
		{"employers with staff ranges", inc(map[string][]string{"employers": {"urn:li:organization:1"}, "staffCountRanges": {"urn:li:staffCountRange:(1,1)"}}), "employers together"},
		{"staff ranges both ways", &Targeting{Include: []map[string][]string{{"staffCountRanges": {"urn:li:staffCountRange:(1,1)"}}}, Exclude: map[string][]string{"staffCountRanges": {"urn:li:staffCountRange:(2,10)"}}}, "not both"},
		{"value included and excluded", &Targeting{Include: []map[string][]string{{"skills": {"urn:li:skill:1"}}}, Exclude: map[string][]string{"skills": {"urn:li:skill:1"}}}, "both included and excluded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateTargeting(tc.in)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil || !errors.Is(err, ErrTargetingInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a refusal containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestValidateTargeting_DropsDuplicatesAndKeepsInput(t *testing.T) {
	in := eventTargeting()
	out, err := ValidateTargeting(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Include[0]["skills"]; len(got) != 1 {
		t.Errorf("duplicates kept: %v", got)
	}
	if got := in.Include[0]["skills"]; len(got) != 2 {
		t.Errorf("the caller's value was mutated: %v", got)
	}
}

func TestTargetingSegmentFacetsSet(t *testing.T) {
	if (*Targeting)(nil).SegmentFacetsSet() {
		t.Error("nil targeting names no segment")
	}
	plain := &Targeting{Include: []map[string][]string{{"skills": {"urn:li:skill:1"}}}, Exclude: map[string][]string{"seniorities": {"urn:li:seniority:1"}}}
	if plain.SegmentFacetsSet() {
		t.Error("facets alone name no segment")
	}
	for _, tc := range []*Targeting{
		{Include: []map[string][]string{{FacetAudienceMatchingSegments: {"urn:li:adSegment:1"}}}},
		{Include: []map[string][]string{{"skills": {"urn:li:skill:1"}}, {FacetDynamicSegments: {"urn:li:adSegment:1"}}}},
		{Include: plain.Include, Exclude: map[string][]string{FacetAudienceMatchingSegments: {"urn:li:adSegment:1"}}},
		{Include: plain.Include, Exclude: map[string][]string{FacetDynamicSegments: {"urn:li:adSegment:1"}}},
	} {
		if !tc.SegmentFacetsSet() {
			t.Errorf("segment not detected in %+v", tc)
		}
	}
}
