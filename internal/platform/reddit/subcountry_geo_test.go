// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Ids are trimmed and uppercased, duplicates dropped (first kept), and each carries its country
// and its position in the caller's list.
func TestNormalizeSubCountryGeoTargets_Normalizes(t *testing.T) {
	// Countries other than the ids' own, targeted or excluded, are no overlap.
	got, err := NormalizeSubCountryGeoTargets([]string{"ca"}, []string{"ru"}, []string{" us-ca "})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if want := []SubCountryGeo{{ID: "US-CA", Country: "US", Entry: 1}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	got, err = NormalizeSubCountryGeoTargets(nil, nil, []string{" us-ca ", "gb:2641599", "662", "US-CA", "JP-13"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := []SubCountryGeo{
		{ID: "US-CA", Country: "US", Entry: 1},
		{ID: "GB:2641599", Country: "GB", Entry: 2},
		{ID: "662", Country: "US", Entry: 3},
		{ID: "JP-13", Country: "JP", Entry: 5},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	if got, err := NormalizeSubCountryGeoTargets([]string{"US"}, nil, nil); got != nil || err != nil {
		t.Fatalf("an empty list = %v, %v; want nil, nil", got, err)
	}
}

// Every rule breach is an *ExclusionError naming subCountryGeoTargets and the entry.
func TestNormalizeSubCountryGeoTargets_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name             string
		geo, excl, input []string
		want             string
	}{
		{"blank", nil, nil, []string{"US-CA", "  "}, "entry 2 is blank"},
		{"bare country", nil, nil, []string{"us"}, `entry 1 ("US") is a whole country; put it in geoTargets`},
		{"postal code", nil, nil, []string{"US-90210"}, `entry 1 ("US-90210") is not a Reddit region`},
		{"three-letter country", nil, nil, []string{"USA-CA"}, "is not a Reddit region"},
		{"unassigned country", nil, nil, []string{"XX-CA"}, `("XX-CA") is not a Reddit region`},
		{"unassigned city country", nil, nil, []string{"XX:123"}, "is not a Reddit region"},
		{"city id with a leading zero", nil, nil, []string{"CA:0123"}, "is not a Reddit region"},
		{"four-digit DMA", nil, nil, []string{"6620"}, "is not a Reddit region"},
		{"free text", nil, nil, []string{"California"}, "is not a Reddit region"},
		{"inside a targeted country", []string{" us "}, nil, []string{"CA:6167865", "us-ca"}, `entry 2 ("US-CA") is inside US, which geoTargets already targets as a whole country`},
		{"DMA inside a targeted US", []string{"US"}, nil, []string{"662"}, `entry 1 ("662") is inside US`},
		{"inside an excluded country", nil, []string{"gb"}, []string{"GB:2641599"}, "is inside GB, which excludedGeoTargets excludes"},
		{"over the cap", nil, nil, make([]string, MaxSubCountryGeoTargets+1), "at most 50 geolocation ids, got 51"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NormalizeSubCountryGeoTargets(tc.geo, tc.excl, tc.input)
			var exErr *ExclusionError
			if !errors.As(err, &exErr) || exErr.Field != FieldSubCountryGeoTargets || !strings.Contains(exErr.Msg, tc.want) {
				t.Fatalf("err = %v, want an ExclusionError on %s containing %q", err, FieldSubCountryGeoTargets, tc.want)
			}
		})
	}
}

// The ids follow the countries in targeting.geolocations, on the ad group and in the steps.
func TestCreateCampaign_SendsSubCountryGeolocations(t *testing.T) {
	c, bodies, cleanup := newBodyCaptureServers(t)
	defer cleanup()

	in := validRedditInput()
	in.GeoTargets = []string{"ca"}
	in.SubCountryGeoTargets = []string{"us-ca", "662"}
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	campaign, adGroup := bodies()
	targeting, _ := adGroup["targeting"].(map[string]any)
	if got, want := targeting["geolocations"], []any{"CA", "US-CA", "662"}; !reflect.DeepEqual(got, want) {
		t.Errorf("geolocations = %#v, want %#v", got, want)
	}
	if name, _ := campaign["name"].(string); !strings.Contains(name, " | NA | ") {
		t.Errorf("campaign name %q: the region must come from the first country", name)
	}
	if name, _ := adGroup["name"].(string); !strings.Contains(name, "| CA+US-CA+662 |") {
		t.Errorf("ad group name %q must list every location", name)
	}
	if joined := strings.Join(res.Steps, " | "); !strings.Contains(joined, "geo: CA, US-CA, 662") || !strings.Contains(joined, "3 geos") {
		t.Errorf("steps must name every location; steps: %s", joined)
	}
}

// With no countries the US default is dropped: only the sub-country ids are targeted, the campaign
// is named after the first one's country, and excluding the US is no longer a contradiction.
func TestCreateCampaign_SubCountryOnlyDropsTheUSDefault(t *testing.T) {
	c, bodies, cleanup := newBodyCaptureServers(t)
	defer cleanup()

	in := validRedditInput()
	in.GeoTargets = nil
	in.SubCountryGeoTargets = []string{"GB:2641599"}
	in.ExcludedGeoTargets = []string{"US"}
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	campaign, adGroup := bodies()
	targeting, _ := adGroup["targeting"].(map[string]any)
	if got, want := targeting["geolocations"], []any{"GB:2641599"}; !reflect.DeepEqual(got, want) {
		t.Errorf("geolocations = %#v, want %#v", got, want)
	}
	if got, want := targeting["excluded_geolocations"], []any{"US"}; !reflect.DeepEqual(got, want) {
		t.Errorf("excluded_geolocations = %#v, want %#v", got, want)
	}
	if name, _ := campaign["name"].(string); !strings.Contains(name, " | EMEA | ") {
		t.Errorf("campaign name %q: the region must come from the sub-country id's country", name)
	}
}

// Without the field the US default and the exclusion rule against it are unchanged.
func TestNormalizeTargetingExclusions_USDefaultStillAppliesWithoutSubCountry(t *testing.T) {
	if _, err := NormalizeTargetingExclusions(nil, nil, nil, nil, []string{"US"}); err == nil {
		t.Fatal("excluding the US default with no sub-country ids must still be refused")
	}
	if _, err := NormalizeTargetingExclusions(nil, nil, []string{"GB:2641599"}, nil, []string{"US"}); err != nil {
		t.Fatalf("with sub-country ids there is no US default to contradict: %v", err)
	}
}

// An overlap is refused before any request: nothing is created.
func TestCreateCampaign_SubCountryOverlapRefusedBeforeAnyRequest(t *testing.T) {
	var calls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewClient(testCreds, testAccount, WithBaseURL(srv.URL+"/api/v3"), WithTokenURL(srv.URL), WithNowFunc(fixedRedditClock()))

	in := validRedditInput()
	in.GeoTargets = []string{"US"}
	in.SubCountryGeoTargets = []string{"US-CA"}
	res, err := c.CreateCampaign(context.Background(), in)
	if res != nil || !isTargetingRefusal(err) || !strings.Contains(err.Error(), "already targets as a whole country") {
		t.Fatalf("CreateCampaign = %v, %v; want a pre-send refusal", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 0 {
		t.Fatalf("%d requests were made", calls)
	}
}

// geoValidationServer answers POST /targeting/geolocations_validations with status and body and
// records the request bodies.
func geoValidationServer(t *testing.T, status int, body string) (*Client, func() []map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var seen []map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v3/targeting/geolocations_validations" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		mu.Lock()
		seen = append(seen, m)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(api.Close)
	tok := httptest.NewServer(tokenHandlerReturning("tok"))
	t.Cleanup(tok.Close)
	c := NewClient(testCreds, AccountConfig{AccountID: "t2_acct"}, WithBaseURL(api.URL+"/api/v3"), WithTokenURL(tok.URL),
		WithNowFunc(fixedRedditClock()), withRetryBaseDelay(tinyBackoff))
	return c, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]any(nil), seen...)
	}
}

func subCountryEntries(t *testing.T, ids ...string) []SubCountryGeo {
	t.Helper()
	entries, err := NormalizeSubCountryGeoTargets(nil, nil, ids)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return entries
}

// One validations call with every id; an answer confirming each id in its own country passes.
func TestValidateSubCountryGeoTargets_Confirms(t *testing.T) {
	c, seen := geoValidationServer(t, 200, `{"data":[{"geolocation":{"id":"US-CA","country":"US","region":"CA"}},{"geolocation":{"id":"ca:6167865","country":"CA","city":"Toronto"}},{"geolocation":{"id":"662"}}]}`)
	if err := c.ValidateSubCountryGeoTargets(context.Background(), subCountryEntries(t, "US-CA", "CA:6167865", "662")); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	bodies := seen()
	if len(bodies) != 1 {
		t.Fatalf("calls = %d, want 1", len(bodies))
	}
	want := map[string]any{"data": map[string]any{"geolocation_ids": []any{"US-CA", "CA:6167865", "662"}}}
	if !reflect.DeepEqual(bodies[0], want) {
		t.Errorf("body = %#v, want %#v", bodies[0], want)
	}
}

// An id Reddit answers with an error_message is a refusal naming the caller's entry; Reddit's own
// text is not quoted.
func TestValidateSubCountryGeoTargets_RefusesAnUntargetableID(t *testing.T) {
	c, _ := geoValidationServer(t, 200, `{"data":[{"geolocation":{"id":"US-CA","country":"US"}},{"geolocation":{"id":"US-ZZ"},"error_message":"<script>not targetable</script>"}]}`)
	entries, err := NormalizeSubCountryGeoTargets(nil, nil, []string{"US-CA", "US-CA", "us-zz"})
	if err != nil {
		t.Fatal(err)
	}
	err = c.ValidateSubCountryGeoTargets(context.Background(), entries)
	if !isTargetingRefusal(err) || !strings.Contains(err.Error(), `invalid redditConfig.subCountryGeoTargets: entry 3 ("US-ZZ") is not a targetable Reddit geolocation`) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "script") {
		t.Errorf("Reddit's text leaked into the refusal: %v", err)
	}
}

// Anything that leaves an id unconfirmed without Reddit naming it is a plain error, never a
// refusal of the caller's value.
func TestValidateSubCountryGeoTargets_UnconfirmedIsAnError(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"server error":              {500, `{}`},
		"no list":                   {200, `{"data":null}`},
		"not a list":                {200, `{"data":{"id":"US-CA"}}`},
		"duplicated key":            {200, `{"data":[{"geolocation":{"id":"US-NY","id":"US-CA","country":"US"}}]}`},
		"row with no id":            {200, `{"data":[{"geolocation":{"id":"US-CA","country":"US"}},{"geolocation":{"country":"US"}}]}`},
		"id not mentioned":          {200, `{"data":[{"geolocation":{"id":"US-NY","country":"US"}}]}`},
		"unattributed error":        {200, `{"data":[{"error_message":"invalid geolocation"}]}`},
		"different country than id": {200, `{"data":[{"geolocation":{"id":"US-CA","country":"CA"}}]}`},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := geoValidationServer(t, tc.status, tc.body)
			err := c.ValidateSubCountryGeoTargets(context.Background(), subCountryEntries(t, "US-CA"))
			if err == nil || isTargetingRefusal(err) {
				t.Fatalf("err = %v, want a plain error", err)
			}
		})
	}
}

// No ids, no call.
func TestValidateSubCountryGeoTargets_EmptyMakesNoCall(t *testing.T) {
	c, seen := geoValidationServer(t, 500, `{}`)
	if err := c.ValidateSubCountryGeoTargets(context.Background(), nil); err != nil || len(seen()) != 0 {
		t.Fatalf("err = %v, calls = %d", err, len(seen()))
	}
}
