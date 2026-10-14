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

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
)

const liLeakText = "UPSTREAM-LINKEDIN-TEXT-must-not-leak"

// liImageAPI is a fake LinkedIn REST API for the campaign tree and the Images API.
type liImageAPI struct {
	srv *httptest.Server

	mu         sync.Mutex
	calls      []string
	posts      []map[string]any
	initStatus int
}

func newLIImageAPI(t *testing.T) *liImageAPI {
	t.Helper()
	a := &liImageAPI{}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		a.calls = append(a.calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images"):
			if a.initStatus != 0 {
				w.WriteHeader(a.initStatus)
				_, _ = io.WriteString(w, liLeakText)
				return
			}
			_, _ = io.WriteString(w, `{"value":{"uploadUrl":"`+a.srv.URL+`/dms-uploads/IMG1/0?ut=sig","image":"urn:li:image:IMG1"}}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/images/"):
			_, _ = io.WriteString(w, `{"status":"AVAILABLE"}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
			var m map[string]any
			_ = json.Unmarshal(body, &m)
			a.posts = append(a.posts, m)
			_, _ = io.WriteString(w, `{"id":"urn:li:share:300"}`)
		case strings.Contains(r.URL.Path, "creatives"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCreative:400"}`)
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *liImageAPI) count(prefix string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, c := range a.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func newLIImageDispatcher(api *liImageAPI, assets creativeAssetReader, reserver *AssetReserver) *LinkedInDispatcher {
	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(api.srv.URL), linkedin.WithClock(clock), linkedin.WithImagePolling(time.Millisecond, time.Second))
	if assets != nil {
		d.SetCreativeAssetRepo(assets)
	}
	d.SetAssetReserver(reserver)
	return d
}

// liImageConfig builds a linkedInConfig whose variants carry the given raw JSON fragments.
func liImageConfig(variants ...string) json.RawMessage {
	return json.RawMessage(`{"linkedInConfig":{"bidding":{"strategy":"manual","bidAmount":1},
		"budgetUsd":100,"startDate":"2099-01-01","endDate":"2099-02-01",
		"geoTargets":[{"label":"United States","urn":"urn:li:geo:103644278"}],
		"targetingProfile":"cloud-native",
		"targetingProfiles":[{"id":"cloud-native","label":"Cloud Native","skills":["urn:li:skill:1"],"groups":["urn:li:group:100"]}],
		"variants":[` + strings.Join(variants, ",") + `]}}`)
}

func liVariant(extra string) string {
	v := `{"introText":"Join us at the event","headline":"KubeCon 2099"`
	if extra != "" {
		v += "," + extra
	}
	return v + "}"
}

func liPNGAsset(t *testing.T, w, h int) *scopedCreativeAssets {
	brief := testBrief()
	return &scopedCreativeAssets{
		projectID: brief.ProjectID, briefID: brief.ID, assetID: msAssetID,
		asset: &model.CreativeAsset{ID: msAssetID, MimeType: model.MimeTypePNG, Bytes: msTestPNG(t, w, h)},
	}
}

func isNoUpstreamCreate(err error) bool {
	var nuc interface{ NoUpstreamCreate() bool }
	return err != nil && errors.As(err, &nuc) && nuc.NoUpstreamCreate()
}

// TestLinkedInImage_HappyPath: a stored asset named by two variants (one spelled in upper case)
// is uploaded once, used as both dark posts' thumbnail, and recorded under its canonical id in
// the result and the config snapshot.
func TestLinkedInImage_HappyPath(t *testing.T) {
	api := newLIImageAPI(t)
	assets := liPNGAsset(t, 1200, 627)
	d := newLIImageDispatcher(api, assets, nil)
	cfg := liImageConfig(liVariant(`"imageAssetId":"`+msAssetID+`"`), liVariant(`"imageAssetId":"`+strings.ToUpper(msAssetID)+`"`))

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want %q", camp.Status, campaignStatusCreated)
	}
	if got := api.count("POST /images"); got != 1 {
		t.Errorf("initializeUpload calls = %d, want 1 for one asset shared by two variants", got)
	}
	if got := api.count("PUT "); got != 1 {
		t.Errorf("PUTs = %d, want 1", got)
	}
	if len(api.posts) != 2 {
		t.Fatalf("dark posts = %d, want 2", len(api.posts))
	}
	for i, p := range api.posts {
		content, _ := p["content"].(map[string]any)
		article, _ := content["article"].(map[string]any)
		if article["thumbnail"] != "urn:li:image:IMG1" {
			t.Errorf("post %d thumbnail = %v, want urn:li:image:IMG1", i, article["thumbnail"])
		}
	}
	var res struct {
		ImageURNs map[string]string `json:"imageUrns"`
	}
	if err := json.Unmarshal(camp.Result, &res); err != nil || res.ImageURNs[msAssetID] != "urn:li:image:IMG1" {
		t.Errorf("result imageUrns = %v (err %v), want %s -> urn:li:image:IMG1", res.ImageURNs, err, msAssetID)
	}
	if strings.Contains(string(camp.ConfigSnapshot), strings.ToUpper(msAssetID)) {
		t.Error("the config snapshot must store the canonical asset id, not the caller's spelling")
	}
}

// TestLinkedInImage_RefusedBeforeAnythingIsCreated: every asset defect is a pre-create error
// (the claim is released), sends no request to LinkedIn, and an oversized asset is refused on
// its recorded size without its bytes ever being loaded.
func TestLinkedInImage_RefusedBeforeAnythingIsCreated(t *testing.T) {
	other := testBrief()
	cases := []struct {
		name    string
		assets  func(t *testing.T) *scopedCreativeAssets
		variant string
		noLoad  bool
	}{
		{"malformed id", func(t *testing.T) *scopedCreativeAssets { return liPNGAsset(t, 1200, 627) }, liVariant(`"imageAssetId":"not-a-uuid"`), true},
		{"asset of another brief", func(t *testing.T) *scopedCreativeAssets {
			a := liPNGAsset(t, 1200, 627)
			a.briefID = other.ID + "-other"
			return a
		}, liVariant(`"imageAssetId":"` + msAssetID + `"`), true},
		{"oversized asset is never loaded", func(t *testing.T) *scopedCreativeAssets {
			a := liPNGAsset(t, 1200, 627)
			a.sizeOverride = linkedin.MaxImageBytes + 1
			return a
		}, liVariant(`"imageAssetId":"` + msAssetID + `"`), true},
		{"both imageUrn and imageAssetId", func(t *testing.T) *scopedCreativeAssets { return liPNGAsset(t, 1200, 627) },
			liVariant(`"imageAssetId":"` + msAssetID + `","imageUrn":"urn:li:image:X"`), true},
		{"image below LinkedIn's minimum", func(t *testing.T) *scopedCreativeAssets { return liPNGAsset(t, 300, 200) },
			liVariant(`"imageAssetId":"` + msAssetID + `"`), false},
		{"empty stored bytes", func(t *testing.T) *scopedCreativeAssets {
			a := liPNGAsset(t, 1200, 627)
			a.sizeOverride = 1
			a.asset.Bytes = nil
			return a
		}, liVariant(`"imageAssetId":"` + msAssetID + `"`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newLIImageAPI(t)
			assets := tc.assets(t)
			reserver := NewAssetReserver(10<<20, time.Millisecond)
			d := newLIImageDispatcher(api, assets, reserver)
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, liImageConfig(tc.variant))
			if camp != nil || !isNoUpstreamCreate(err) {
				t.Fatalf("want a NoUpstreamCreate refusal, got camp=%v err=%v", camp, err)
			}
			if n := api.count(""); n != 0 {
				t.Errorf("%d request(s) reached LinkedIn; a refused asset must send none", n)
			}
			if tc.noLoad && assets.loads() != 0 {
				t.Errorf("the asset bytes were loaded %d time(s); this refusal must happen before any load", assets.loads())
			}
			// The reservation was released: the whole budget is available again.
			if rel, ok := reserver.reserve(context.Background(), 10<<20); !ok {
				t.Error("a refused dispatch leaked its asset reservation")
			} else {
				rel()
			}
		})
	}

	t.Run("store not configured", func(t *testing.T) {
		api := newLIImageAPI(t)
		d := newLIImageDispatcher(api, nil, nil)
		_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, liImageConfig(liVariant(`"imageAssetId":"`+msAssetID+`"`)))
		if !isNoUpstreamCreate(err) || !strings.Contains(err.Error(), "not configured") {
			t.Fatalf("err = %v, want a pre-create 'not configured' refusal", err)
		}
	})
}

// TestLinkedInImage_UploadFailureRetainsClaimAndDegrades: once the campaign exists an image
// failure is NOT a pre-create error. The campaign is returned created_degraded with zero
// creatives, the claim is retained (so no re-dispatch can upload again), the Steps say NOT
// uploaded, and no upstream text reaches them.
func TestLinkedInImage_UploadFailureRetainsClaimAndDegrades(t *testing.T) {
	api := newLIImageAPI(t)
	api.initStatus = http.StatusForbidden
	reserver := NewAssetReserver(10<<20, time.Millisecond)
	d := newLIImageDispatcher(api, liPNGAsset(t, 1200, 627), reserver)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, liImageConfig(liVariant(`"imageAssetId":"`+msAssetID+`"`)))
	if err == nil || camp == nil {
		t.Fatalf("want a retained partial (campaign + error), got camp=%v err=%v", camp, err)
	}
	if isNoUpstreamCreate(err) {
		t.Error("an image failure after the campaign exists must RETAIN the claim, not release it")
	}
	if camp.PlatformCampaignID != "200" || camp.Status != campaignStatusCreatedDegraded {
		t.Errorf("campaign = %q status %q, want 200 created_degraded", camp.PlatformCampaignID, camp.Status)
	}
	if got := api.count("POST /images"); got != 1 {
		t.Errorf("initializeUpload sent %d times, want exactly 1", got)
	}
	if len(api.posts) != 0 {
		t.Error("no dark post may be created when its image failed")
	}
	if !strings.Contains(string(camp.Result), "NOT uploaded") {
		t.Errorf("result steps must say NOT uploaded: %s", camp.Result)
	}
	if strings.Contains(string(camp.Result)+err.Error(), liLeakText) {
		t.Error("upstream response text leaked into the result or error")
	}
	if rel, ok := reserver.reserve(context.Background(), 10<<20); !ok {
		t.Error("the asset reservation was not released when the dispatch returned")
	} else {
		rel()
	}
}
