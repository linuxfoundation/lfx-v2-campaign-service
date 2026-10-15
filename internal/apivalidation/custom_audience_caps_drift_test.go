// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package apivalidation

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	abserver "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_audience_builder/server"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/audience"
	goahttp "goa.design/goa/v3/http"
)

// decodePreviewCustom routes a preview-custom-audience body through the real generated muxer,
// decoder and validator, and returns the decode error.
func decodePreviewCustom(t *testing.T, criteria map[string]any) error {
	t.Helper()
	mux := goahttp.NewMuxer()
	decode := abserver.DecodePreviewCustomAudienceRequest(mux, goahttp.RequestDecoder)
	var routed *http.Request
	abserver.MountPreviewCustomAudienceHandler(mux, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { routed = r }))
	body, err := json.Marshal(map[string]any{"custom": map[string]any{"criteria": criteria}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/projects/cncf/audience-builder/custom/preview", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(httptest.NewRecorder(), req)
	if routed == nil {
		t.Fatal("preview-custom-audience was not routed")
	}
	_, err = decode(routed)
	return err
}

func values(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%02d", prefix, i)
	}
	return out
}

// TestCustomDraftCaps_MatchTheGeneratedValidator pins the Go caps that keep an interpreted draft
// inside the criteria contract against the generated validator itself, not against the Go
// constants: a draft parsed from an oversized model reply must decode unchanged, and one item
// past each cap must be refused. A Go cap drifting from its design MaxLength fails here.
func TestCustomDraftCaps_MatchTheGeneratedValidator(t *testing.T) {
	// A name one past the contract's length comes first so the parser must drop it, then one at
	// the length, then more filters than the cap.
	filters := []string{
		`{"property":"` + strings.Repeat("q", 101) + `","operator":"is_known","values":[]}`,
		`{"property":"` + strings.Repeat("p", 100) + `","operator":"is_known","values":[]}`,
	}
	quote := func(in []string) string { return `"` + strings.Join(in, `","`) + `"` }
	for i := range 12 {
		filters = append(filters, fmt.Sprintf(`{"property":"prop_%02d","operator":"is_any_of","values":[%s]}`, i, quote(values("v", 30))))
	}
	raw := `{"criteria":{"cities":[` + quote(values("city", 30)) + `],"countries":[` + quote(values("Countryland ", 30)) + `],"contact_location":true,"event_registered":false,"education_enrolled":true,"gate_event_history":false,"brand_short":"","property_filters":[` +
		strings.Join(filters, ",") + `],"speakers":{"scope":"current","event_names":[` + quote(values("Synthetic Summit Track ", 30)) + `]},"ambassadors":false,"education_topics":[` + quote(values("topic", 30)) + `],"page_view_urls":[` + quote(values("events.example.org/page-", 15)) +
		`],"registered_for":[` + quote(values("Synthetic Summit 19", 30)) + `],"label":""},"questions":[],"notes":[]}`
	d, err := audience.ParseCustomDraft(raw)
	if err != nil {
		t.Fatalf("ParseCustomDraft: %v", err)
	}
	c := d.Criteria
	// The parser must have hit every cap, or the one-past cases below prove nothing.
	if len(c.Cities) != 25 || len(c.Countries) != 25 || len(c.EducationTopics) != 25 || c.Speakers == nil || len(c.Speakers.EventNames) != 25 ||
		len(c.PropertyFilters) != 10 || len(c.PropertyFilters[1].Values) != 25 || len(c.PageViewURLs) != audience.CustomMaxPageViewURLs {
		t.Fatalf("draft not at its caps: %+v", c)
	}
	if c.PropertyFilters[0].Property != strings.Repeat("p", 100) {
		t.Fatalf("the over-long property name was kept: %q", c.PropertyFilters[0].Property)
	}
	propertyFilters := make([]map[string]any, 0, len(c.PropertyFilters))
	for _, f := range c.PropertyFilters {
		propertyFilters = append(propertyFilters, map[string]any{"property": f.Property, "operator": string(f.Operator), "values": f.Values})
	}
	draft := map[string]any{
		"cities":           c.Cities,
		"countries":        c.Countries,
		"education_topics": c.EducationTopics,
		"speakers":         map[string]any{"scope": string(c.Speakers.Scope), "event_names": c.Speakers.EventNames},
		"contact_location": true,
		"gate_list_id":     "26716",
		"property_filters": propertyFilters,
		"page_view_urls":   c.PageViewURLs,
		"registered_for":   c.RegisteredFor,
	}
	if err := decodePreviewCustom(t, draft); err != nil {
		t.Fatalf("an unedited parsed draft must decode: %v", err)
	}

	// registered_for is filled by the warehouse in dispatch, which honours CustomMaxRegisteredFor.
	atCap := map[string]any{"registered_for": values("Synthetic Summit 19", audience.CustomMaxRegisteredFor)}
	if err := decodePreviewCustom(t, atCap); err != nil {
		t.Errorf("registered_for at CustomMaxRegisteredFor must decode: %v", err)
	}

	onePast := map[string]map[string]any{
		"cities":           {"cities": append(c.Cities, "one-more")},
		"property_filters": {"property_filters": append(propertyFilters, map[string]any{"property": "one_more", "operator": "is_known"})},
		"page_view_urls":   {"page_view_urls": append(c.PageViewURLs, "events.example.org/one-more")},
		"registered_for":   {"registered_for": values("Synthetic Summit 19", audience.CustomMaxRegisteredFor+1)},
		"property name":    {"property_filters": []map[string]any{{"property": strings.Repeat("p", 101), "operator": "is_known"}}},
		"countries":        {"countries": append(c.Countries, "One More")},
		"education_topics": {"education_topics": append(c.EducationTopics, "one-more")},
		"event_names":      {"speakers": map[string]any{"scope": "current", "event_names": append(c.Speakers.EventNames, "One More")}},
		"filter values":    {"property_filters": []map[string]any{{"property": "prop_01", "operator": "is_any_of", "values": append(c.PropertyFilters[1].Values, "one-more")}}},
	}
	for name, body := range onePast {
		if err := decodePreviewCustom(t, body); err == nil {
			t.Errorf("%s one past the parser's cap decoded; the Go cap is looser than the contract", name)
		}
	}
}
