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
	goahttp "goa.design/goa/v3/http"
)

// decodeGeoSuggest runs a real geo-targets request through the generated HTTP decoder, so the
// service-layer checks are tested against requests the endpoint actually accepts.
func decodeGeoSuggest(t *testing.T, query url.Values) (q string, types []string, limit *int, err error) {
	t.Helper()
	mux := goahttp.NewMuxer()
	decode := connserver.DecodeSuggestGoogleAdsGeoTargetsRequest(mux, goahttp.RequestDecoder)
	var routed *http.Request
	connserver.MountSuggestGoogleAdsGeoTargetsHandler(mux, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { routed = r }))
	req := httptest.NewRequest(http.MethodGet, "/projects/cncf/google-ads/geo-targets?"+query.Encode(), nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)
	if routed == nil {
		t.Fatal("suggest-google-ads-geo-targets was not routed")
	}
	p, derr := decode(routed)
	if derr != nil {
		return "", nil, nil, derr
	}
	return p.Q, p.TargetTypes, p.Limit, nil
}

func TestGeoSuggestDecoder_AcceptsMultiByteWithinLimitsAndRepeatedTypes(t *testing.T) {
	v := url.Values{}
	v.Set("q", strings.Repeat("東", 50))
	v.Add("target_types", strings.Repeat("市", 39)+"1")
	v.Add("target_types", "Postal Code")
	q, types, limit, err := decodeGeoSuggest(t, v)
	if err != nil {
		t.Fatalf("decoder refused a within-contract request: %v", err)
	}
	if len([]rune(q)) != 50 || len(types) != 2 || types[1] != "Postal Code" {
		t.Errorf("q=%q types=%v", q, types)
	}
	if limit != nil {
		t.Errorf("an omitted limit must decode as absent (the service applies 20), got %d", *limit)
	}
}

func TestGeoSuggestDecoder_RefusesWhatTheServiceWouldRefuse(t *testing.T) {
	cases := map[string]url.Values{
		"blank q":          {"q": {"   "}},
		"separator type":   {"q": {"a"}, "target_types": {" _ "}},
		"limit 0":          {"q": {"a"}, "limit": {"0"}},
		"101-character q":  {"q": {strings.Repeat("東", 101)}},
		"three-letter iso": {"q": {"a"}, "country_code": {"USA"}},
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := decodeGeoSuggest(t, v); err == nil {
				t.Error("decoder accepted a request the contract forbids")
			}
		})
	}
}
