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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

const carouselCards = `"variants":[
	{"primaryText":"Meet the speakers","headline":"Keynotes","imageUrl":"https://cdn.example.org/k.png"},
	{"headline":"Security track","imageUrl":"https://cdn.example.org/s.png"}]`

// TestMeta_PreflightCreate_Format: format "carousel" is a synchronous 400 naming the flag while
// META_CAROUSEL_ENABLED is not exactly "true"; an unknown or wrong-typed format is a 400 either way;
// with the flag on the carousel's card rules are 400s too.
func TestMeta_PreflightCreate_Format(t *testing.T) {
	for _, v := range []string{"", "TRUE", "1"} {
		t.Run("flag="+v, func(t *testing.T) {
			t.Setenv(constants.EnvMetaCarouselEnabled, v)
			runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
				{name: "absent", config: `{"metaConfig":{"geoTargets":["US"]}}`},
				{name: "single", config: `{"metaConfig":{"format":"Single"}}`},
				{name: "carousel", config: `{"metaConfig":{"format":"carousel",` + carouselCards + `}}`,
					want: "metaConfig.format is disabled on this deployment (META_CAROUSEL_ENABLED is not \"true\")"},
				{name: "unknown", config: `{"metaConfig":{"format":"slideshow"}}`, want: `metaConfig.format: "slideshow" is not one of single, carousel`},
				{name: "numeric", config: `{"metaConfig":{"format":2}}`, want: `"format" must be a string`},
			})
		})
	}
	t.Run("flag on", func(t *testing.T) {
		t.Setenv(constants.EnvMetaCarouselEnabled, "true")
		runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
			{name: "carousel", config: `{"metaConfig":{"format":"carousel",` + carouselCards + `}}`},
			{name: "one card", config: `{"metaConfig":{"format":"carousel","variants":[{"primaryText":"P","headline":"H","imageUrl":"https://cdn.example.org/k.png"}]}}`,
				want: "a carousel needs 2 to 10 variants"},
			{name: "card without image", config: `{"metaConfig":{"format":"carousel","variants":[{"primaryText":"P","headline":"H","imageUrl":"https://cdn.example.org/k.png"},{"headline":"H2"}]}}`,
				want: "metaConfig.variants[1]: a carousel card needs an image"},
		})
	})
}

// TestMeta_DispatchRefusesCarouselWhileFlagIsOff: a path that skipped the pre-check is refused
// pre-create with the claim released, and Meta is never called.
func TestMeta_DispatchRefusesCarouselWhileFlagIsOff(t *testing.T) {
	t.Setenv(constants.EnvMetaCarouselEnabled, "false")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Meta request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","geoTargets":["US"],"format":"carousel",` + carouselCards + `}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	var nuc interface{ NoUpstreamCreate() bool }
	if camp != nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() || !errors.Is(err, domain.ErrCreateConfigInvalid) ||
		!strings.Contains(err.Error(), "META_CAROUSEL_ENABLED") {
		t.Fatalf("Dispatch = %+v, %v; want a NoUpstreamCreate refusal naming META_CAROUSEL_ENABLED", camp, err)
	}
}

// TestMeta_DispatchCarouselIsCreatedNotDegraded: with the flag on, two variants become ONE carousel
// ad, and one ad is the full count — the campaign is created, not created_degraded — with the
// format kept in the snapshot and the result.
func TestMeta_DispatchCarouselIsCreatedNotDegraded(t *testing.T) {
	t.Setenv(constants.EnvMetaCarouselEnabled, "true")
	var (
		mu        sync.Mutex
		creatives []string
		ads       int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1,"currency":"USD"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			b, _ := io.ReadAll(r.Body)
			creatives = append(creatives, string(b))
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			ads++
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
	cfg := json.RawMessage(`{"metaConfig":{"budget":100,"startDate":"2099-01-01","endDate":"2099-02-01","geoTargets":["US"],"format":"Carousel",` + carouselCards + `}}`)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Fatalf("status = %q, want %q: one carousel ad is the full count", camp.Status, campaignStatusCreated)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(creatives) != 1 || ads != 1 || !strings.Contains(creatives[0], `"child_attachments"`) {
		t.Fatalf("creatives=%v ads=%d; want one carousel creative and one ad", creatives, ads)
	}
	var res meta.CampaignResult
	if err := json.Unmarshal(camp.Result, &res); err != nil || res.Format != meta.FormatCarousel || res.AdCount != 1 {
		t.Fatalf("result Format=%q AdCount=%d (%v)", res.Format, res.AdCount, err)
	}
	if !strings.Contains(string(camp.ConfigSnapshot), `"format":"Carousel"`) {
		t.Fatalf("snapshot = %s; want the format kept", camp.ConfigSnapshot)
	}
}
