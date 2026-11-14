// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/meta"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

const metaVideoAssetID = "6f1c2d3e-4a5b-4c7d-8e9f-0a1b2c3d4e5f"

// TestMeta_PreflightCreate_VideoVariants: a videoAssetId is a synchronous 400 naming the flag while
// META_VIDEO_ENABLED is not exactly "true"; with it on, a non-uuid id and an image alongside are
// 400s too, and a well-formed video variant passes.
func TestMeta_PreflightCreate_VideoVariants(t *testing.T) {
	for _, v := range []string{"", "TRUE", "1"} {
		t.Run("flag="+v, func(t *testing.T) {
			t.Setenv(constants.EnvMetaVideoEnabled, v)
			runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
				{name: "no video", config: `{"metaConfig":{"variants":[{"headline":"K","primaryText":"J"}]}}`},
				{name: "blank video", config: `{"metaConfig":{"variants":[{"videoAssetId":"  "}]}}`},
				{name: "video", config: `{"metaConfig":{"variants":[{"videoAssetId":"` + metaVideoAssetID + `"}]}}`,
					want: "metaConfig.variants[].videoAssetId is disabled on this deployment (META_VIDEO_ENABLED is not \"true\")"},
			})
		})
	}
	t.Run("flag on", func(t *testing.T) {
		t.Setenv(constants.EnvMetaVideoEnabled, "true")
		runBiddingPreflight(t, &MetaDispatcher{}, model.ProviderMetaAds, []biddingPreflightCase{
			{name: "video", config: `{"metaConfig":{"variants":[{"videoAssetId":"` + metaVideoAssetID + `"}]}}`},
			{name: "not a uuid", config: `{"metaConfig":{"variants":[{},{"videoAssetId":"clip.mp4"}]}}`,
				want: `metaConfig.variants[1].videoAssetId: "clip.mp4" is not a valid asset id`},
			{name: "image url alongside", config: `{"metaConfig":{"variants":[{"videoAssetId":"` + metaVideoAssetID + `","imageUrl":"https://cdn.example.org/a.png"}]}}`,
				want: "a video variant carries no imageUrl or imageAssetId"},
			{name: "image asset alongside", config: `{"metaConfig":{"variants":[{"videoAssetId":"` + metaVideoAssetID + `","imageAssetId":"` + metaVideoAssetID + `"}]}}`,
				want: "a video variant carries no imageUrl or imageAssetId"},
		})
	})
}

// TestMeta_DispatchVideoRefusalsBeforeAnySpend: Dispatch re-checks the gate (a path that skipped the
// pre-check), and a video slot naming an image asset is refused before anything is created, with
// the claim released.
func TestMeta_DispatchVideoRefusalsBeforeAnySpend(t *testing.T) {
	for _, tc := range []struct {
		name, flag string
		asset      *model.CreativeAsset
		want       string
	}{
		{"flag off", "false", &model.CreativeAsset{Bytes: []byte("....ftypisom"), MimeType: model.MimeTypeMP4}, "META_VIDEO_ENABLED"},
		{"image asset in the video slot", "true", &model.CreativeAsset{Bytes: []byte("\x89PNG"), MimeType: model.MimeTypePNG}, "as its video, but it is not an MP4 video"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvMetaVideoEnabled, tc.flag)
			var mutations int32
			srv := countingMetaServer(t, &mutations)
			defer srv.Close()
			d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
				meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }))
			d.SetCreativeAssetRepo(&fakeCreativeAssets{asset: tc.asset})
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, metaConfigWithVariant(`"videoAssetId":"`+metaVideoAssetID+`"`))
			assertPreSpendRefusal(t, camp, err, atomic.LoadInt32(&mutations), tc.want)
		})
	}
}

// TestMeta_DispatchVideoAssetBecomesAVideoAd: with the flag on, a stored MP4 is resolved scoped to
// the brief, its bytes reach /advideos, the creative carries video_data, and the persisted result
// names the video.
func TestMeta_DispatchVideoAssetBecomesAVideoAd(t *testing.T) {
	t.Setenv(constants.EnvMetaVideoEnabled, "true")
	videoBytes := []byte("\x00\x00\x00\x18ftypisom-FAKEVIDEO")
	var (
		mu         sync.Mutex
		uploadBody []byte
		creative   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/thumbnails"):
			_, _ = io.WriteString(w, `{"data":[{"uri":"https://scontent.example/t.jpg","is_preferred":true}]}`)
		case r.Method == http.MethodGet && r.URL.Query().Get("fields") == "status":
			_, _ = io.WriteString(w, `{"status":{"video_status":"ready"}}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1,"currency":"USD"}`)
		case strings.HasSuffix(r.URL.Path, "/advideos"):
			uploadBody, _ = io.ReadAll(r.Body)
			_, _ = io.WriteString(w, `{"id":"9100000000001"}`)
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case strings.HasSuffix(r.URL.Path, "/adcreatives"):
			b, _ := io.ReadAll(r.Body)
			creative = string(b)
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case strings.HasSuffix(r.URL.Path, "/ads"):
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	assets := &fakeCreativeAssets{asset: &model.CreativeAsset{ID: metaVideoAssetID, Bytes: videoBytes, MimeType: model.MimeTypeMP4}}
	d := NewMetaDispatcher(fakeConnReader{conn: activeMetaConn(goodMetaCreds)}, identityEncryptor{},
		meta.WithBaseURL(srv.URL), meta.WithClock(func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }),
		meta.WithVideoPolling(time.Millisecond, 5*time.Millisecond))
	d.SetCreativeAssetRepo(assets)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderMetaAds, metaConfigWithVariant(`"videoAssetId":"`+metaVideoAssetID+`"`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Fatalf("status = %q, want %q", camp.Status, campaignStatusCreated)
	}
	if got := assets.calls; len(got) != 1 || got[0] != "cncf/brief-1/"+metaVideoAssetID {
		t.Fatalf("asset lookup = %v, want the brief-scoped read", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if !bytes.Contains(uploadBody, videoBytes) {
		t.Fatal("the /advideos upload does not carry the stored video's bytes")
	}
	if !strings.Contains(creative, `"video_id":"9100000000001"`) || strings.Contains(creative, "link_data") || strings.Contains(creative, "FAKEVIDEO") {
		t.Fatalf("creative body = %s; want video_data naming the uploaded video and no bytes", creative)
	}
	var res meta.CampaignResult
	if err := json.Unmarshal(camp.Result, &res); err != nil || res.VideoIDs[metaVideoAssetID] != "9100000000001" {
		t.Fatalf("persisted result VideoIDs = %v (%v), want the asset mapped to the video id", res.VideoIDs, err)
	}
	if !strings.Contains(string(camp.ConfigSnapshot), metaVideoAssetID) || strings.Contains(string(camp.ConfigSnapshot), "FAKEVIDEO") {
		t.Fatalf("snapshot = %s; want the asset id kept and no bytes", camp.ConfigSnapshot)
	}
}
