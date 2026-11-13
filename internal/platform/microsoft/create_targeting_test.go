// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

func TestValidateCreateTargeting(t *testing.T) {
	la := ProximityTarget{Latitude: 34.05, Longitude: -118.24, Radius: 25, Unit: "miles", Name: " LA "}
	tooMany := make([]ProximityTarget, MaxProximityTargets+1)
	for i := range tooMany {
		tooMany[i] = la
	}
	for _, tc := range []struct {
		name     string
		geo, ex  []string
		prox     []ProximityTarget
		langs    []string
		want     string // "" = accepted
		wantLang string
	}{
		{"nothing", nil, nil, nil, nil, "", ""},
		{"valid", []string{"US"}, []string{" pr", "PR"}, []ProximityTarget{la}, []string{"english", "Spanish", "ENGLISH"}, "", "English,Spanish"},
		{"all alone", nil, nil, nil, []string{"all"}, "", "All"},
		{"km bound", nil, nil, []ProximityTarget{{Latitude: -85, Longitude: 180, Radius: 800, Unit: "Kilometers"}}, nil, "", ""},
		{"overlap", []string{"us"}, []string{"US"}, nil, nil, "excludedGeoTargets: entry 1 (US) is also in geoTargets", ""},
		{"bad code", nil, []string{"XX"}, nil, nil, "excludedGeoTargets: geo target \"XX\" is not a Microsoft-supported", ""},
		{"blank code", nil, []string{" "}, nil, nil, "excludedGeoTargets: geo target must not be empty", ""},
		{"too many radii", nil, nil, tooMany, nil, "proximityTargets: at most 10", ""},
		{"latitude", nil, nil, []ProximityTarget{{Latitude: 85.1, Radius: 1, Unit: "Miles"}}, nil, "proximityTargets: entry 1 latitude must be between -85 and 85", ""},
		{"longitude", nil, nil, []ProximityTarget{{Longitude: -180.5, Radius: 1, Unit: "Miles"}}, nil, "entry 1 longitude", ""},
		{"unit", nil, nil, []ProximityTarget{{Radius: 1, Unit: "feet"}}, nil, "entry 1 unit must be Miles or Kilometers", ""},
		{"miles cap", nil, nil, []ProximityTarget{{Radius: 501, Unit: "Miles"}}, nil, "entry 1 radius must be a whole number from 1 to 500 miles", ""},
		{"fraction", nil, nil, []ProximityTarget{{Radius: 2.5, Unit: "Kilometers"}}, nil, "radius must be a whole number from 1 to 800 kilometers", ""},
		{"zero radius", nil, nil, []ProximityTarget{{Radius: 0, Unit: "Miles"}}, nil, "radius must be a whole number", ""},
		{"long name", nil, nil, []ProximityTarget{{Radius: 1, Unit: "Miles", Name: strings.Repeat("n", 51)}}, nil, "name must be at most 50", ""},
		{"control name", nil, nil, []ProximityTarget{{Radius: 1, Unit: "Miles", Name: "a\tb"}}, nil, "name must not contain a control character", ""},
		{"unknown language", nil, nil, nil, []string{"Klingon"}, "languages: entry 1 is not a Microsoft Search campaign language", ""},
		{"simplified chinese is not offered", nil, nil, nil, []string{"SimplifiedChinese"}, "languages: entry 1 is not", ""},
		{"all mixed", nil, nil, nil, []string{"English", "All"}, "languages: All targets every language", ""},
		{"too many languages", nil, nil, nil, strings.Split("English,Spanish,French,German,Italian,Dutch,Danish,Finnish,Swedish,Polish,Greek", ","), "languages: at most 10", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateCreateTargeting(tc.geo, tc.ex, tc.prox, tc.langs)
			if tc.want != "" {
				var tErr *CreateTargetingError
				if !errors.As(err, &tErr) || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v, want a CreateTargetingError containing %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if strings.Join(got.Languages, ",") != tc.wantLang {
				t.Errorf("languages = %v, want %s", got.Languages, tc.wantLang)
			}
			if tc.name == "valid" && (strings.Join(got.ExcludedGeoTargets, ",") != "PR" || got.ProximityTargets[0].Unit != "Miles" || got.ProximityTargets[0].Name != "LA") {
				t.Errorf("normalized = %+v", got)
			}
		})
	}
}

// targetingAPI is a fake Microsoft API for the full create, recording every request body by
// path in order. criterionStatus/criterionBody override the n-th (1-based) CampaignCriterions
// answer; campaignQueryBody scripts the campaign name lookup.
type targetingAPI struct {
	mu                sync.Mutex
	calls             []string
	bodies            []string
	criterionN        int
	failCriterion     int
	criterionStatus   int
	criterionBody     string
	campaignQueryBody string
}

func (a *targetingAPI) snapshot() (calls, bodies []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...), append([]string(nil), a.bodies...)
}

func (a *targetingAPI) bodyOf(path string, nth int) string {
	calls, bodies := a.snapshot()
	n := 0
	for i, c := range calls {
		if c == path {
			n++
			if n == nth {
				return bodies[i]
			}
		}
	}
	return ""
}

func newTargetingClient(t *testing.T, a *targetingAPI) *Client {
	t.Helper()
	fileSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, geoFileFixture(geoRowUS, geoRowGB, geoRowJP, "160,Puerto Rico,Country,,Active,2630"))
	}))
	t.Cleanup(fileSrv.Close)
	return newAPIClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		p := r.URL.Path[strings.Index(r.URL.Path, "/v13/")+len("/v13/"):]
		a.mu.Lock()
		a.calls = append(a.calls, p)
		a.bodies = append(a.bodies, string(b))
		a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch p {
		case "GeoLocationsFileUrl/Query":
			_, _ = fmt.Fprintf(w, `{"FileUrl":%q,"FileUrlExpiryTimeUtc":"2026-08-18T12:15:00Z","LastModifiedTimeUtc":"2026-06-05T18:43:00Z"}`, fileSrv.URL+"/geo.csv")
		case "Campaigns/QueryByAccountId":
			writeOr(w, a.campaignQueryBody, `{"Campaigns":[]}`)
		case "CampaignCriterions/QueryByIds":
			_, _ = io.WriteString(w, `{"CampaignCriterions":[],"PartialErrors":[]}`)
		case "AdGroups/QueryByCampaignId":
			_, _ = io.WriteString(w, `{"AdGroups":[]}`)
		case "Ads/QueryByAdGroupId":
			_, _ = io.WriteString(w, `{"Ads":[]}`)
		case "Campaigns":
			_, _ = io.WriteString(w, `{"CampaignIds":[321],"PartialErrors":[]}`)
		case "CampaignCriterions":
			a.mu.Lock()
			a.criterionN++
			n := a.criterionN
			a.mu.Unlock()
			if n == a.failCriterion {
				w.WriteHeader(a.criterionStatus)
				_, _ = io.WriteString(w, a.criterionBody)
				return
			}
			_, _ = fmt.Fprintf(w, `{"CampaignCriterionIds":[%d],"NestedPartialErrors":[]}`, 9000+n)
		case "AdGroups":
			_, _ = io.WriteString(w, `{"AdGroupIds":[654],"PartialErrors":[]}`)
		case "Ads":
			_, _ = io.WriteString(w, `{"AdIds":[987],"PartialErrors":[]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, p)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func targetingCreateInput() CampaignInput {
	in := validInput()
	in.GeoTargets = []string{"US"}
	in.ExcludedGeoTargets = []string{"pr"}
	in.ProximityTargets = []ProximityTarget{{Latitude: 34.05, Longitude: -118.24, Radius: 25, Unit: "miles", Name: "LA"}}
	in.Languages = []string{"japanese", "English"}
	return in
}

// The exclusions and the radius follow the positive geo, each in its own Targets call, before the
// ad group; the campaign carries the languages and the ad group no Language.
func TestCreateCampaign_ExcludedRadiusAndLanguages(t *testing.T) {
	a := &targetingAPI{}
	c := newTargetingClient(t, a)
	res, err := c.CreateCampaign(context.Background(), targetingCreateInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	calls, _ := a.snapshot()
	var muts []string
	for _, p := range calls {
		if p == "Campaigns" || p == "CampaignCriterions" || p == "AdGroups" || p == "Ads" {
			muts = append(muts, p)
		}
	}
	if got := strings.Join(muts, ","); got != "Campaigns,CampaignCriterions,CampaignCriterions,CampaignCriterions,AdGroups,Ads" {
		t.Fatalf("mutations = %s", got)
	}
	if b := a.bodyOf("Campaigns", 1); !strings.Contains(b, `"Languages":["Japanese","English"]`) {
		t.Errorf("campaign body = %s", b)
	}
	if b := a.bodyOf("CampaignCriterions", 2); b != `{"CampaignCriterions":[{"CampaignId":321,"Criterion":{"Type":"LocationCriterion","LocationId":160},"Type":"NegativeCampaignCriterion"}],"CriterionType":"Targets"}` {
		t.Errorf("exclusion body = %s", b)
	}
	if b := a.bodyOf("CampaignCriterions", 3); b != `{"CampaignCriterions":[{"CampaignId":321,"Criterion":{"Type":"RadiusCriterion","LatitudeDegrees":34.05,"LongitudeDegrees":-118.24,"Name":"LA","Radius":25,"RadiusUnit":"Miles"},"Type":"BiddableCampaignCriterion"}],"CriterionType":"Targets"}` {
		t.Errorf("radius body = %s", b)
	}
	if b := a.bodyOf("AdGroups", 1); strings.Contains(b, `"Language"`) {
		t.Errorf("ad group body carries a Language under a campaign with languages: %s", b)
	}
	if strings.Join(res.ExcludedGeoCriterionIDs, ",") != "9002" || strings.Join(res.RadiusCriterionIDs, ",") != "9003" {
		t.Errorf("criterion ids = %v %v", res.ExcludedGeoCriterionIDs, res.RadiusCriterionIDs)
	}
	steps := strings.Join(res.Steps, "\n")
	for _, s := range []string{"Geo exclusions attached: 1 excluded location criteria (PR)", "Radius targeting attached: 1 radius criteria (25 Miles around LA)", "Campaign languages set: Japanese, English"} {
		if !strings.Contains(steps, s) {
			t.Errorf("steps lack %q:\n%s", s, steps)
		}
	}
}

// Without any of the three the create is byte-for-byte what it was: no campaign Languages, the ad
// group English, one criterion call.
func TestCreateCampaign_NoCreateTargetingKeepsTheOldShape(t *testing.T) {
	a := &targetingAPI{}
	c := newTargetingClient(t, a)
	in := validInput()
	in.GeoTargets = []string{"US"}
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if b := a.bodyOf("Campaigns", 1); strings.Contains(b, "Languages") {
		t.Errorf("campaign body = %s", b)
	}
	if b := a.bodyOf("AdGroups", 1); !strings.Contains(b, `"Language":"English"`) {
		t.Errorf("ad group body = %s", b)
	}
	if a.bodyOf("CampaignCriterions", 2) != "" {
		t.Error("a second criterion call was made")
	}
}

// A refused value, or an excluded code the locations file cannot resolve, creates nothing.
func TestCreateCampaign_CreateTargetingRefusalsCreateNothing(t *testing.T) {
	for name, mutate := range map[string]func(*CampaignInput){
		"overlap":    func(in *CampaignInput) { in.ExcludedGeoTargets = []string{"US"} },
		"bad radius": func(in *CampaignInput) { in.ProximityTargets[0].Radius = 900 },
		"unresolved": func(in *CampaignInput) { in.ExcludedGeoTargets = []string{"DE"} },
	} {
		t.Run(name, func(t *testing.T) {
			a := &targetingAPI{}
			c := newTargetingClient(t, a)
			in := targetingCreateInput()
			mutate(&in)
			res, err := c.CreateCampaign(context.Background(), in)
			if res != nil || err == nil {
				t.Fatalf("CreateCampaign = (%v, %v), want (nil, err)", res, err)
			}
			if name != "unresolved" {
				var r *domain.PreflightRefusal
				if !errors.As(err, &r) {
					t.Errorf("err = %v, want a refusal", err)
				}
			}
			calls, _ := a.snapshot()
			for _, p := range calls {
				if p == "Campaigns" || p == "CampaignCriterions" {
					t.Fatalf("a mutation was sent: %v", calls)
				}
			}
		})
	}
}

// A failed exclusion or radius attach stops the create before the ad group, with the campaign as
// a partial and a classified error.
func TestCreateCampaign_CriterionFailureStopsBeforeTheAdGroup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		nth    int
		status int
		body   string
		want   string
	}{
		{"exclusion rejected", 2, http.StatusOK, `{"CampaignCriterionIds":[null],"NestedPartialErrors":[{"Code":1032,"ErrorCode":"X"}]}`, "geo exclusions rejected (campaign 321 exists but would serve in the excluded countries if enabled"},
		{"exclusion unconfirmed", 2, http.StatusInternalServerError, `{}`, "geo exclusions UNCONFIRMED"},
		{"radius rejected", 3, http.StatusBadRequest, `{"Code":1001,"ErrorCode":"InvalidRequest"}`, "radius targeting rejected"},
		{"radius short ids", 3, http.StatusOK, `{"CampaignCriterionIds":[],"NestedPartialErrors":[]}`, "radius targeting UNCONFIRMED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &targetingAPI{failCriterion: tc.nth, criterionStatus: tc.status, criterionBody: tc.body}
			c := newTargetingClient(t, a)
			res, err := c.CreateCampaign(context.Background(), targetingCreateInput())
			if res == nil || res.CampaignID != "321" || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateCampaign = (%+v, %v), want a partial and an error containing %q", res, err, tc.want)
			}
			calls, _ := a.snapshot()
			for _, p := range calls {
				if p == "AdGroups" {
					t.Fatalf("the ad group was created after a failed criterion step: %v", calls)
				}
			}
		})
	}
}

// A campaign found by name gets none of the three: the create stops before the ad group, with a
// step naming the fields.
func TestCreateCampaign_ReusedCampaignStopsBeforeTheAdGroup(t *testing.T) {
	in := targetingCreateInput()
	name := composeName(in)
	a := &targetingAPI{campaignQueryBody: `{"Campaigns":[{"Id":321,"Name":` + jsonString(name) + `}]}`}
	c := newTargetingClient(t, a)
	res, err := c.CreateCampaign(context.Background(), in)
	if res == nil || err == nil || !strings.Contains(err.Error(), "create-time targeting NOT applied to reused campaign 321 (excludedGeoTargets, proximityTargets, languages") {
		t.Fatalf("CreateCampaign = (%+v, %v)", res, err)
	}
	calls, _ := a.snapshot()
	for _, p := range calls {
		if p == "Campaigns" || p == "CampaignCriterions" || p == "AdGroups" {
			t.Fatalf("a mutation was sent to a reused campaign: %v", calls)
		}
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "Stopped before the ad group: campaign 321 was found by name") {
		t.Errorf("steps = %v", res.Steps)
	}
}

// The step label reduces a link in a radius name (it is persisted), and falls back to the
// coordinates when nothing is left or no name was given.
func TestProximityLabel_RedactsTheName(t *testing.T) {
	got := proximityLabel([]ProximityTarget{
		{Latitude: 1, Longitude: 2, Radius: 5, Unit: "Miles", Name: "Venue https://x.test/?token=SECRET"},
		{Latitude: 3.5, Longitude: -4, Radius: 10, Unit: "Kilometers"},
	})
	if strings.Contains(got, "SECRET") || !strings.Contains(got, "5 Miles around Venue") || !strings.Contains(got, "10 Kilometers around 3.5,-4") {
		t.Fatalf("label = %q", got)
	}
}
