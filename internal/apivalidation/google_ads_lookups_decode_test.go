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

// The conversion-action picker (#458) is mounted for GET only, so no write method reaches it,
// and the generated decoder takes the project from the path.
func TestConversionActionsRoute_IsGetOnly(t *testing.T) {
	mux := goahttp.NewMuxer()
	var routed []*http.Request
	connserver.MountListGoogleAdsConversionActionsHandler(mux, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		routed = append(routed, r)
	}))
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		req := httptest.NewRequest(m, "/projects/cncf/google-ads/conversion-actions", nil)
		req.Header.Set("Authorization", "Bearer t")
		mux.ServeHTTP(httptest.NewRecorder(), req)
	}
	if len(routed) != 1 || routed[0].Method != http.MethodGet {
		methods := make([]string, 0, len(routed))
		for _, r := range routed {
			methods = append(methods, r.Method)
		}
		t.Fatalf("methods routed = %v, want GET only", methods)
	}
	p, err := connserver.DecodeListGoogleAdsConversionActionsRequest(mux, goahttp.RequestDecoder)(routed[0])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.ProjectID != "cncf" {
		t.Errorf("project_id = %q, want cncf", p.ProjectID)
	}
}

// decodeAudiences runs a real audiences request through the generated HTTP decoder. routed
// reports whether the handler was reached at all for method.
func decodeAudiences(t *testing.T, method string, query url.Values) (routed bool, typ string, q *string, err error) {
	t.Helper()
	mux := goahttp.NewMuxer()
	var got *http.Request
	connserver.MountListGoogleAdsAudiencesHandler(mux, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r }))
	req := httptest.NewRequest(method, "/projects/cncf/google-ads/audiences?"+query.Encode(), nil)
	req.Header.Set("Authorization", "Bearer t")
	mux.ServeHTTP(httptest.NewRecorder(), req)
	if got == nil {
		return false, "", nil, nil
	}
	p, derr := connserver.DecodeListGoogleAdsAudiencesRequest(mux, goahttp.RequestDecoder)(got)
	if derr != nil {
		return true, "", nil, derr
	}
	if p.ProjectID != "cncf" {
		t.Errorf("project_id = %q, want cncf", p.ProjectID)
	}
	return true, p.Type, p.Q, nil
}

// The audience picker (#496) is mounted for GET only, so no write method reaches it.
func TestAudiencesRoute_IsGetOnly(t *testing.T) {
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if routed, _, _, _ := decodeAudiences(t, m, url.Values{"type": {"audience"}}); routed {
			t.Errorf("%s was routed, want GET only", m)
		}
	}
	routed, typ, q, err := decodeAudiences(t, http.MethodGet, url.Values{"type": {"user_interest"}, "q": {strings.Repeat("東", 100)}})
	if !routed || err != nil || typ != "user_interest" || q == nil || len([]rune(*q)) != 100 {
		t.Fatalf("routed=%v err=%v type=%q q=%v, want a within-contract request decoded", routed, err, typ, q)
	}
	if _, _, q, err = decodeAudiences(t, http.MethodGet, url.Values{"type": {"audience"}}); err != nil || q != nil {
		t.Errorf("an omitted q must decode as absent: q=%v err=%v", q, err)
	}
}

func TestAudiencesDecoder_RefusesWhatTheServiceWouldRefuse(t *testing.T) {
	cases := map[string]url.Values{
		"no type":         {},
		"unknown type":    {"type": {"user_list"}},
		"case-folded":     {"type": {"Audience"}},
		"blank q":         {"type": {"audience"}, "q": {"   "}},
		"101-character q": {"type": {"audience"}, "q": {strings.Repeat("東", 101)}},
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := decodeAudiences(t, http.MethodGet, v); err == nil {
				t.Error("decoder accepted a request the contract forbids")
			}
		})
	}
}
