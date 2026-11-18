// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// ---------------------------------------------------------------------------
// Sub-country geolocation targeting (#549 gap 11).
//
// Reddit Ads API v3 (public OpenAPI document, https://ads-api.reddit.com/api/v3/openapi.json,
// read 2026-11-18): the ad group's targeting.geolocations takes geolocation ids from
// GET /targeting/geolocations (list_geolocations), not only countries. The documented examples are
// a country ("US"), a region ("US-VA", "US-CA"), a DMA ("662") and a city ("CA:6167865",
// "GB:2641599"). POST /targeting/geolocations_validations (geolocations_validations) takes
// {"data":{"geolocation_ids":[...]}} and answers data[] of {geolocation:{id, country, region, dma,
// city, ...}, error_message}, where error_message says why an id is untargetable.
//
// The ids are taken as Reddit spells them; there is no name lookup. Each one is confirmed with the
// validations read before anything is created, so an id Reddit would refuse at ad-group time
// cannot orphan the PAUSED campaign created a step earlier.
//
// VERIFICATION LEVEL: neither the id shapes beyond the documented examples nor the validations
// answer has been exercised against a live account, which is why the dispatcher keeps this behind
// REDDIT_SUBCOUNTRY_GEO_ENABLED.
// ---------------------------------------------------------------------------

const (
	// MaxSubCountryGeoTargets caps CampaignInput.SubCountryGeoTargets, matching
	// MaxExcludedGeoTargets. Reddit's own maxItems is 20000; the cap bounds the request this
	// service builds and the one validations read.
	MaxSubCountryGeoTargets = 50

	// FieldSubCountryGeoTargets is the redditConfig key a refusal names.
	FieldSubCountryGeoTargets = "subCountryGeoTargets"

	// dmaCountry is the country of every DMA: DMAs are Nielsen's US media markets.
	dmaCountry = "US"
)

var (
	// subCountryRegionRE is a region id: an ISO 3166-1 alpha-2 country, a hyphen and the
	// ISO 3166-2 subdivision part (1-3 letters or digits), e.g. "US-CA". A longer digit run
	// ("US-90210") is a postal code, which this field does not accept.
	subCountryRegionRE = regexp.MustCompile(`^([A-Z]{2})-([A-Z0-9]{1,3})$`)
	// subCountryCityRE is a city id: a country, a colon and a positive numeric id,
	// e.g. "CA:6167865".
	subCountryCityRE = regexp.MustCompile(`^([A-Z]{2}):[1-9][0-9]{0,9}$`)
	// subCountryDMARE is a DMA id: a three-digit Nielsen market code, e.g. "662".
	subCountryDMARE = regexp.MustCompile(`^[1-9][0-9]{2}$`)
)

// SubCountryGeo is one normalized sub-country geolocation id, the country it lies in, and its
// 1-based position in the caller's list, so a refusal names the entry the caller sent.
type SubCountryGeo struct {
	ID      string
	Country string
	Entry   int
}

// subCountryGeoIDs returns the ids of geos, in order.
func subCountryGeoIDs(geos []SubCountryGeo) []string {
	out := make([]string, len(geos))
	for i, g := range geos {
		out[i] = g.ID
	}
	return out
}

// subCountryCountry returns the country an id lies in, or "" when id is not a region, city or
// DMA id. id must already be trimmed and uppercased.
func subCountryCountry(id string) string {
	if m := subCountryRegionRE.FindStringSubmatch(id); m != nil {
		return m[1]
	}
	if m := subCountryCityRE.FindStringSubmatch(id); m != nil {
		return m[1]
	}
	if subCountryDMARE.MatchString(id) {
		return dmaCountry
	}
	return ""
}

// NormalizeSubCountryGeoTargets validates and normalizes the sub-country geolocation ids against
// the country targeting they would be sent with. It is the ONE rule set: CreateCampaign applies
// it before any upstream call, and the dispatcher applies it in PreflightCreate so a bad list is
// a synchronous 400.
//
//   - Each entry is trimmed and uppercased, and must be a region ("US-CA"), city ("CA:6167865")
//     or DMA ("662") id whose country is an assigned ISO 3166-1 alpha-2 code. A blank entry, a
//     bare country (that belongs in geoTargets) or any other shape is refused.
//   - Duplicates are dropped, first kept.
//   - An id inside a country geoTargets already targets is refused: the country covers it, so
//     sending both is an overlap. The rule reads the countries as sent, so the US default does
//     not apply when this list is set (CreateCampaign drops it).
//   - An id inside a country excludedGeoTargets excludes is refused: it could never serve.
//
// The list is capped (MaxSubCountryGeoTargets) on its raw length. A refusal is an
// *ExclusionError naming FieldSubCountryGeoTargets.
func NormalizeSubCountryGeoTargets(geoTargets, excludedGeoTargets, subCountry []string) ([]SubCountryGeo, error) {
	if len(subCountry) == 0 {
		return nil, nil
	}
	if n := len(subCountry); n > MaxSubCountryGeoTargets {
		return nil, &ExclusionError{Field: FieldSubCountryGeoTargets, Msg: fmt.Sprintf("at most %d geolocation ids, got %d", MaxSubCountryGeoTargets, n)}
	}
	targeted := upperSet(geoTargets)
	excluded := upperSet(excludedGeoTargets)
	out := make([]SubCountryGeo, 0, len(subCountry))
	seen := make(map[string]struct{}, len(subCountry))
	for i, raw := range subCountry {
		id := strings.ToUpper(strings.TrimSpace(raw))
		if id == "" {
			return nil, &ExclusionError{Field: FieldSubCountryGeoTargets, Msg: fmt.Sprintf("entry %d is blank", i+1)}
		}
		if geoCodeRE.MatchString(id) {
			return nil, &ExclusionError{Field: FieldSubCountryGeoTargets, Msg: fmt.Sprintf("entry %d (%s) is a whole country; put it in geoTargets", i+1, domain.CallerValue(id))}
		}
		country := subCountryCountry(id)
		if country == "" || !iso3166Alpha2[country] {
			return nil, &ExclusionError{Field: FieldSubCountryGeoTargets, Msg: fmt.Sprintf("entry %d (%s) is not a Reddit region (US-CA), city (CA:6167865) or DMA (662) geolocation id", i+1, domain.CallerValue(id))}
		}
		if _, dup := seen[id]; dup {
			continue
		}
		if _, both := targeted[country]; both {
			return nil, &ExclusionError{Field: FieldSubCountryGeoTargets, Msg: fmt.Sprintf("entry %d (%s) is inside %s, which geoTargets already targets as a whole country; send the country or the locations inside it, not both", i+1, domain.CallerValue(id), country)}
		}
		if _, both := excluded[country]; both {
			return nil, &ExclusionError{Field: FieldSubCountryGeoTargets, Msg: fmt.Sprintf("entry %d (%s) is inside %s, which excludedGeoTargets excludes; a location cannot be targeted inside an excluded country", i+1, domain.CallerValue(id), country)}
		}
		seen[id] = struct{}{}
		out = append(out, SubCountryGeo{ID: id, Country: country, Entry: i + 1})
	}
	return out, nil
}

// redditGeoLabel is the ad group name's geo segment: the countries "+"-joined, as before, then at
// most the first sub-country id and a count of the rest ("US-CA+49 more"). Listing every id would
// let a list well under MaxSubCountryGeoTargets push the name past redditMaxNameRunes, a refusal
// only CreateCampaign can make; the summary keeps every allowed list nameable. The full list is in
// targeting.geolocations and the step log.
func redditGeoLabel(countries []string, subCountry []SubCountryGeo) string {
	parts := append([]string(nil), countries...)
	switch n := len(subCountry); {
	case n == 1:
		parts = append(parts, subCountry[0].ID)
	case n > 1:
		parts = append(parts, fmt.Sprintf("%s+%d more", subCountry[0].ID, n-1))
	}
	return strings.Join(parts, "+")
}

// upperSet is the set of trimmed, uppercased, non-blank values.
func upperSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, v := range values {
		if s := strings.ToUpper(strings.TrimSpace(v)); s != "" {
			set[s] = struct{}{}
		}
	}
	return set
}

type validatedGeolocation struct {
	Geolocation *struct {
		ID      string `json:"id"`
		Country string `json:"country"`
	} `json:"geolocation"`
	ErrorMessage string `json:"error_message"`
}

// ValidateSubCountryGeoTargets confirms every entry (from NormalizeSubCountryGeoTargets) is a
// targetable Reddit geolocation in the country its id names, with one
// POST /targeting/geolocations_validations. An id Reddit answers with an error_message is a
// domain.PreflightRefusal naming the entry's position in the caller's list (Reddit's own text is
// not quoted). Anything else that leaves an id unconfirmed — a failed read, an answer that cannot
// be believed, an id answered twice, an id the answer does not mention, an error Reddit did not attribute to an id, or
// a country other than the one the id names — is a plain error. Either way the caller creates
// nothing.
//
// The ids are matched ignoring case. Whether Reddit echoes each id back exactly as sent is one of
// the things the live probe must confirm before REDDIT_SUBCOUNTRY_GEO_ENABLED is turned on.
func (c *Client) ValidateSubCountryGeoTargets(ctx context.Context, entries []SubCountryGeo) error {
	if len(entries) == 0 {
		return nil
	}
	if len(entries) > MaxSubCountryGeoTargets {
		return domain.PreflightRefusalf("invalid redditConfig.%s: at most %d geolocation ids, got %d", FieldSubCountryGeoTargets, MaxSubCountryGeoTargets, len(entries))
	}
	body := map[string]any{"data": map[string]any{"geolocation_ids": subCountryGeoIDs(entries)}}
	// A validation creates nothing, so the throttle retry of request() is safe here.
	resp, err := c.request(ctx, http.MethodPost, "/targeting/geolocations_validations", body)
	if err != nil {
		return fmt.Errorf("reddit geolocation validation failed: %w", err)
	}
	if resp.raw != nil && identityjson.Check(resp.raw) != nil {
		return fmt.Errorf("reddit geolocation validation: the response cannot be trusted (duplicated key or malformed text)")
	}
	var items []validatedGeolocation
	if len(resp.Data) == 0 || string(resp.Data) == "null" || json.Unmarshal(resp.Data, &items) != nil {
		return fmt.Errorf("reddit geolocation validation: the response carries no list")
	}
	valid := map[string]string{} // id -> country
	refused := map[string]struct{}{}
	listed := map[string]struct{}{}
	unattributed := 0
	for _, it := range items {
		id := ""
		if it.Geolocation != nil {
			id = strings.ToUpper(strings.TrimSpace(it.Geolocation.ID))
		}
		// A repeated id makes the answer unreadable: two rows could disagree on its country or on
		// whether it is targetable, and keeping either one would be a guess.
		if id != "" {
			if _, dup := listed[id]; dup {
				return fmt.Errorf("reddit geolocation validation: %s was answered twice", id)
			}
			listed[id] = struct{}{}
		}
		if strings.TrimSpace(it.ErrorMessage) != "" {
			if id == "" {
				unattributed++
			} else {
				refused[id] = struct{}{}
			}
			continue
		}
		if id == "" {
			return fmt.Errorf("reddit geolocation validation: a geolocation has no id")
		}
		valid[id] = strings.ToUpper(strings.TrimSpace(it.Geolocation.Country))
	}
	for _, e := range entries {
		if _, bad := refused[e.ID]; bad {
			return domain.PreflightRefusalf("invalid redditConfig.%s: entry %d (%s) is not a targetable Reddit geolocation", FieldSubCountryGeoTargets, e.Entry, domain.CallerValue(e.ID))
		}
		country, ok := valid[e.ID]
		if !ok {
			if unattributed > 0 {
				return fmt.Errorf("reddit geolocation validation: Reddit refused %d geolocation(s) without naming them, so %s is unconfirmed", unattributed, e.ID)
			}
			return fmt.Errorf("reddit geolocation validation: the answer does not mention %s", e.ID)
		}
		// A blank country cannot contradict the id; a different one means the overlap rules were
		// applied to the wrong country, so the create stops rather than guess.
		if country != "" && country != e.Country {
			return fmt.Errorf("reddit geolocation validation: Reddit places %s in %s, not %s", e.ID, country, e.Country)
		}
	}
	return nil
}
