// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package apivalidation

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	connsrv "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_connections/server"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/googleads"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	goahttp "goa.design/goa/v3/http"
)

// TestMonitorAccountIDPatterns_MatchPlatformValidators guards against the two account_id
// CHARSET checks for the account-monitor endpoints — the design-layer Pattern, exercised here
// through the actual generated decoder (DecodeMonitor*AccountRequest, routed through a real
// goahttp.Muxer exactly as the running server would), and the platform client's own runtime
// check (googleads.ValidateCustomerID, linkedin.ValidateAccountID, meta.ValidateAccountID,
// reddit.ValidateAccountID) — drifting apart silently. The two exist for different reasons
// (Goa rejects a malformed id at the transport before dispatch; the client re-checks because a
// non-HTTP caller skips Goa entirely — see each Validate*'s doc comment) but must accept and
// reject the same ids on charset, or one layer's "valid" becomes the other's runtime error.
//
// Charset only, deliberately: the design attributes also carry MaxLength(64), but none of the
// four platform Validate* helpers bounds length at all, so the two layers already disagree for
// an id of 65+ otherwise-valid characters. That is not a live defect — the design layer is the
// stricter, outer one, so an HTTP caller is refused before a non-HTTP caller's dispatcher-level
// check ever runs — but it means this test's cases stay short enough that MaxLength never
// enters into the comparison; it is not a length-drift guard.
//
// Driving the real decoder (rather than a hand-copied regexp literal) means a regenerated
// design/connection.go Pattern is picked up automatically: this test cannot go stale relative to
// the generated code the way a copied literal could. See internal/apivalidation/doc.go: tests in
// this package exercise generated validators from outside the generated tree.
func TestMonitorAccountIDPatterns_MatchPlatformValidators(t *testing.T) {
	// Every case includes leading/trailing whitespace variants: the platform validators must
	// not trim before matching, because the design-layer decoder never does — a validator that
	// trims accepts an id (e.g. " t2_abc") the design Pattern rejects, which is exactly the kind
	// of silent divergence this test exists to catch.
	t.Run("google ads", func(t *testing.T) {
		decodeOK := monitorDecoderChecker(t, "/connection-google-ads/account-monitor",
			connsrv.MountMonitorGoogleAdsAccountHandler, connsrv.DecodeMonitorGoogleAdsAccountRequest)
		cases := []string{"8666746580", "0", "", "12-34", "abc123", "8666746580 ", " 8666746580", "123abc", "8666746580/", "\t8666746580", "8666746580\n"}
		for _, id := range cases {
			designOK := decodeOK(id)
			platformOK := googleads.ValidateCustomerID(id) == nil
			if designOK != platformOK {
				t.Errorf("account_id %q: design decoder accepts=%v, googleads.ValidateCustomerID accepts=%v — the two shape checks have drifted apart", id, designOK, platformOK)
			}
		}
	})

	t.Run("linkedin", func(t *testing.T) {
		decodeOK := monitorDecoderChecker(t, "/connection-linkedin-ads/account-monitor",
			connsrv.MountMonitorLinkedinAdsAccountHandler, connsrv.DecodeMonitorLinkedinAdsAccountRequest)
		cases := []string{"512345678", "0", "", "12-34", "abc123", "512345678 ", " 512345678", "123abc", " "}
		for _, id := range cases {
			designOK := decodeOK(id)
			platformOK := linkedin.ValidateAccountID(id) == nil
			if designOK != platformOK {
				t.Errorf("account_id %q: design decoder accepts=%v, linkedin.ValidateAccountID accepts=%v — the two shape checks have drifted apart", id, designOK, platformOK)
			}
		}
	})

	t.Run("meta", func(t *testing.T) {
		decodeOK := monitorDecoderChecker(t, "/connection-meta-ads/account-monitor",
			connsrv.MountMonitorMetaAdsAccountHandler, connsrv.DecodeMonitorMetaAdsAccountRequest)
		cases := []string{"act_193556282970417", "act_0", "", "193556282970417", "act_abc", "act_193556282970417 ", " act_193556282970417", "act_"}
		for _, id := range cases {
			designOK := decodeOK(id)
			platformOK := meta.ValidateAccountID(id) == nil
			if designOK != platformOK {
				t.Errorf("account_id %q: design decoder accepts=%v, meta.ValidateAccountID accepts=%v — the two shape checks have drifted apart", id, designOK, platformOK)
			}
		}
	})

	t.Run("reddit", func(t *testing.T) {
		decodeOK := monitorDecoderChecker(t, "/connection-reddit-ads/account-monitor",
			connsrv.MountMonitorRedditAdsAccountHandler, connsrv.DecodeMonitorRedditAdsAccountRequest)
		cases := []string{"t2_gv9wtbfa", "T2_ABC123", "", "t2/../abc", "t2 gv9wtbfa", "t2-gv9wtbfa", "12345", " t2_gv9wtbfa", "t2_gv9wtbfa ", "\tt2_gv9wtbfa"}
		for _, id := range cases {
			designOK := decodeOK(id)
			platformOK := reddit.ValidateAccountID(id) == nil
			if designOK != platformOK {
				t.Errorf("account_id %q: design decoder accepts=%v, reddit.ValidateAccountID accepts=%v — the two shape checks have drifted apart", id, designOK, platformOK)
			}
		}
	})
}

// monitorDecoderChecker routes a GET request for the given account-monitor path through a real
// goahttp.Muxer, exactly as the running server would, so mux.Vars(r) resolves {project_id} the
// way it does in production — decoding a raw httptest request without routing would silently
// skip that resolution. It returns a func reporting whether a given account_id decodes without
// a validation error (days is fixed at a valid 30 so only account_id varies).
func monitorDecoderChecker[P any](
	t *testing.T,
	path string,
	mount func(goahttp.Muxer, http.Handler),
	decodeFn func(goahttp.Muxer, func(*http.Request) goahttp.Decoder) func(*http.Request) (P, error),
) func(accountID string) bool {
	t.Helper()
	mux := goahttp.NewMuxer()
	decode := decodeFn(mux, goahttp.RequestDecoder)

	var routed *http.Request
	mount(mux, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		routed = r
	}))

	return func(accountID string) bool {
		routed = nil
		q := url.Values{"account_id": {accountID}, "days": {"30"}}
		mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet,
			"/projects/cncf"+path+"?"+q.Encode(), nil))
		if routed == nil {
			t.Fatalf("account_id %q: request was not routed; the decoder would not see path params", accountID)
		}
		_, err := decode(routed)
		return err == nil
	}
}
