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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	goahttp "goa.design/goa/v3/http"
)

// decodeMetaTargetingSearch runs a real targeting-search request through the generated HTTP
// decoder (#550).
func decodeMetaTargetingSearch(t *testing.T, query url.Values) (kind, q string, err error) {
	t.Helper()
	mux := goahttp.NewMuxer()
	decode := connserver.DecodeSearchMetaTargetingOptionsRequest(mux, goahttp.RequestDecoder)
	var routed *http.Request
	connserver.MountSearchMetaTargetingOptionsHandler(mux, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { routed = r }))
	req := httptest.NewRequest(http.MethodGet, "/projects/cncf/meta-ads/targeting-search?"+query.Encode(), nil)
	mux.ServeHTTP(httptest.NewRecorder(), req)
	if routed == nil {
		t.Fatal("search-meta-targeting-options was not routed")
	}
	p, derr := decode(routed)
	if derr != nil {
		return "", "", derr
	}
	return p.Type, p.Query, nil
}

// The design's type enum is exactly the kinds the meta package serves, and its query bounds are
// MinTargetingQueryLen to MaxTargetingQueryLen characters.
func TestMetaTargetingSearchDecoder_MirrorsThePlatform(t *testing.T) {
	for _, k := range meta.TargetingSearchKinds() {
		if _, _, err := decodeMetaTargetingSearch(t, url.Values{"type": {k}, "query": {"go"}}); err != nil {
			t.Errorf("decoder refuses type %s, which the platform serves: %v", k, err)
		}
	}
	kind, q, err := decodeMetaTargetingSearch(t, url.Values{"type": {"interest"}, "query": {strings.Repeat("é", meta.MaxTargetingQueryLen)}})
	if err != nil || kind != "interest" || len([]rune(q)) != meta.MaxTargetingQueryLen {
		t.Errorf("a %d-character query: type=%q len=%d err=%v", meta.MaxTargetingQueryLen, kind, len([]rune(q)), err)
	}
	if _, _, err := decodeMetaTargetingSearch(t, url.Values{"type": {"behavior"}, "query": {strings.Repeat("é", meta.MinTargetingQueryLen)}}); err != nil {
		t.Errorf("a %d-character query was refused: %v", meta.MinTargetingQueryLen, err)
	}
	if _, _, err := decodeMetaTargetingSearch(t, url.Values{"type": {"work_position"}, "query": {"  C++ (dev) & R&D  "}}); err != nil {
		t.Errorf("an ordinary query was refused: %v", err)
	}
}

func TestMetaTargetingSearchDecoder_RefusesWhatTheServiceWouldRefuse(t *testing.T) {
	cases := map[string]url.Values{
		"no type":             {"query": {"go"}},
		"no query":            {"type": {"interest"}},
		"unknown type":        {"type": {"interests"}, "query": {"go"}},
		"blank query":         {"type": {"interest"}, "query": {"   "}},
		"1-character query":   {"type": {"interest"}, "query": {"g"}},
		"101-character query": {"type": {"interest"}, "query": {strings.Repeat("é", meta.MaxTargetingQueryLen+1)}},
		"newline":             {"type": {"interest"}, "query": {"go\nlang"}},
		"NUL":                 {"type": {"interest"}, "query": {"go\x00lang"}},
		"DEL":                 {"type": {"interest"}, "query": {"go\x7flang"}},
		"C1 control":          {"type": {"interest"}, "query": {"go\u0085lang"}},
	}
	for name, v := range cases {
		if _, _, err := decodeMetaTargetingSearch(t, v); err == nil {
			t.Errorf("%s: the decoder accepted %v", name, v)
		}
	}
}
