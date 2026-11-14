// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// adPerVariantCfg is an authored-post Reddit config with n variants.
func adPerVariantCfg(n int, extra string) json.RawMessage {
	vs := make([]string, n)
	for i := range vs {
		vs[i] = fmt.Sprintf(`{"headline":"Headline %d"}`, i+1)
	}
	return conversionEventCfg(`,"imageUrl":"https://events.example/banner.png","variants":[` + strings.Join(vs, ",") + `]` + extra)
}

func TestRedditAdPerVariant_PreflightRefusesOverTheCapOrABlankHeadline(t *testing.T) {
	t.Setenv(constants.EnvRedditAdPerVariantEnabled, "true")
	d := NewRedditDispatcher(fakeConnReader{}, identityEncryptor{})
	err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, adPerVariantCfg(6, ``))
	requireConversionEventRefusal(t, err, "redditConfig.variants: 6 variants supplied")
	blank := conversionEventCfg(`,"imageUrl":"https://events.example/banner.png","variants":[{"headline":"A"},{"headline":" "}]`)
	err = d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, blank)
	requireConversionEventRefusal(t, err, "redditConfig.variants[2]: headline is required")
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, adPerVariantCfg(5, ``)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("five variants must pass, got %v", err)
	}
}

// Off, or on a create that does not author its post, the variants are the one-ad create's and
// are not checked by these rules.
func TestRedditAdPerVariant_NotCheckedOffOrWithoutAnAuthoredPost(t *testing.T) {
	d := NewRedditDispatcher(fakeConnReader{}, identityEncryptor{})
	for _, flag := range []string{"", "TRUE"} {
		t.Setenv(constants.EnvRedditAdPerVariantEnabled, flag)
		if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, adPerVariantCfg(6, ``)); !errors.Is(err, domain.ErrPreflightNotApplicable) {
			t.Fatalf("flag %q: want the one-ad create admitted, got %v", flag, err)
		}
	}
	t.Setenv(constants.EnvRedditAdPerVariantEnabled, "true")
	postURL := conversionEventCfg(`,"postUrl":"https://www.reddit.com/r/kubernetes/comments/abc123/x/","variants":[{"headline":"1"},{"headline":"2"},{"headline":"3"},{"headline":"4"},{"headline":"5"},{"headline":"6"}]`)
	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderRedditAds, postURL); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("an existing postUrl is the one-ad create, got %v", err)
	}
}

// variantDispatchAPI is a Reddit API stand-in counting requests and failing one variant's ad.
type variantDispatchAPI struct {
	mu       sync.Mutex
	requests int
	ads      int
	failAd   int
}

func (a *variantDispatchAPI) dispatcher(t *testing.T) *RedditDispatcher {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.requests++
		id := "x"
		switch {
		case strings.HasSuffix(r.URL.Path, "/posts"):
			id = fmt.Sprintf("t3_p%d", a.requests)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			id = "cmp_1"
		case strings.HasSuffix(r.URL.Path, "/ad_groups"):
			id = "ag_1"
		case strings.HasSuffix(r.URL.Path, "/ads"):
			a.ads++
			if a.ads == a.failAd {
				http.Error(w, `{"error":"no"}`, http.StatusBadRequest)
				return
			}
			id = fmt.Sprintf("ad_%d", a.ads)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": id}})
	}))
	t.Cleanup(api.Close)
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tok.Close)
	return NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
		reddit.WithNowFunc(func() time.Time { return time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC) }),
	)
}

// Dispatch re-checks the cap before any Reddit request, for a path that skipped PreflightCreate.
func TestRedditAdPerVariant_DispatchRefusesBeforeAnyRequest(t *testing.T) {
	t.Setenv(constants.EnvRedditAdPerVariantEnabled, "true")
	a := &variantDispatchAPI{}
	camp, err := a.dispatcher(t).Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, adPerVariantCfg(6, ``))
	if camp != nil {
		t.Fatalf("no campaign may be returned, got %+v", camp)
	}
	requirePreCreate(t, err)
	requireConversionEventRefusal(t, err, "at most 5")
	if a.requests != 0 {
		t.Fatalf("%d Reddit requests were sent before the refusal", a.requests)
	}
}

// Every ad id is persisted on the row; a failed variant leaves the created ads in place and the
// row created_degraded.
func TestRedditAdPerVariant_DispatchRecordsEveryAdAndDegradesOnAFailedVariant(t *testing.T) {
	t.Setenv(constants.EnvRedditAdPerVariantEnabled, "true")
	for _, tc := range []struct {
		failAd     int
		wantIDs    []string
		wantStatus string
	}{
		{0, []string{"ad_1", "ad_2", "ad_3"}, campaignStatusCreated},
		{3, []string{"ad_1", "ad_2"}, campaignStatusCreatedDegraded},
	} {
		a := &variantDispatchAPI{failAd: tc.failAd}
		camp, err := a.dispatcher(t).Dispatch(context.Background(), testBrief(), model.ProviderRedditAds, adPerVariantCfg(3, ``))
		if err != nil {
			t.Fatalf("Dispatch: %v", err)
		}
		if camp.Status != tc.wantStatus {
			t.Errorf("failAd %d: status = %q, want %q", tc.failAd, camp.Status, tc.wantStatus)
		}
		_, ids := redditChildIDs(camp)
		if !reflect.DeepEqual(ids, tc.wantIDs) {
			t.Errorf("failAd %d: persisted ad ids = %v, want %v", tc.failAd, ids, tc.wantIDs)
		}
	}
}

// The toggle reads adIds and cascades over every ad; a row written before adIds existed still
// toggles its one adId.
func TestRedditAdPerVariant_ToggleCascadesOverEveryAd(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"data":{"id":"x"}}`))
	}))
	defer api.Close()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	defer tok.Close()
	d := NewRedditDispatcher(
		fakeConnReader{conn: activeRedditConn(goodRedditCreds)}, identityEncryptor{},
		reddit.WithBaseURL(api.URL+"/api/v3"), reddit.WithTokenURL(tok.URL),
	)
	camp := toggleCampaign("t3_c", "t5_ag", "t6_a1")
	camp.Result = []byte(`{"adGroupId":"t5_ag","adId":"t6_a1","adIds":["t6_a1","t6_a2"]}`)
	if err := d.ToggleStatus(context.Background(), "proj", model.ProviderRedditAds, camp, model.CampaignRunActive); err != nil {
		t.Fatalf("ToggleStatus: %v", err)
	}
	want := []string{
		"/api/v3/ad_accounts/t2_acct/ads/t6_a1",
		"/api/v3/ad_accounts/t2_acct/ads/t6_a2",
		"/api/v3/ad_accounts/t2_acct/ad_groups/t5_ag",
		"/api/v3/ad_accounts/t2_acct/campaigns/t3_c",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("activate PATCHes = %v, want %v", paths, want)
	}
	legacy := toggleCampaign("t3_c", "t5_ag", "t6_only")
	if ag, ids := redditChildIDs(legacy); ag != "t5_ag" || !reflect.DeepEqual(ids, []string{"t6_only"}) {
		t.Fatalf("legacy row child ids = %q %v", ag, ids)
	}
}
