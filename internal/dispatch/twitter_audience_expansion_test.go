// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// twitterLineItemServer answers a full create, keeping the line-item POST's query and counting
// every request.
func twitterLineItemServer(t *testing.T) (*TwitterDispatcher, func() string, *atomic.Int64) {
	t.Helper()
	var mu sync.Mutex
	var lineItemQuery string
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`))
		case strings.HasSuffix(r.URL.Path, "campaigns"):
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case strings.HasSuffix(r.URL.Path, "line_items"):
			mu.Lock()
			lineItemQuery = r.URL.RawQuery
			mu.Unlock()
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case strings.HasSuffix(r.URL.Path, "promoted_tweets"):
			_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	d := NewTwitterDispatcher(fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(srv.URL), twitter.WithWriteDelay(0))
	return d, func() string { mu.Lock(); defer mu.Unlock(); return lineItemQuery }, &calls
}

// Gap 11 (#564): audienceExpansion sits behind its own default-off flag (a synchronous 400 naming
// it, and refused again at dispatch before any X call), is refused synchronously when malformed,
// and reaches the new line item as audience_expansion.
func TestTwitterAudienceExpansion(t *testing.T) {
	t.Setenv(constants.EnvTwitterAudienceExpansionEnabled, "")
	d, lineItemQuery, calls := twitterLineItemServer(t)
	requireConfigInvalid(t, d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(`,"audienceExpansion":"BROAD"`)),
		constants.EnvTwitterAudienceExpansionEnabled)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, twitterTargetingCfg(`,"audienceExpansion":"BROAD"`))
	requirePreCreate(t, err)
	if n := calls.Load(); n != 0 {
		t.Fatalf("%d X calls with the flag off; want 0", n)
	}
	// A blank value is unset and needs no flag.
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(`,"audienceExpansion":" "`)); err != domain.ErrPreflightNotApplicable {
		t.Fatalf("blank audienceExpansion with the flag off: %v, want not applicable", err)
	}

	t.Setenv(constants.EnvTwitterAudienceExpansionEnabled, "true")
	for extra, want := range map[string]string{
		`,"audienceExpansion":["BROAD"]`:                "twitterConfig.audienceExpansion: must be a string",
		`,"AUDIENCEEXPANSION":1`:                        "twitterConfig.audienceExpansion: must be a string",
		`,"audienceExpansion":"WIDE"`:                   "twitterConfig.audienceExpansion: \"WIDE\" must be BROAD, DEFINED or EXPANDED",
		`,"budgetAmount":"bad","audienceExpansion":"x"`: "twitterConfig.audienceExpansion:",
	} {
		requireConfigInvalid(t, d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(extra)), want)
	}
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderTwitterAds, twitterTargetingCfg(`,"audienceExpansion":null`)); err != domain.ErrPreflightNotApplicable {
		t.Fatalf("null audienceExpansion: %v, want not applicable", err)
	}
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, twitterTargetingCfg(`,"audienceExpansion":"expanded"`)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if q, _ := url.ParseQuery(lineItemQuery()); q.Get("audience_expansion") != "EXPANDED" {
		t.Fatalf("line-item query %q, want audience_expansion=EXPANDED", lineItemQuery())
	}
}
