// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package apivalidation

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	briefsrv "github.com/linuxfoundation/lfx-v2-campaign-service/gen/http/lfx_v2_campaign_service_briefs/server"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	goahttp "goa.design/goa/v3/http"
)

// TestMetaAdSetIDPattern_MatchesPlatformValidator routes toggle-meta-ad-set-status through the
// real generated muxer and decoder and asserts the design's ad_set_id Pattern + MaxLength accept
// exactly the ids meta.ValidateAdSetID accepts — the rule the dispatcher repeats before an id is
// interpolated into a Graph path (LFXV2-2665).
func TestMetaAdSetIDPattern_MatchesPlatformValidator(t *testing.T) {
	mux := goahttp.NewMuxer()
	decode := briefsrv.DecodeToggleMetaAdSetStatusRequest(mux, goahttp.RequestDecoder)
	var routed *http.Request
	briefsrv.MountToggleMetaAdSetStatusHandler(mux, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { routed = r }))
	const brief = "6f9619ff-8b86-d011-b42d-00c04fc964ff"
	cases := []string{"120210000000000888", "1", "0", "01", "12a", " 1", "1 ", "act_1", "-1",
		strings.Repeat("9", 32), strings.Repeat("9", 33)}
	for _, id := range cases {
		routed = nil
		path := "/projects/cncf/briefs/" + brief + "/campaigns/" + brief + "/meta-ad-sets/" + url.PathEscape(id) + "/status"
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"status":"PAUSED"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("If-Match", `"1"`)
		mux.ServeHTTP(httptest.NewRecorder(), req)
		if routed == nil {
			t.Fatalf("id %q: POST %s was not routed", id, path)
		}
		_, err := decode(routed)
		if designOK, platformOK := err == nil, meta.ValidateAdSetID(id) == nil; designOK != platformOK {
			t.Errorf("id %q: design accepts=%v (%v), meta.ValidateAdSetID accepts=%v", id, designOK, err, platformOK)
		}
	}
}
