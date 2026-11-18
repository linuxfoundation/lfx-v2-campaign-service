// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestLinkedinRegion pins the Region segment (#587): the first geo target's region in the
// vocabulary Meta and Reddit use, and "Global" for none or an unmapped URN.
func TestLinkedinRegion(t *testing.T) {
	geo := func(urns ...string) []GeoTarget {
		out := make([]GeoTarget, len(urns))
		for i, u := range urns {
			out[i] = GeoTarget{URN: u}
		}
		return out
	}
	for _, tc := range []struct {
		name string
		geo  []GeoTarget
		want string
	}{
		{"no geo", nil, "Global"},
		{"United States", geo("urn:li:geo:103644278"), "NA"},
		{"Germany", geo("urn:li:geo:101282230"), "EMEA"},
		{"United Kingdom", geo("urn:li:geo:101165590"), "EMEA"},
		{"India", geo("urn:li:geo:102713980"), "India"},
		{"OSS Japan's set: the first decides", geo("urn:li:geo:101355337", "urn:li:geo:102454443", "urn:li:geo:103291313"), "APAC"},
		{"the first decides a mixed set", geo("urn:li:geo:103644278", "urn:li:geo:101355337"), "NA"},
		{"an unmapped URN", geo("urn:li:geo:999"), "Global"},
		{"surrounding space is ignored", geo(" urn:li:geo:101452733 "), "APAC"},
		// CreateCampaign drops a target the resolver left without a URN, so the region is the
		// first one actually targeted, not the first one listed.
		{"an unresolved first entry is skipped", geo("", "urn:li:geo:103644278"), "NA"},
		{"only unresolved entries", geo("", " "), "Global"},
	} {
		if got := linkedinRegion(tc.geo); got != tc.want {
			t.Errorf("%s: linkedinRegion = %q, want %q", tc.name, got, tc.want)
		}
	}
	// Every resolvable country has a region, so no LFX create lands on "Global" by omission.
	for name, g := range geoResolveMap {
		if _, ok := linkedinGeoRegion[g.URN]; !ok {
			t.Errorf("geoResolveMap %q (%s) has no region in linkedinGeoRegion", name, g.URN)
		}
	}
}

// TestTargetingSegment pins #587's Targeting segment: "Retargeting" for the retargeting tactic,
// the vocabulary word lf-dbt stores as-is (it reads an 8-segment name by position); every other
// tactic keeps the "Prospecting <tactic>" shape.
func TestTargetingSegment(t *testing.T) {
	for _, tc := range []struct{ tactic, slot, want string }{
		{"", "", "Prospecting"},
		{"retargeting", "", "Retargeting"},
		{"Retargeting", "#2", "Retargeting #2"},
		{"prospecting", "", "Prospecting prospecting"},
		{"lookalike", "#3", "Prospecting lookalike #3"},
		{"", "#2", "Prospecting #2"},
		{" retargeting | x", "", "Prospecting retargeting - x"},
	} {
		if got := targetingSegment(tc.tactic, tc.slot); got != tc.want {
			t.Errorf("targetingSegment(%q, %q) = %q, want %q", tc.tactic, tc.slot, got, tc.want)
		}
	}
}

func TestBuildCampaignName_RegionAndRetargeting(t *testing.T) {
	in := CampaignInput{Tactic: "retargeting", StageToken: "BoFU", GeoTargets: []GeoTarget{{URN: "urn:li:geo:101355337"}}}
	const want = "Events | OSS Japan | APAC | Conversions | Retargeting | Static | tlf | BoFU"
	if got := buildCampaignName("OSS Japan", "Conversions", "tlf", in); got != want {
		t.Fatalf("name = %q\nwant   %q", got, want)
	}
	const legacy = "Events | OSS Japan | LinkedIn | Conversions | Prospecting retargeting | Static | tlf | BoFU"
	if got := legacyCampaignName("OSS Japan", "Conversions", "tlf", in); got != legacy {
		t.Fatalf("legacy name = %q\nwant          %q", got, legacy)
	}
}

// legacyFallbackServer fakes a LinkedIn account whose campaign group already exists (or not) and
// whose campaign search answers with existingCampaign when it is non-empty. It records the
// campaign POST bodies' names and how many campaign searches ran.
func legacyFallbackServer(t *testing.T, groupExists bool, existingCampaign string) (*httptest.Server, func() (posts []string, searches int)) {
	t.Helper()
	var (
		mu       sync.Mutex
		posts    []string
		searches int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			switch {
			case strings.HasSuffix(r.URL.Path, "adCampaignGroups/100"):
				_, _ = io.WriteString(w, `{"id":100,"runSchedule":{"start":1}}`)
			case strings.Contains(r.URL.Path, "adCampaignGroups"):
				if groupExists {
					_, _ = io.WriteString(w, `{"elements":[{"name":"Events | KubeCon | tlf","status":"ACTIVE","id":"urn:li:sponsoredCampaignGroup:100"}],"metadata":{"nextPageToken":""}}`)
					return
				}
				_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			case strings.Contains(r.URL.Path, "adCampaigns"):
				mu.Lock()
				searches++
				mu.Unlock()
				if existingCampaign != "" {
					// Marshalled, not concatenated, so a name needing JSON escaping stays valid JSON.
					_ = json.NewEncoder(w).Encode(map[string]any{
						"elements": []map[string]string{{"name": existingCampaign, "status": "PAUSED", "id": "urn:li:sponsoredCampaign:777", "campaignGroup": "urn:li:sponsoredCampaignGroup:100"}},
						"metadata": map[string]string{"nextPageToken": ""},
					})
					return
				}
				_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			default:
				_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
			}
			return
		}
		b, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			mu.Lock()
			posts = append(posts, string(b))
			mu.Unlock()
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected POST "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() ([]string, int) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), posts...), searches
	}
}

// The pre-#587 name of validCampaignInput's automatic create.
const legacyKubeConName = "Events | KubeCon | LinkedIn | Traffic | Prospecting | Static | tlf | MoFU"

// TestCreateCampaign_ReusesALegacyNamedCampaign: a retry spanning #587 finds the campaign an
// earlier attempt created under the old name, in the existing group, and reuses it under that
// name, instead of creating a second campaign beside it.
func TestCreateCampaign_ReusesALegacyNamedCampaign(t *testing.T) {
	srv, seen := legacyFallbackServer(t, true, legacyKubeConName)
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	res, err := c.CreateCampaign(context.Background(), validCampaignInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	posts, _ := seen()
	if res.CampaignID != "777" || len(posts) != 0 {
		t.Fatalf("campaign = %q with %d campaign POST(s); want the reused 777 and none", res.CampaignID, len(posts))
	}
	if res.CampaignName != legacyKubeConName {
		t.Errorf("campaign name = %q, want the legacy %q it was found under", res.CampaignName, legacyKubeConName)
	}
	if !strings.Contains(strings.Join(res.Steps, "\n"), "under its earlier name") {
		t.Errorf("steps %v do not say the campaign was found under its earlier name", res.Steps)
	}
}

// TestCreateCampaign_NewNameWhenNoLegacyCampaign: with no legacy campaign the create uses the new
// name, and a NEW group skips the legacy lookup (one campaign search, not two).
func TestCreateCampaign_NewNameWhenNoLegacyCampaign(t *testing.T) {
	const want = `"name":"Events | KubeCon | NA | Traffic | Prospecting | Static | tlf | MoFU"`
	for _, tc := range []struct {
		name         string
		groupExists  bool
		wantSearches int
	}{
		{"existing group: the legacy name is looked up first", true, 2},
		{"new group: no legacy lookup", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, seen := legacyFallbackServer(t, tc.groupExists, "")
			c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
			if _, err := c.CreateCampaign(context.Background(), validCampaignInput()); err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			posts, searches := seen()
			if len(posts) != 1 || !strings.Contains(posts[0], want) {
				t.Fatalf("campaign POSTs = %v, want one carrying %s", posts, want)
			}
			if searches != tc.wantSearches {
				t.Errorf("campaign searches = %d, want %d", searches, tc.wantSearches)
			}
		})
	}
}

// TestCreateCampaign_TextCreateSkipsTheLegacyLookup: no text campaign (#586) predates #587, so a
// text create in an existing group never looks up the pre-#587 name. Were it to, it would find a
// single-image campaign of that Static name (the fake reports no type, so the type guard cannot
// refuse it) and push text ads onto it. One campaign search, and a new campaign is created.
func TestCreateCampaign_TextCreateSkipsTheLegacyLookup(t *testing.T) {
	const legacyConversions = "Events | KubeCon | LinkedIn | Conversions | Prospecting | Static | tlf | MoFU"
	srv, seen := legacyFallbackServer(t, true, legacyConversions)
	c := NewClient(Credentials{AccessToken: "t"}, testConfig(), WithBaseURL(srv.URL), WithClock(fixedClock()))
	in := validCampaignInput()
	in.Variants = nil
	in.Format = FormatText
	in.ManualCPCBid = 5
	in.TextAds = []TextAd{{Headline: "KubeCon 2099", Description: "Join cloud native engineers."}}
	res, err := c.CreateCampaign(context.Background(), in)
	posts, searches := seen()
	if searches != 1 {
		t.Errorf("campaign searches = %d, want 1 (no legacy lookup for a text create)", searches)
	}
	if len(posts) != 1 || !strings.Contains(posts[0], `"type":"TEXT_AD"`) {
		t.Fatalf("campaign POSTs = %v (err %v), want one TEXT_AD create", posts, err)
	}
	if res == nil || res.CampaignID == "777" || res.CampaignName == legacyConversions {
		t.Errorf("result = %+v, want a new campaign under the new name", res)
	}
}
