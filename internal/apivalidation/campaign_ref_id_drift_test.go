// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package apivalidation

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	connsrv "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_connections/server"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	goahttp "goa.design/goa/v3/http"
)

// TestCampaignRefIDPatterns_MatchPlatformValidators routes a request through the real generated
// muxer and decoder for each campaign-ref route LFXV2-2665 added and asserts the design's
// Pattern + MaxLength accept exactly the ids the platform package's ValidateCampaignID accepts —
// the rule the service applies for a non-HTTP caller. Unlike the monitor account-id test, the
// cases DO cross each length bound: both layers state one.
func TestCampaignRefIDPatterns_MatchPlatformValidators(t *testing.T) {
	t.Run("meta", func(t *testing.T) {
		decodeOK := campaignRefDecoderChecker(t, "/meta-ads/campaign-ref",
			connsrv.MountResolveMetaAdsCampaignHandler, connsrv.DecodeResolveMetaAdsCampaignRequest)
		cases := []string{"120210000000000001", "1", "0", "01", "", "12a", " 1", "1 ", "act_1", "-1",
			strings.Repeat("9", 32), strings.Repeat("9", 33)}
		for _, id := range cases {
			if designOK, platformOK := decodeOK(id), meta.ValidateCampaignID(id) == nil; designOK != platformOK {
				t.Errorf("id %q: design accepts=%v, meta.ValidateCampaignID accepts=%v", id, designOK, platformOK)
			}
		}
	})
	t.Run("reddit", func(t *testing.T) {
		decodeOK := campaignRefDecoderChecker(t, "/reddit-ads/campaign-ref",
			connsrv.MountResolveRedditAdsCampaignHandler, connsrv.DecodeResolveRedditAdsCampaignRequest)
		cases := []string{"t2_camp_123", "1234567890", "", "a-b", "a/b", "a.b", " a", "a ", "a?b",
			strings.Repeat("a", 64), strings.Repeat("a", 65)}
		for _, id := range cases {
			if designOK, platformOK := decodeOK(id), reddit.ValidateCampaignID(id) == nil; designOK != platformOK {
				t.Errorf("id %q: design accepts=%v, reddit.ValidateCampaignID accepts=%v", id, designOK, platformOK)
			}
		}
	})
	t.Run("x ads", func(t *testing.T) {
		decodeOK := campaignRefDecoderChecker(t, "/twitter-ads/campaign-ref",
			connsrv.MountResolveTwitterAdsCampaignHandler, connsrv.DecodeResolveTwitterAdsCampaignRequest)
		cases := []string{"8wxyz", "ABC123", "", "a_b", "a-b", "a/b", " a", "a ",
			strings.Repeat("a", 64), strings.Repeat("a", 65)}
		for _, id := range cases {
			if designOK, platformOK := decodeOK(id), twitter.ValidateCampaignID(id) == nil; designOK != platformOK {
				t.Errorf("id %q: design accepts=%v, twitter.ValidateCampaignID accepts=%v", id, designOK, platformOK)
			}
		}
	})
}

// campaignRefDecoderChecker mounts one campaign-ref route on a real goahttp.Muxer and returns a
// function reporting whether its generated decoder accepts a platform_campaign_id. A request
// that is not routed at all fails the test: the route path itself is part of what is pinned.
func campaignRefDecoderChecker[P any](
	t *testing.T,
	path string,
	mount func(goahttp.Muxer, http.Handler),
	decodeFn func(goahttp.Muxer, func(*http.Request) goahttp.Decoder) func(*http.Request) (P, error),
) func(id string) bool {
	t.Helper()
	mux := goahttp.NewMuxer()
	decode := decodeFn(mux, goahttp.RequestDecoder)
	var routed *http.Request
	mount(mux, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { routed = r }))
	return func(id string) bool {
		routed = nil
		q := url.Values{"platform_campaign_id": {id}}
		mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/projects/cncf"+path+"?"+q.Encode(), nil))
		if routed == nil {
			t.Fatalf("id %q: GET /projects/cncf%s was not routed", id, path)
		}
		_, err := decode(routed)
		return err == nil
	}
}
