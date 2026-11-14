// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// TestReddit_ListPixels_Gate: off, the read is the lookup-unsupported sentinel (400) and nothing
// is resolved or sent; on, it lists GET /ad_accounts/{id}/pixels on the create's account.
func TestReddit_ListPixels_Gate(t *testing.T) {
	var hits []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" "+r.URL.Path)
		_, _ = io.WriteString(w, `{"data":[{"id":"t2_123456","name":" Snoo's Pixel ","business_id":"biz1"},{"id":"t2_2","name":"Other"}],"pagination":{"next_url":null}}`)
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()
	d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL))

	for _, flag := range []string{"", "TRUE"} {
		t.Setenv(constants.EnvRedditPixelListEnabled, flag)
		res, called, err := d.ListRedditPixels(context.Background(), "cncf", model.ProviderRedditAds)
		if res != nil || called || !errors.Is(err, domain.ErrTargetingLookupUnsupported) {
			t.Fatalf("flag %q: (%v, %v, %v), want the unsupported sentinel and no call", flag, res, called, err)
		}
	}
	if len(hits) != 0 {
		t.Fatalf("flag off contacted Reddit: %v", hits)
	}

	t.Setenv(constants.EnvRedditPixelListEnabled, "true")
	res, called, err := d.ListRedditPixels(context.Background(), "cncf", model.ProviderRedditAds)
	if err != nil || !called {
		t.Fatalf("flag on: called=%v err=%v", called, err)
	}
	if res.AccountID != "t2_acct" || res.Truncated || len(res.Pixels) != 2 {
		t.Fatalf("result = %+v", res)
	}
	if p := res.Pixels[0]; p.ID != "t2_123456" || p.Name != "Snoo's Pixel" || p.BusinessID != "biz1" {
		t.Errorf("first pixel = %+v", p)
	}
	if len(hits) != 1 || hits[0] != "GET /api/v3/ad_accounts/t2_acct/pixels" {
		t.Errorf("requests = %v, want one GET of the account's pixels", hits)
	}
}

// An answer the client cannot believe is an error with nothing partial, recorded as a call.
func TestReddit_ListPixels_MalformedIsAnError(t *testing.T) {
	t.Setenv(constants.EnvRedditPixelListEnabled, "true")
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{"id":"t2_1"},{"id":"t2_1"}]}`)
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()
	d := NewRedditDispatcher(fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL))
	res, called, err := d.ListRedditPixels(context.Background(), "cncf", model.ProviderRedditAds)
	if res != nil || !called || !errors.Is(err, reddit.ErrPixelsMalformed) {
		t.Fatalf("(%v, %v, %v), want nil, called, ErrPixelsMalformed", res, called, err)
	}
}
