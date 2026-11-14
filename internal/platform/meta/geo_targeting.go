// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// MaxGeoKeys caps each of targeting.regions and targeting.cities (#550, gap 13). Meta allows far
// more; a regional event names a handful, and the lookup sends every key in one request URL.
const MaxGeoKeys = 25

// subCountryGeo reports whether t names a region or a city.
func (t *AudienceTargeting) subCountryGeo() bool {
	return t != nil && (len(t.Regions) > 0 || len(t.Cities) > 0)
}

// geoLocationMeta is one location of Meta's geo-location metadata read (Targeting Search,
// `type=adgeolocationmeta`): the key that was asked about, its type and the ISO country it lies in.
type geoLocationMeta struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	CountryCode string `json:"country_code"`
}

// checkGeoLocations proves, before the first mutating call, that every region and city t names is
// a location Meta knows, and that its parent country passes the same rules a country target does:
// ISO, not sanctioned or otherwise ineligible (metaIneligibleCountries), not a regulated market
// (regulatedCountries), not already targeted as a whole country (Meta refuses overlapping
// locations), and, when it is an EU DSA territory, covered by the DSA disclosures. One
// GET /search?type=adgeolocationmeta&regions=[…]&cities=[…] answers every key. A failed read or a
// key left unanswered is an error; a location that breaks a rule is a refusal; either way nothing is
// created. It returns the parent countries in first-seen order (the campaign name's region segment
// uses them when no country is targeted).
func (c *Client) checkGeoLocations(ctx context.Context, t *AudienceTargeting, countries []string, dsaDisclosed bool) ([]string, error) {
	if !t.subCountryGeo() {
		return nil, nil
	}
	q := "/search?type=adgeolocationmeta"
	for _, l := range []struct {
		param string
		keys  []string
	}{{"regions", t.Regions}, {"cities", t.Cities}} {
		if len(l.keys) == 0 {
			continue
		}
		list, err := json.Marshal(l.keys)
		if err != nil {
			return nil, fmt.Errorf("meta targeting: encode the %s list: %w", l.param, err)
		}
		q += "&" + l.param + "=" + url.QueryEscape(string(list))
	}
	raw, err := c.getChecked(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("meta targeting: the regions and cities could not be looked up, so nothing was created: %w", err)
	}
	var page struct {
		Data *struct {
			Regions map[string]geoLocationMeta `json:"regions"`
			Cities  map[string]geoLocationMeta `json:"cities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &page); err != nil || page.Data == nil {
		return nil, fmt.Errorf("meta targeting: the geo-location lookup returned no data, so nothing was created")
	}
	targeted := make(map[string]bool, len(countries))
	for _, cc := range countries {
		targeted[cc] = true
	}
	var parents []string
	seen := map[string]bool{}
	for _, l := range []struct {
		kind string
		keys []string
		got  map[string]geoLocationMeta
	}{{"region", t.Regions, page.Data.Regions}, {"city", t.Cities, page.Data.Cities}} {
		for _, key := range l.keys {
			loc, ok := l.got[key]
			if !ok || strings.TrimSpace(loc.Key) != key {
				return nil, fmt.Errorf("meta targeting: the geo-location lookup did not answer for %s %s, so nothing was created", l.kind, domain.CallerValue(key))
			}
			if typ := strings.TrimSpace(loc.Type); typ != "" && typ != l.kind {
				return nil, domain.PreflightRefusalf("meta targeting: %s key %s is a Meta %s, not a %s; move it to the matching list; nothing was created", l.kind, domain.CallerValue(key), domain.CallerValue(typ), l.kind)
			}
			cc := strings.ToUpper(strings.TrimSpace(loc.CountryCode))
			if _, iso := iso3166Alpha2[cc]; !iso || !geoCodeRE.MatchString(cc) {
				return nil, fmt.Errorf("meta targeting: the geo-location lookup named no country for %s %s, so nothing was created", l.kind, domain.CallerValue(key))
			}
			switch {
			case metaIneligibleCountries[cc]:
				return nil, domain.PreflightRefusalf("meta targeting: %s %s is in %s, which is not eligible for Meta ad targeting (sanctioned or excluded); nothing was created", l.kind, domain.CallerValue(key), cc)
			case regulatedCountries[cc]:
				return nil, domain.PreflightRefusalf("meta targeting: %s %s is in %s, a market that requires a regional compliance declaration in Meta Ads Manager and is excluded from API targeting; nothing was created", l.kind, domain.CallerValue(key), cc)
			case targeted[cc]:
				return nil, domain.PreflightRefusalf("meta targeting: %s %s is in %s, which geoTargets already targets as a whole country; Meta refuses overlapping locations, so remove one; nothing was created", l.kind, domain.CallerValue(key), cc)
			case dsaTerritories[cc] && !dsaDisclosed:
				return nil, domain.PreflightRefusalf("meta targeting: %s %s is in %s, an EU territory, so dsaBeneficiary and dsaPayor are required; nothing was created", l.kind, domain.CallerValue(key), cc)
			}
			if !seen[cc] {
				seen[cc] = true
				parents = append(parents, cc)
			}
		}
	}
	return parents, nil
}

// adSetGeoLocations builds the ad set's `geo_locations`: the eligible countries (omitted when there
// are none, which only a region- or city-only create reaches) plus t's regions and cities, each as
// a list of `{"key": …}`. t must be NormalizeAudienceTargeting's output.
func adSetGeoLocations(countries []string, t *AudienceTargeting) map[string]any {
	geo := map[string]any{}
	if len(countries) > 0 {
		geo["countries"] = countries
	}
	if t == nil {
		return geo
	}
	if len(t.Regions) > 0 {
		geo["regions"] = geoKeyRefs(t.Regions)
	}
	if len(t.Cities) > 0 {
		geo["cities"] = geoKeyRefs(t.Cities)
	}
	return geo
}

func geoKeyRefs(keys []string) []map[string]any {
	out := make([]map[string]any, len(keys))
	for i, k := range keys {
		out[i] = map[string]any{"key": k}
	}
	return out
}

// geoSummary renders the ad set's locations for its Steps line: the countries, then the number of
// regions and cities (their keys are opaque numbers, so a count reads better).
func geoSummary(countries []string, t *AudienceTargeting) string {
	parts := append([]string(nil), countries...)
	if t != nil && len(t.Regions) > 0 {
		parts = append(parts, fmt.Sprintf("%d region(s)", len(t.Regions)))
	}
	if t != nil && len(t.Cities) > 0 {
		parts = append(parts, fmt.Sprintf("%d city(ies)", len(t.Cities)))
	}
	return strings.Join(parts, ", ")
}
