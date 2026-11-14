// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package apivalidation

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	connserver "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_connections/server"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	goahttp "goa.design/goa/v3/http"
)

// decodeLinkedInTypeahead runs a real targeting-entities request through the generated HTTP
// decoder (#585).
func decodeLinkedInTypeahead(t *testing.T, query url.Values) (facet, q string, err error) {
	t.Helper()
	mux := goahttp.NewMuxer()
	decode := connserver.DecodeSearchLinkedinAdsTargetingEntitiesRequest(mux, goahttp.RequestDecoder)
	var routed *http.Request
	connserver.MountSearchLinkedinAdsTargetingEntitiesHandler(mux, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { routed = r }))
	req := httptest.NewRequest(http.MethodGet, "/projects/cncf/linkedin-ads/targeting-entities?"+query.Encode(), nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)
	if routed == nil {
		t.Fatal("search-linkedin-ads-targeting-entities was not routed")
	}
	p, derr := decode(routed)
	if derr != nil {
		return "", "", derr
	}
	return p.Facet, p.Query, nil
}

// The design's facet enum is exactly the facets the linkedin package serves, and its query
// bound is MaxTypeaheadQueryLen characters.
func TestLinkedInTypeaheadDecoder_MirrorsThePlatform(t *testing.T) {
	for _, f := range linkedin.TypeaheadFacetKeys() {
		if _, _, err := decodeLinkedInTypeahead(t, url.Values{"facet": {f}, "query": {"a"}}); err != nil {
			t.Errorf("decoder refuses facet %s, which the platform serves: %v", f, err)
		}
	}
	facet, q, err := decodeLinkedInTypeahead(t, url.Values{"facet": {"locations"}, "query": {strings.Repeat("é", linkedin.MaxTypeaheadQueryLen)}})
	if err != nil || facet != "locations" || len([]rune(q)) != linkedin.MaxTypeaheadQueryLen {
		t.Errorf("a %d-character query: facet=%q len=%d err=%v", linkedin.MaxTypeaheadQueryLen, facet, len([]rune(q)), err)
	}
}

func TestLinkedInTypeaheadDecoder_RefusesWhatTheServiceWouldRefuse(t *testing.T) {
	cases := map[string]url.Values{
		"no facet":            {"query": {"a"}},
		"no query":            {"facet": {"skills"}},
		"segment facet":       {"facet": {"dynamicSegments"}, "query": {"a"}},
		"non-typeahead facet": {"facet": {"seniorities"}, "query": {"a"}},
		"blank query":         {"facet": {"skills"}, "query": {"   "}},
		"101-character query": {"facet": {"skills"}, "query": {strings.Repeat("é", linkedin.MaxTypeaheadQueryLen+1)}},
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := decodeLinkedInTypeahead(t, v); err == nil {
				t.Error("decoder accepted a request the contract forbids")
			}
		})
	}
}
