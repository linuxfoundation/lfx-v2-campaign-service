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
	"sync/atomic"
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
	mu.Lock()
	got, _ := json.Marshal(campaignBody["targetingCriteria"])
	mu.Unlock()
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

// TestCreateCampaign_ReusedCampaignWithTargetingStopsBeforeCreatives pins the #557 review guard:
// when linkedInConfig.targeting is set and the campaign is found by name, its existing criteria are
// not the requested ones as far as this run can tell, so the create stops before any conversion,
// dark post or creative, leaves the campaign untouched, and returns it as a partial alongside an
// error for reconciliation. The same lookup without targeting (a profile create) still reuses the
// campaign and attaches the creatives.
func TestCreateCampaign_ReusedCampaignWithTargetingStopsBeforeCreatives(t *testing.T) {
	newInput := func(targeting *Targeting) CampaignInput {
		return CampaignInput{
			EventName: "KubeCon", RegistrationURL: "https://events.example.org/kubecon",
			BudgetUSD: 100, StartDate: "2099-01-01", EndDate: "2099-02-01",
			GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
			TargetingProfile: "cloud-native",
			Variants:         []CreativeVariant{{IntroText: "Join us — it's great", Headline: "KubeCon 2099"}},
			Project:          "CNCF",
			Conversions:      []string{"104012"},
			Targeting:        targeting,
		}
	}
	for _, tc := range []struct {
		name      string
		targeting *Targeting
		wantStop  bool
	}{
		{"caller targeting", eventTargeting(), true},
		{"profile", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := newInput(tc.targeting)
			plan, err := campaignBidding(0)
			if err != nil {
				t.Fatal(err)
			}
			name := buildCampaignName("KubeCon", plan.nameObjective, "CNCF", in)
			var mu sync.Mutex
			var writes []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					if strings.Contains(r.URL.Path, "adCampaigns") && !strings.Contains(r.URL.Path, "adCampaignGroups") {
						_, _ = io.WriteString(w, `{"elements":[{"id":777,"name":"`+name+`","status":"ACTIVE","campaignGroup":"urn:li:sponsoredCampaignGroup:100","objectiveType":"`+plan.objectiveType+`"}],"metadata":{}}`)
						return
					}
					_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
					return
				}
				if !strings.Contains(r.URL.Path, "adCampaignGroups") {
					mu.Lock()
					writes = append(writes, r.Method+" "+r.URL.Path)
					mu.Unlock()
				}
				switch {
				case strings.Contains(r.URL.Path, "adCampaignGroups"):
					_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
				case strings.Contains(r.URL.Path, "posts"):
					_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
				case strings.Contains(r.URL.Path, "creatives"):
					_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
				default:
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer srv.Close()
			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
			res, err := c.CreateCampaign(context.Background(), in)
			mu.Lock()
			got := strings.Join(writes, "\n")
			mu.Unlock()
			if !tc.wantStop {
				if err != nil {
					t.Fatalf("profile reuse: %v", err)
				}
				if res.CampaignID != "777" || res.CreativeCount != 1 {
					t.Fatalf("profile reuse: campaign %q, %d creatives; want 777 with its creative", res.CampaignID, res.CreativeCount)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "targeting NOT applied to reused campaign 777") {
				t.Fatalf("want a reconciliation error for the reused campaign, got %v", err)
			}
			if res == nil || res.CampaignID != "777" || res.CampaignGroupID != "100" || res.CampaignName != name || res.CreativeCount != 0 {
				t.Fatalf("partial = %+v; want campaign 777 in group 100 with no creative", res)
			}
			if !strings.Contains(strings.Join(res.Steps, "\n"), "Stopped before the creatives: campaign 777") {
				t.Errorf("steps %v lack the stop step", res.Steps)
			}
			if got != "" {
				t.Errorf("writes below the campaign group on a reused targeted campaign:\n%s", got)
			}
		})
	}
}

// TestBuildTargetingCriteria_ProfileKeepsFixedFacets pins that without targeting the campaign
// still carries the profile clause and the hard-coded job functions and seniority exclusions.
func TestBuildTargetingCriteria_ProfileKeepsFixedFacets(t *testing.T) {
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
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1) }))
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
	if n := calls.Load(); n != 0 {
		t.Errorf("%d LinkedIn requests before the refusal", n)
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
		excl []string
	}{
		{"event shape", eventTargeting(), "", nil},
		{"no clauses", &Targeting{}, "at least one clause", nil},
		{"too many clauses", &Targeting{Include: elevenClauses}, "at most 10", nil},
		{"empty clause", inc(map[string][]string{}), "include[0] is empty", nil},
		{"empty facet", inc(map[string][]string{"skills": {}}), "is empty", nil},
		{"unknown facet", inc(map[string][]string{"memberBehaviors": {"urn:li:memberBehavior:2"}}), "unknown facet", nil},
		{"wrong namespace", inc(map[string][]string{"skills": {"urn:li:title:1"}}), "not urn:li:skill:<id>", nil},
		{"non-numeric id", inc(map[string][]string{"industries": {"urn:li:industry:abc"}}), "not urn:li:industry:<id>", nil},
		{"leading zero id", inc(map[string][]string{"industries": {"urn:li:industry:04"}}), "not urn:li:industry:<id>", nil},
		{"blank value", inc(map[string][]string{"skills": {" "}}), "blank value", nil},
		{"padded value", inc(map[string][]string{"skills": {" urn:li:skill:1"}}), "surrounding whitespace", nil},
		{"bad staff range", inc(map[string][]string{"staffCountRanges": {"urn:li:staffCountRange:(1,2)"}}), "staffCountRange", nil},
		{"good staff range", inc(map[string][]string{"staffCountRanges": {"urn:li:staffCountRange:(201,500)"}}), "", nil},
		{"bad locale", inc(map[string][]string{"interfaceLocales": {"urn:li:locale:EN_us"}}), "urn:li:locale", nil},
		{"company employer", inc(map[string][]string{"employers": {"urn:li:company:1"}}), "", nil},
		{"member interests", inc(map[string][]string{"interests": {"urn:li:interest:689290"}}), "", nil},
		{"excluded interests", &Targeting{Include: eventTargeting().Include, Exclude: map[string][]string{"interests": {"urn:li:interest:689290"}}}, "", nil},
		{"interest in the wrong namespace", inc(map[string][]string{"interests": {"urn:li:skill:1"}}), "not urn:li:interest:<id>", nil},
		{"segment shape", inc(map[string][]string{FacetAudienceMatchingSegments: {"urn:li:adSegment:x"}}), "urn:li:adSegment:<id>", nil},
		{"over facet cap", inc(map[string][]string{"skills": many("urn:li:skill:", 101)}), "at most 100", nil},
		{"employers up to 200", inc(map[string][]string{"employers": many("urn:li:organization:", 150)}), "", nil},
		{"exclude groups", &Targeting{Include: eventTargeting().Include, Exclude: map[string][]string{"groups": {"urn:li:group:1"}}}, "only under include", nil},
		{"exclude locale", &Targeting{Include: eventTargeting().Include, Exclude: map[string][]string{"interfaceLocales": {"urn:li:locale:en_US"}}}, "only under include", nil},
		{"titles with seniorities", inc(map[string][]string{"titles": {"urn:li:title:1"}}, map[string][]string{"seniorities": {"urn:li:seniority:5"}}), "titles together", nil},
		{"titles with functions same clause", inc(map[string][]string{"titles": {"urn:li:title:1"}, "jobFunctions": {"urn:li:function:8"}}), "titles together", nil},
		{"titles with excluded seniorities", &Targeting{Include: []map[string][]string{{"titles": {"urn:li:title:1"}}}, Exclude: map[string][]string{"seniorities": {"urn:li:seniority:1"}}}, "", nil},
		{"employers with industries", inc(map[string][]string{"employers": {"urn:li:organization:1"}}, map[string][]string{"industries": {"urn:li:industry:4"}}), "employers together", nil},
		{"employers with staff ranges", inc(map[string][]string{"employers": {"urn:li:organization:1"}, "staffCountRanges": {"urn:li:staffCountRange:(1,1)"}}), "employers together", nil},
		{"staff ranges both ways", &Targeting{Include: []map[string][]string{{"staffCountRanges": {"urn:li:staffCountRange:(1,1)"}}}, Exclude: map[string][]string{"staffCountRanges": {"urn:li:staffCountRange:(2,10)"}}}, "not both", nil},
		{"included employer in employerExclusions", inc(map[string][]string{"employers": {"urn:li:organization:1111"}}), "is in employerExclusions", []string{"urn:li:company:1111"}},
		{"excluded employer also in employerExclusions", &Targeting{Include: []map[string][]string{{"skills": {"urn:li:skill:1"}}}, Exclude: map[string][]string{"employers": {"urn:li:company:1111"}}}, "", []string{"urn:li:company:1111"}},
		{"merged employers over the cap", &Targeting{Include: []map[string][]string{{"skills": {"urn:li:skill:1"}}}, Exclude: map[string][]string{"employers": many("urn:li:organization:", 200)}}, "at most 200", []string{"urn:li:company:999999"}},
		{"merged employers at the cap by id", &Targeting{Include: []map[string][]string{{"skills": {"urn:li:skill:1"}}}, Exclude: map[string][]string{"employers": many("urn:li:organization:", 200)}}, "", []string{"urn:li:company:7"}},
		{"malformed employerExclusions", inc(map[string][]string{"skills": {"urn:li:skill:1"}}), "employerExclusions", []string{"urn:li:skill:1"}},
		{"value included and excluded", &Targeting{Include: []map[string][]string{{"skills": {"urn:li:skill:1"}}}, Exclude: map[string][]string{"skills": {"urn:li:skill:1"}}}, "both included and excluded", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateTargeting(tc.in, tc.excl)
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
	out, err := ValidateTargeting(in, nil)
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

// TestValidateTargeting_EmployerAliasesCountOnce pins that urn:li:company:<n> and
// urn:li:organization:<n> are counted as ONE employer against the cap, as mergeEmployersCapped and
// the wire builder count them, and that the first URN of each id is the one kept (#557 review).
func TestValidateTargeting_EmployerAliasesCountOnce(t *testing.T) {
	// aliases lists both forms for ids 1..n; odd ids lead with company, even ids with organization.
	aliases := func(n int) []string {
		out := make([]string, 0, 2*n)
		for i := 1; i <= n; i++ {
			org, co := "urn:li:organization:"+strconv.Itoa(i), "urn:li:company:"+strconv.Itoa(i)
			if i%2 == 1 {
				out = append(out, co, org)
			} else {
				out = append(out, org, co)
			}
		}
		return out
	}
	firsts := func(n int) []string {
		out := make([]string, 0, n)
		for i, v := range aliases(n) {
			if i%2 == 0 {
				out = append(out, v)
			}
		}
		return out
	}
	skills := []map[string][]string{{"skills": {"urn:li:skill:1"}}}
	cases := []struct {
		name    string
		in      *Targeting
		include bool // the employers list under test sits in include[0], not exclude
		n       int  // distinct employer ids supplied
		want    string
	}{
		{"exclude, both forms, 101 ids", &Targeting{Include: skills, Exclude: map[string][]string{"employers": aliases(101)}}, false, 101, ""},
		{"exclude, both forms, at the cap", &Targeting{Include: skills, Exclude: map[string][]string{"employers": aliases(maxEmployerFacetValues)}}, false, maxEmployerFacetValues, ""},
		{"exclude, both forms, one id over the cap", &Targeting{Include: skills, Exclude: map[string][]string{"employers": aliases(maxEmployerFacetValues + 1)}}, false, maxEmployerFacetValues + 1, "more than 200 distinct values"},
		{"include, both forms, at the cap", &Targeting{Include: []map[string][]string{{"employers": aliases(maxEmployerFacetValues)}}}, true, maxEmployerFacetValues, ""},
		{"include, both forms, one id over the cap", &Targeting{Include: []map[string][]string{{"employers": aliases(maxEmployerFacetValues + 1)}}}, true, maxEmployerFacetValues + 1, "more than 200 distinct values"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := ValidateTargeting(tc.in, nil)
			if tc.want != "" {
				if err == nil || !errors.Is(err, ErrTargetingInvalid) || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("want a refusal containing %q, got %v", tc.want, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			got := out.Exclude["employers"]
			if tc.include {
				got = out.Include[0]["employers"]
			}
			if want := firsts(tc.n); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("employers kept %d values, want the first URN of each of %d ids:\n got %v\nwant %v", len(got), tc.n, got, want)
			}
		})
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
