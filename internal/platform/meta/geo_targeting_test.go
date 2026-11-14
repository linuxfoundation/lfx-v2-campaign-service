// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// geoServer answers the account read, the geo-location metadata read (with answer, or a Graph
// error when answer is empty) and the create POSTs, recording the lookups and every POST body.
type geoServer struct {
	*httptest.Server
	mu      sync.Mutex
	lookups []string
	posts   []string
	bodies  map[string]map[string]any
}

func newGeoServer(t *testing.T, answer string) *geoServer {
	t.Helper()
	gs := &geoServer{bodies: map[string]map[string]any{}}
	gs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		gs.mu.Lock()
		defer gs.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/search":
			q := r.URL.Query()
			gs.lookups = append(gs.lookups, q.Get("type")+" regions="+q.Get("regions")+" cities="+q.Get("cities"))
			if answer == "" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `{"error":{"message":"An unknown error occurred","type":"OAuthException","code":1}}`)
				return
			}
			_, _ = io.WriteString(w, answer)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/act_777"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1,"currency":"USD"}`)
		case r.Method == http.MethodPost:
			suffix := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			gs.posts = append(gs.posts, suffix)
			gs.bodies[suffix] = decodeBody(t, r)
			ids := map[string]string{"campaigns": "120100000000123", "adsets": "120200000000456", "adcreatives": "creative_1", "ads": "ad_1"}
			_, _ = io.WriteString(w, `{"id":"`+ids[suffix]+`"}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return gs
}

func (gs *geoServer) snapshot() (lookups, posts []string, bodies map[string]map[string]any) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	return append([]string(nil), gs.lookups...), append([]string(nil), gs.posts...), gs.bodies
}

const californiaAndBerlin = `{"data":{"regions":{"3847":{"key":"3847","name":"California","type":"region","country_code":"US"}},
	"cities":{"2420605":{"key":"2420605","name":"Berlin","type":"city","country_code":"DE"}}}}`

// TestCreateCampaign_RegionsAndCitiesAreAddedToTheCountries: the keys are looked up in one read
// before the first write, then added to geo_locations beside the countries.
func TestCreateCampaign_RegionsAndCitiesAreAddedToTheCountries(t *testing.T) {
	gs := newGeoServer(t, `{"data":{"regions":{"3847":{"key":"3847","name":"California","type":"region","country_code":"US"}},
		"cities":{"2420605":{"key":"2420605","name":"Toronto","type":"city","country_code":"CA"}}}}`)
	defer gs.Close()
	in := metaFieldInput()
	in.GeoTargets = []string{"GB"}
	in.Targeting = &AudienceTargeting{Regions: []string{"3847", " 3847"}, Cities: []string{"2420605"}}
	if _, err := biddingClient(gs.URL, 100).CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	lookups, posts, bodies := gs.snapshot()
	if !reflect.DeepEqual(lookups, []string{`adgeolocationmeta regions=["3847"] cities=["2420605"]`}) {
		t.Errorf("lookups = %v, want one adgeolocationmeta read of both lists", lookups)
	}
	if len(posts) == 0 || posts[0] != "campaigns" {
		t.Fatalf("posts = %v", posts)
	}
	targeting, _ := bodies["adsets"]["targeting"].(map[string]any)
	want := map[string]any{
		"countries": []any{"GB"},
		"regions":   []any{map[string]any{"key": "3847"}},
		"cities":    []any{map[string]any{"key": "2420605"}},
	}
	if !reflect.DeepEqual(targeting["geo_locations"], want) {
		t.Errorf("geo_locations = %#v, want %#v", targeting["geo_locations"], want)
	}
	if _, has := targeting["targeting_automation"]; has {
		t.Errorf("targeting_automation = %v; a location is not an inclusion", targeting["targeting_automation"])
	}
}

// TestCreateCampaign_RegionOnlyDropsTheUSDefault: with no geoTargets, a region is the whole
// targeting (no US default to overlap it) and the campaign is named after its country.
func TestCreateCampaign_RegionOnlyDropsTheUSDefault(t *testing.T) {
	gs := newGeoServer(t, `{"data":{"regions":{"3847":{"key":"3847","type":"region","country_code":"US"}}}}`)
	defer gs.Close()
	in := metaFieldInput()
	in.GeoTargets = nil
	in.Targeting = &AudienceTargeting{Regions: []string{"3847"}}
	res, err := biddingClient(gs.URL, 100).CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	_, _, bodies := gs.snapshot()
	targeting, _ := bodies["adsets"]["targeting"].(map[string]any)
	want := map[string]any{"regions": []any{map[string]any{"key": "3847"}}}
	if !reflect.DeepEqual(targeting["geo_locations"], want) {
		t.Errorf("geo_locations = %#v, want %#v", targeting["geo_locations"], want)
	}
	if !strings.Contains(res.CampaignName, "| NA |") {
		t.Errorf("campaign name = %q, want the NA region segment from the region's country", res.CampaignName)
	}
}

// TestCreateCampaign_UnusableRegionOrCityCreatesNothing: a parent country that is sanctioned,
// regulated, already targeted, or in the EU without DSA disclosures, and a key of the wrong type, is
// a refusal; a failed read, missing data, an unanswered key or no country is an error. No POST.
func TestCreateCampaign_UnusableRegionOrCityCreatesNothing(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		geo          []string
		want         string
		refusal      bool
	}{
		{"sanctioned", `{"data":{"regions":{"3847":{"key":"3847","type":"region","country_code":"IR"}}}}`, nil, `region "3847" is in IR, which is not eligible for Meta ad targeting`, true},
		{"russia", `{"data":{"regions":{"3847":{"key":"3847","type":"region","country_code":"ru"}}}}`, nil, "is in RU, which is not eligible", true},
		{"regulated", `{"data":{"regions":{"3847":{"key":"3847","type":"region","country_code":"SG"}}}}`, nil, "is in SG, a market that requires a regional compliance declaration", true},
		{"overlaps a country", `{"data":{"regions":{"3847":{"key":"3847","type":"region","country_code":"US"}}}}`, []string{"US"}, "is in US, which geoTargets already targets as a whole country", true},
		{"EU without DSA", `{"data":{"regions":{"3847":{"key":"3847","type":"region","country_code":"DE"}}}}`, nil, "is in DE, an EU territory, so dsaBeneficiary and dsaPayor are required", true},
		{"a city key in regions", `{"data":{"regions":{"3847":{"key":"3847","type":"city","country_code":"US"}}}}`, nil, `region key "3847" is a Meta "city", not a region`, true},
		{"not answered", `{"data":{"regions":{}}}`, nil, `did not answer for region "3847"`, false},
		{"another key", `{"data":{"regions":{"3847":{"key":"9999","type":"region","country_code":"US"}}}}`, nil, `did not answer for region "3847"`, false},
		{"no country", `{"data":{"regions":{"3847":{"key":"3847","type":"region"}}}}`, nil, "named no country", false},
		{"not an ISO country", `{"data":{"regions":{"3847":{"key":"3847","type":"region","country_code":"XX"}}}}`, nil, "named no country", false},
		{"no data", `{}`, nil, "returned no data", false},
		{"read fails", "", nil, "could not be looked up, so nothing was created", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gs := newGeoServer(t, tc.answer)
			defer gs.Close()
			in := metaFieldInput()
			in.GeoTargets = tc.geo
			in.Targeting = &AudienceTargeting{Regions: []string{"3847"}}
			res, err := biddingClient(gs.URL, 100).CreateCampaign(context.Background(), in)
			var refusal *domain.PreflightRefusal
			if res != nil || err == nil || !strings.Contains(err.Error(), tc.want) || errors.As(err, &refusal) != tc.refusal {
				t.Fatalf("CreateCampaign = %+v, %v; want an error containing %q (refusal %v)", res, err, tc.want, tc.refusal)
			}
			if _, posts, _ := gs.snapshot(); len(posts) != 0 {
				t.Fatalf("posts = %v, want none", posts)
			}
		})
	}
}

// TestCreateCampaign_EUCityWithDisclosures: an EU city is accepted when both DSA disclosures are
// supplied.
func TestCreateCampaign_EUCityWithDisclosures(t *testing.T) {
	gs := newGeoServer(t, californiaAndBerlin)
	defer gs.Close()
	in := metaFieldInput()
	in.GeoTargets = nil
	in.DSABeneficiary, in.DSAPayor = "The Linux Foundation", "The Linux Foundation"
	in.Targeting = &AudienceTargeting{Cities: []string{"2420605"}}
	if _, err := biddingClient(gs.URL, 100).CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
}

// TestNormalizeAudienceTargeting_RegionsAndCities: keys are numeric, de-duplicated and capped, and
// count as neither an audience field nor zero targeting.
func TestNormalizeAudienceTargeting_RegionsAndCities(t *testing.T) {
	many := make([]string, MaxGeoKeys+1)
	for i := range many {
		many[i] = "38" + string(rune('0'+i/10)) + string(rune('0'+i%10))
	}
	for _, tc := range []struct {
		name string
		in   AudienceTargeting
		want string
	}{
		{"a region name", AudienceTargeting{Regions: []string{"California"}}, `regions entry "California" is not a Meta region key`},
		{"a city name", AudienceTargeting{Cities: []string{"Berlin"}}, `cities entry "Berlin" is not a Meta city key`},
		{"too many regions", AudienceTargeting{Regions: many}, "regions names 26 ids; at most 25 are allowed"},
		{"too many cities", AudienceTargeting{Cities: many}, "cities names 26 ids; at most 25 are allowed"},
	} {
		in := tc.in
		_, err := NormalizeAudienceTargeting(&in)
		var refusal *domain.PreflightRefusal
		if !errors.As(err, &refusal) || !errors.Is(err, domain.ErrCreateConfigInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want a refusal containing %q", tc.name, err, tc.want)
		}
	}
	got, err := NormalizeAudienceTargeting(&AudienceTargeting{Regions: []string{"3847", " 3847 "}, Cities: []string{"1"}})
	if err != nil || !reflect.DeepEqual(got.Regions, []string{"3847"}) || !reflect.DeepEqual(got.Cities, []string{"1"}) {
		t.Fatalf("normalize = %+v, %v", got, err)
	}
	for _, tg := range []*AudienceTargeting{{Regions: []string{"1"}}, {Cities: []string{"1"}}} {
		if tg.HasAudienceFields() || tg.IsZero() {
			t.Errorf("%+v: audience field %v, zero %v; want neither", tg, tg.HasAudienceFields(), tg.IsZero())
		}
	}
}
