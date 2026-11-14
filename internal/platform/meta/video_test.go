// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// videoServer is a Meta stub for the video flow. statuses is the sequence of video_status values the
// readiness poll reads (the last repeats); uploadStatus/uploadBody answer /advideos.
type videoServer struct {
	t            *testing.T
	mu           sync.Mutex
	statuses     []string
	statusReads  int
	uploads      int32
	uploadBody   string
	uploadStatus int
	uploadFile   []byte
	uploadName   string
	uploadPartCT string
	thumbs       string
	creatives    []map[string]any
	ads          int32
}

func newVideoServer(t *testing.T, statuses ...string) *videoServer {
	return &videoServer{t: t, statuses: statuses, uploadStatus: http.StatusOK, uploadBody: `{"id":"9100000000001"}`,
		thumbs: `{"data":[{"uri":"https://scontent.example/t1.jpg","is_preferred":false},{"uri":"https://scontent.example/t2.jpg","is_preferred":true}]}`}
}

func (s *videoServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/act_777"):
			_, _ = io.WriteString(w, `{"name":"LF Core","account_status":1}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/advideos"):
			atomic.AddInt32(&s.uploads, 1)
			_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			mr := multipart.NewReader(r.Body, params["boundary"])
			if part, err := mr.NextPart(); err == nil {
				s.uploadName = part.FormName() + "/" + part.FileName()
				s.uploadPartCT = part.Header.Get("Content-Type")
				s.uploadFile, _ = io.ReadAll(part)
			}
			w.WriteHeader(s.uploadStatus)
			_, _ = io.WriteString(w, s.uploadBody)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/thumbnails"):
			_, _ = io.WriteString(w, s.thumbs)
		case r.Method == http.MethodGet && r.URL.Query().Get("fields") == "status":
			st := s.statuses[len(s.statuses)-1]
			if s.statusReads < len(s.statuses) {
				st = s.statuses[s.statusReads]
			}
			s.statusReads++
			_, _ = io.WriteString(w, `{"id":"`+strings.TrimPrefix(r.URL.Path, "/")+`","status":{"video_status":"`+st+`"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/campaigns"):
			_, _ = io.WriteString(w, `{"id":"120100000000123"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adsets"):
			_, _ = io.WriteString(w, `{"id":"120200000000456"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adcreatives"):
			s.creatives = append(s.creatives, decodeBody(s.t, r))
			_, _ = io.WriteString(w, `{"id":"creative_1"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ads"):
			atomic.AddInt32(&s.ads, 1)
			_, _ = io.WriteString(w, `{"id":"ad_1"}`)
		default:
			s.t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

var testVideo = []byte("\x00\x00\x00\x18ftypisom-FAKEVIDEO")

func videoVariant(headline string) AdVariant {
	return AdVariant{PrimaryText: "Watch the keynote", Headline: headline, Description: "Cloud native",
		VideoAssetID: "6f1c2d3e-4a5b-4c7d-8e9f-0a1b2c3d4e5f", VideoBytes: testVideo, VideoMIME: MimeTypeVideoMP4}
}

// videoClient polls every millisecond within a three-poll wait and records the sleeps it asks for
// instead of spending them.
func videoClient(srvURL string, sleeps *int32) *Client {
	return NewClient(Credentials{AccessToken: "tok"}, AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
		WithBaseURL(srvURL), WithClock(fixedMetaClock()), WithVideoPolling(time.Millisecond, 3*time.Millisecond),
		withSleepFn(func(context.Context, time.Duration) error { atomic.AddInt32(sleeps, 1); return nil }))
}

// TestCreateCampaign_VideoVariantsShareOneUploadAndCarryVideoData pins the video flow: two variants
// naming the same MP4 upload it ONCE as the multipart `source` file, the shared poll waits until the
// video is ready, the PREFERRED auto-generated thumbnail is the creative's image_url, and each
// creative carries video_data (copy, button and UTM link) instead of link_data.
func TestCreateCampaign_VideoVariantsShareOneUploadAndCarryVideoData(t *testing.T) {
	vs := newVideoServer(t, "processing", "ready")
	srv := httptest.NewServer(vs.handler())
	defer srv.Close()
	var sleeps int32
	in := metaFieldInput()
	in.CallToAction = "WATCH_MORE"
	in.Variants = []AdVariant{videoVariant("Keynote"), videoVariant("Keynote 2")}
	res, err := videoClient(srv.URL, &sleeps).CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if vs.uploads != 1 {
		t.Fatalf("/advideos called %d times, want exactly 1 for two variants naming one video", vs.uploads)
	}
	if vs.uploadName != "source/creative.mp4" || vs.uploadPartCT != MimeTypeVideoMP4 || !bytes.Equal(vs.uploadFile, testVideo) {
		t.Fatalf("upload part = %q (%q), %d bytes; want source/creative.mp4 labelled video/mp4 carrying the video", vs.uploadName, vs.uploadPartCT, len(vs.uploadFile))
	}
	if sleeps != 1 {
		t.Errorf("readiness poll slept %d times, want 1 (processing, then ready)", sleeps)
	}
	if res.AdCount != 2 || len(vs.creatives) != 2 {
		t.Fatalf("AdCount=%d creatives=%d, want 2 and 2", res.AdCount, len(vs.creatives))
	}
	for i, body := range vs.creatives {
		spec, _ := body["object_story_spec"].(map[string]any)
		if _, has := spec["link_data"]; has {
			t.Fatalf("creative %d sent link_data alongside video_data: %v", i+1, spec)
		}
		vd, _ := spec["video_data"].(map[string]any)
		cta, _ := vd["call_to_action"].(map[string]any)
		value, _ := cta["value"].(map[string]any)
		link, _ := value["link"].(string)
		if vd["video_id"] != "9100000000001" || vd["image_url"] != "https://scontent.example/t2.jpg" ||
			vd["message"] != "Watch the keynote" || vd["link_description"] != "Cloud native" || cta["type"] != "WATCH_MORE" ||
			!strings.Contains(link, "utm_content=variant-"+string(rune('1'+i))) {
			t.Fatalf("creative %d video_data = %v", i+1, vd)
		}
	}
	if res.VideoIDs["6f1c2d3e-4a5b-4c7d-8e9f-0a1b2c3d4e5f"] != "9100000000001" || res.Ads[0].VideoID != "9100000000001" {
		t.Fatalf("result does not record the video: VideoIDs=%v Ads=%+v", res.VideoIDs, res.Ads)
	}
}

// TestCreateCampaign_VideoFailuresCreateNoAdAndNeverReupload: a video that never becomes usable
// fails its variants with no creative or ad request, the upload is sent ONCE (never retried, not even
// on a throttle), and a minted video id stays on the result.
func TestCreateCampaign_VideoFailuresCreateNoAdAndNeverReupload(t *testing.T) {
	for _, tc := range []struct {
		name, status, uploadBody, want string
		uploadStatus                   int
		wantVideoID                    bool
	}{
		{name: "still processing when the wait ends", status: "processing", want: "was not ready within 3ms (status \"processing\")", wantVideoID: true},
		{name: "processing error", status: "error", want: "failed Meta's processing", wantVideoID: true},
		{name: "upload 5xx is unconfirmed", status: "ready", uploadStatus: http.StatusInternalServerError, uploadBody: `{}`, want: "video upload outcome UNCONFIRMED"},
		{name: "a throttled upload is not retried", status: "ready", uploadStatus: http.StatusBadRequest,
			uploadBody: `{"error":{"message":"rate limited","code":17}}`, want: "video upload outcome UNCONFIRMED"},
		{name: "a definite 4xx is not uploaded", status: "ready", uploadStatus: http.StatusBadRequest,
			uploadBody: `{"error":{"message":"bad video","code":100}}`, want: "video NOT uploaded"},
		{name: "a 2xx with no id is unconfirmed", status: "ready", uploadBody: `{}`, want: "video upload outcome UNCONFIRMED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vs := newVideoServer(t, tc.status)
			if tc.uploadStatus != 0 {
				vs.uploadStatus = tc.uploadStatus
			}
			if tc.uploadBody != "" {
				vs.uploadBody = tc.uploadBody
			}
			srv := httptest.NewServer(vs.handler())
			defer srv.Close()
			var sleeps int32
			in := metaFieldInput()
			in.Variants = []AdVariant{videoVariant("Keynote")}
			res, err := videoClient(srv.URL, &sleeps).CreateCampaign(context.Background(), in)
			if err != nil {
				t.Fatalf("CreateCampaign: %v (a per-variant failure is a degraded success)", err)
			}
			vs.mu.Lock()
			defer vs.mu.Unlock()
			if vs.uploads != 1 || len(vs.creatives) != 0 || vs.ads != 0 || res.AdCount != 0 {
				t.Fatalf("uploads=%d creatives=%d ads=%d AdCount=%d; want one upload and nothing else", vs.uploads, len(vs.creatives), vs.ads, res.AdCount)
			}
			// The wait is bounded: a 3ms wait polled every 1ms reads the status at most four times
			// (once, then after each of three sleeps), however long Meta keeps processing.
			if tc.status == "processing" && (vs.statusReads != 4 || sleeps != 3) {
				t.Fatalf("readiness poll read the status %d times with %d sleeps, want 4 and 3", vs.statusReads, sleeps)
			}
			if steps := strings.Join(res.Steps, "\n"); !strings.Contains(steps, tc.want) || !strings.Contains(steps, "no creative or ad was created for variant 1") {
				t.Fatalf("steps %q do not report %q", steps, tc.want)
			}
			if got := res.VideoIDs["6f1c2d3e-4a5b-4c7d-8e9f-0a1b2c3d4e5f"] == "9100000000001"; got != tc.wantVideoID {
				t.Fatalf("VideoIDs = %v, want the minted id recorded: %v", res.VideoIDs, tc.wantVideoID)
			}
		})
	}
}

// TestCreateCampaign_VideoCancelReturnsThePartial: a caller that goes away during the readiness wait
// gets the partial result (campaign, ad set, minted video), so the claim is retained and a retry
// never uploads again.
func TestCreateCampaign_VideoCancelReturnsThePartial(t *testing.T) {
	vs := newVideoServer(t, "processing")
	srv := httptest.NewServer(vs.handler())
	defer srv.Close()
	c := NewClient(Credentials{AccessToken: "tok"}, AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
		WithBaseURL(srv.URL), WithClock(fixedMetaClock()), WithVideoPolling(time.Millisecond, time.Second),
		withSleepFn(func(context.Context, time.Duration) error { return context.Canceled }))
	in := metaFieldInput()
	in.Variants = []AdVariant{videoVariant("Keynote")}
	res, err := c.CreateCampaign(context.Background(), in)
	if err == nil || res == nil || res.AdSetID != "120200000000456" || res.VideoIDs["6f1c2d3e-4a5b-4c7d-8e9f-0a1b2c3d4e5f"] != "9100000000001" {
		t.Fatalf("CreateCampaign = %+v, %v; want the partial naming the ad set and the video, with an error", res, err)
	}
}

// TestCreateCampaign_VideoVariantRefusalsSendNothing: a video alongside an image, an unresolved video
// asset and a non-MP4 are config-invalid pre-flight refusals with no request sent.
func TestCreateCampaign_VideoVariantRefusalsSendNothing(t *testing.T) {
	withImage := videoVariant("K")
	withImage.ImageURL = "https://cdn.example.org/a.png"
	withAsset := videoVariant("K")
	withAsset.ImageAssetID = "6f1c2d3e-4a5b-4c7d-8e9f-0a1b2c3d4e5e"
	unresolved := videoVariant("K")
	unresolved.VideoBytes = nil
	notMP4 := videoVariant("K")
	notMP4.VideoMIME = "image/png"
	for _, tc := range []struct {
		name string
		v    AdVariant
		want string
	}{
		{"image url alongside", withImage, "carries no imageUrl or imageAssetId"},
		{"image asset alongside", withAsset, "carries no imageUrl or imageAssetId"},
		{"unresolved asset", unresolved, "was not resolved to stored bytes"},
		{"not an mp4", notMP4, "only video/mp4 videos are supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("no request may be sent, got %s %s", r.Method, r.URL.String())
			}))
			defer srv.Close()
			in := metaFieldInput()
			in.Variants = []AdVariant{tc.v}
			var sleeps int32
			res, err := videoClient(srv.URL, &sleeps).CreateCampaign(context.Background(), in)
			var refusal *domain.PreflightRefusal
			if res != nil || !errors.As(err, &refusal) || !errors.Is(err, domain.ErrCreateConfigInvalid) ||
				!strings.Contains(err.Error(), "metaConfig.variants[0].videoAssetId: ") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CreateCampaign = %+v, %v; want a config-invalid refusal containing %q", res, err, tc.want)
			}
		})
	}
}
