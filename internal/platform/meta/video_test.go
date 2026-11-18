// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
		{name: "duplicate id keys are unconfirmed", status: "ready", uploadBody: `{"id":"9100000000001","ID":"9100000000002"}`, want: "video upload outcome UNCONFIRMED"},
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

// TestCreateCampaign_VideoVariantWithLeadFormIsRefused: a lead form is attached only to
// link_data's call to action, so a video variant on a lead-form create would run without its form.
// It is a config-invalid refusal with no request sent.
func TestCreateCampaign_VideoVariantWithLeadFormIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request may be sent, got %s %s", r.Method, r.URL.String())
	}))
	defer srv.Close()
	in := metaFieldInput()
	in.Objective = "leads"
	in.LeadFormID = "1234567890"
	in.Variants = []AdVariant{videoVariant("K")}
	var sleeps int32
	res, err := videoClient(srv.URL, &sleeps).CreateCampaign(context.Background(), in)
	var refusal *domain.PreflightRefusal
	if res != nil || !errors.As(err, &refusal) || !errors.Is(err, domain.ErrCreateConfigInvalid) ||
		!strings.Contains(err.Error(), "cannot be combined with leadFormId") {
		t.Fatalf("CreateCampaign = %+v, %v; want a config-invalid refusal naming leadFormId", res, err)
	}
}

// TestCreateCampaign_VideoWaitLeavesTimeForTheOtherAds: the readiness wait is clamped to the
// caller's deadline less videoDeadlineReserve, so a video still processing fails only its own
// variant and the image variant's ad is still created, rather than the deadline cutting the whole
// create short mid-wait (local review, PR for #550).
func TestCreateCampaign_VideoWaitLeavesTimeForTheOtherAds(t *testing.T) {
	vs := newVideoServer(t, "processing")
	srv := httptest.NewServer(vs.handler())
	defer srv.Close()
	var sleeps int32
	c := NewClient(Credentials{AccessToken: "tok"}, AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
		WithBaseURL(srv.URL), WithClock(fixedMetaClock()), WithVideoPolling(10*time.Second, 60*time.Second),
		withSleepFn(func(context.Context, time.Duration) error { atomic.AddInt32(&sleeps, 1); return nil }))
	ctx, cancel := context.WithTimeout(context.Background(), videoDeadlineReserve+DefaultRequestTimeout+time.Second)
	defer cancel()
	in := metaFieldInput()
	in.Variants = []AdVariant{
		{PrimaryText: "Join us", Headline: "Image", ImageURL: "https://cdn.example.org/a.png"},
		videoVariant("Keynote"),
	}
	res, err := c.CreateCampaign(ctx, in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v (a not-ready video is a degraded success)", err)
	}
	// The upload gate needs a request timeout (30 s) beyond the reserve, so about 31 s of wait is
	// left: at most 3 polls of 10 s, where the unclamped 60 s wait would take 6.
	if sleeps > 3 {
		t.Fatalf("readiness wait slept %d times; with about 31 s left after the reserve it may poll at most 3 times", sleeps)
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if res.AdCount != 1 || vs.ads != 1 || len(res.Ads) != 1 || res.Ads[0].Variant != 1 {
		t.Fatalf("AdCount=%d ads=%d Ads=%+v; want the image variant's ad only", res.AdCount, vs.ads, res.Ads)
	}
	if steps := strings.Join(res.Steps, "\n"); !strings.Contains(steps, "was not ready within") || !strings.Contains(steps, "no creative or ad was created for variant 2") {
		t.Fatalf("steps %q do not report the not-ready video on variant 2", steps)
	}
}

// TestCreateCampaign_VideoWaitIsBoundedByTheClock: slow status reads spend the readiness wait too.
// With a 3 s wait and every read taking 1.5 s, the poll stops when the clock passes the wait rather
// than after its full count of rounds, and the image variant still gets its ad (local review).
func TestCreateCampaign_VideoWaitIsBoundedByTheClock(t *testing.T) {
	vs := newVideoServer(t, "processing")
	inner := vs.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Query().Get("fields") == "status" {
			time.Sleep(1500 * time.Millisecond)
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	base, start := fixedMetaClock()(), time.Now()
	clock := func() time.Time { return base.Add(time.Since(start)) }
	var sleeps int32
	c := NewClient(Credentials{AccessToken: "tok"}, AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
		WithBaseURL(srv.URL), WithClock(clock), WithVideoPolling(time.Second, 3*time.Second),
		withSleepFn(func(context.Context, time.Duration) error { atomic.AddInt32(&sleeps, 1); return nil }))
	in := metaFieldInput()
	in.Variants = []AdVariant{
		{PrimaryText: "Join us", Headline: "Image", ImageURL: "https://cdn.example.org/a.png"},
		videoVariant("Keynote"),
	}
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v (a not-ready video is a degraded success)", err)
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if vs.statusReads > 3 {
		t.Fatalf("read the status %d times; a 3 s wait of 1.5 s reads allows at most 3", vs.statusReads)
	}
	if res.AdCount != 1 || len(res.Ads) != 1 || res.Ads[0].Variant != 1 {
		t.Fatalf("AdCount=%d Ads=%+v; want the image variant's ad only", res.AdCount, res.Ads)
	}
}

// TestStepSafeErrText: a non-envelope APIError body is dropped, wrapped or not; a parsed Graph
// message is kept.
func TestStepSafeErrText(t *testing.T) {
	raw := &APIError{StatusCode: 502, Method: "GET", Path: "/9100000000001", Message: "<html>PROXY-ECHO-MARKER</html>"}
	for _, err := range []error{raw, fmt.Errorf("status read: %w", raw)} {
		if got := stepSafeErrText(err, 300); strings.Contains(got, "PROXY-ECHO-MARKER") || !strings.Contains(got, "(502)") {
			t.Fatalf("stepSafeErrText(%v) = %q; want the status without the body", err, got)
		}
	}
	graph := &APIError{StatusCode: 400, Method: "GET", Path: "/9", Message: "Unsupported get request", EnvelopeParsed: true}
	if got := stepSafeErrText(graph, 300); !strings.Contains(got, "Unsupported get request") {
		t.Fatalf("stepSafeErrText dropped a Graph message: %q", got)
	}
}

// TestCreateCampaign_VideoStepsDropNonGraphErrorBodies drives the two video Step sites that render an
// upstream error (a failed status read, a failed thumbnail read) with a non-Graph body: Steps name
// the 502 and never carry the body text.
func TestCreateCampaign_VideoStepsDropNonGraphErrorBodies(t *testing.T) {
	for _, tc := range []struct {
		name, status, fail, want string
	}{
		{"status read", "processing", "status", "last status read failed"},
		{"thumbnail read", "ready", "thumbnails", "has no usable auto-generated thumbnail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vs := newVideoServer(t, tc.status)
			inner := vs.handler()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				isStatus := r.Method == http.MethodGet && r.URL.Query().Get("fields") == "status"
				isThumbs := r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/thumbnails")
				if (tc.fail == "status" && isStatus) || (tc.fail == "thumbnails" && isThumbs) {
					w.WriteHeader(http.StatusBadGateway)
					_, _ = io.WriteString(w, "<html>PROXY-ECHO-MARKER</html>")
					return
				}
				inner.ServeHTTP(w, r)
			}))
			defer srv.Close()
			var sleeps int32
			in := metaFieldInput()
			in.Variants = []AdVariant{videoVariant("Keynote")}
			res, err := videoClient(srv.URL, &sleeps).CreateCampaign(context.Background(), in)
			if err != nil {
				t.Fatalf("CreateCampaign: %v (a video failure is a degraded success)", err)
			}
			steps := strings.Join(res.Steps, "\n")
			if !strings.Contains(steps, tc.want) || !strings.Contains(steps, "(502)") || strings.Contains(steps, "PROXY-ECHO-MARKER") {
				t.Fatalf("steps %q: want %q and the 502, without the body", steps, tc.want)
			}
		})
	}
}

// TestCreateCampaign_VideoReadsStopWhenTheSharedWaitEnds: with several distinct videos and slow
// status reads, only a round's first read gets the minimum read time; once the shared wait has
// passed no further read starts, so the videos cannot each spend a fresh floor (PR #653 review).
func TestCreateCampaign_VideoReadsStopWhenTheSharedWaitEnds(t *testing.T) {
	vs := newVideoServer(t, "processing")
	inner := vs.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Query().Get("fields") == "status" {
			time.Sleep(1500 * time.Millisecond)
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	base, start := fixedMetaClock()(), time.Now()
	clock := func() time.Time { return base.Add(time.Since(start)) }
	var sleeps int32
	c := NewClient(Credentials{AccessToken: "tok"}, AccountConfig{AccountID: "act_777", PageID: "987654321", CurrencyOffset: 100},
		WithBaseURL(srv.URL), WithClock(clock), WithVideoPolling(time.Second, time.Second),
		withSleepFn(func(context.Context, time.Duration) error { atomic.AddInt32(&sleeps, 1); return nil }))
	in := metaFieldInput()
	in.Variants = nil
	for _, name := range []string{"A", "B", "C"} {
		v := videoVariant(name)
		v.VideoAssetID = ""
		v.VideoBytes = append(append([]byte{}, testVideo...), name...)
		in.Variants = append(in.Variants, v)
	}
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v (not-ready videos are a degraded success)", err)
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if vs.uploads != 3 || vs.statusReads != 1 {
		t.Fatalf("uploads=%d statusReads=%d; want 3 uploads and ONE status read inside a 1 s wait of 1.5 s reads", vs.uploads, vs.statusReads)
	}
	if steps := strings.Join(res.Steps, "\n"); !strings.Contains(steps, "its status was not read before the wait ended") {
		t.Fatalf("steps %q do not report the unread videos", steps)
	}
}

// TestCreateCampaign_VideoCreativeFailureStepIsSafe: a video creative refused with a non-Graph
// body that echoes the request reaches Steps without the body and without Meta's signed thumbnail
// URL (PR #653 review).
func TestCreateCampaign_VideoCreativeFailureStepIsSafe(t *testing.T) {
	vs := newVideoServer(t, "ready")
	vs.thumbs = `{"data":[{"uri":"https://scontent.example/t2.jpg?oh=THUMB-SIG","is_preferred":true}]}`
	inner := vs.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/adcreatives") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "<html>PROXY-ECHO-MARKER image_url=https://scontent.example/t2.jpg?oh=THUMB-SIG</html>")
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	var sleeps int32
	in := metaFieldInput()
	in.Variants = []AdVariant{videoVariant("Keynote")}
	res, err := videoClient(srv.URL, &sleeps).CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	steps := strings.Join(res.Steps, "\n")
	if res.AdCount != 0 || !strings.Contains(steps, "Ad 1 failed") || strings.Contains(steps, "PROXY-ECHO-MARKER") || strings.Contains(steps, "THUMB-SIG") {
		t.Fatalf("AdCount=%d steps %q: want the failure without the body or the thumbnail signature", res.AdCount, steps)
	}
}

// TestCreateCampaign_VideoUploadsStopBeforeTheDeadlineReserve: an upload starts only while the
// caller's deadline holds a full request timeout beyond videoDeadlineReserve. With a deadline that
// fits one slow upload, the second video is not sent and fails only its variant; the image and the
// first video still get their ads (PR #653 review).
func TestCreateCampaign_VideoUploadsStopBeforeTheDeadlineReserve(t *testing.T) {
	vs := newVideoServer(t, "ready")
	inner := vs.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/advideos") {
			time.Sleep(time.Second)
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	var sleeps int32
	ctx, cancel := context.WithTimeout(context.Background(), videoDeadlineReserve+DefaultRequestTimeout+500*time.Millisecond)
	defer cancel()
	second := videoVariant("Second")
	second.VideoAssetID = ""
	second.VideoBytes = append(append([]byte{}, testVideo...), "second"...)
	in := metaFieldInput()
	in.Variants = []AdVariant{
		{PrimaryText: "Join us", Headline: "Image", ImageURL: "https://cdn.example.org/a.png"},
		videoVariant("Keynote"),
		second,
	}
	res, err := videoClient(srv.URL, &sleeps).CreateCampaign(ctx, in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v (a skipped upload is a degraded success)", err)
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if vs.uploads != 1 || res.AdCount != 2 {
		t.Fatalf("uploads=%d AdCount=%d; want one upload and the image and first video ads", vs.uploads, res.AdCount)
	}
	if steps := strings.Join(res.Steps, "\n"); !strings.Contains(steps, "too little of the provider call's deadline was left") || !strings.Contains(steps, "no creative or ad was created for variant 3") {
		t.Fatalf("steps %q do not report the skipped upload on variant 3", steps)
	}
}

// TestPrepareVideos_DeadCallerAbortsBeforeTheUploadClamp: a caller whose deadline has already passed
// by the upload stage gets the abort (CreateCampaign then returns the partial with the error), not a
// clamp skip that would turn an all-video create into a created_degraded success (PR #653 review).
func TestPrepareVideos_DeadCallerAbortsBeforeTheUploadClamp(t *testing.T) {
	vs := newVideoServer(t, "ready")
	srv := httptest.NewServer(vs.handler())
	defer srv.Close()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	var sleeps int32
	var steps []string
	out, err := videoClient(srv.URL, &sleeps).prepareVideos(ctx, []AdVariant{videoVariant("Keynote")}, &steps, map[string]string{})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || out != nil {
		t.Fatalf("prepareVideos = %v, %v; want an abort carrying the expired deadline", out, err)
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if vs.uploads != 0 {
		t.Fatalf("uploads=%d; want none for a caller already gone", vs.uploads)
	}
}

// TestCreateCampaign_VideoThumbnailReadsLeaveTimeForTheAds: the thumbnail reads share a budget that
// ends videoAdReserve before the caller's deadline. A thumbnail read that stalls fails only its own
// video's variant, and the image variant still gets its ad, rather than the read running into the
// caller's deadline and aborting the create (PR #653 review).
func TestCreateCampaign_VideoThumbnailReadsLeaveTimeForTheAds(t *testing.T) {
	vs := newVideoServer(t, "ready")
	inner := vs.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/thumbnails") {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer srv.Close()
	var sleeps int32
	c := videoClient(srv.URL, &sleeps)
	c.videoWaitReserve, c.videoAdReserve = 2*time.Second, time.Second
	c.httpClient.Timeout = 0 // no upload gate: the deadline here is far below a request timeout
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	in := metaFieldInput()
	in.Variants = []AdVariant{
		{PrimaryText: "Join us", Headline: "Image", ImageURL: "https://cdn.example.org/a.png"},
		videoVariant("Keynote"),
	}
	res, err := c.CreateCampaign(ctx, in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v (a stalled thumbnail is a degraded success)", err)
	}
	if res.AdCount != 1 || len(res.Ads) != 1 || res.Ads[0].Variant != 1 {
		t.Fatalf("AdCount=%d Ads=%+v; want the image variant's ad", res.AdCount, res.Ads)
	}
	if steps := strings.Join(res.Steps, "\n"); !strings.Contains(steps, "ran out of the time left before the ads") || !strings.Contains(steps, "no creative or ad was created for variant 2") {
		t.Fatalf("steps %q do not report the thumbnail budget on variant 2", steps)
	}
}

// TestCreateCampaign_VideoReadsRefuseUncheckedBodies: the status and thumbnail reads decide a paid
// creative, so a 2xx body with duplicate or case-folded keys is refused, never read last-wins: a
// "Status" ready behind a "status" processing does not make the video ready, and a second "Data"
// list does not supply a thumbnail (PR #653 review).
func TestCreateCampaign_VideoReadsRefuseUncheckedBodies(t *testing.T) {
	for _, tc := range []struct {
		name, path, body, want string
	}{
		{"status", "status", `{"id":"9100000000001","status":{"video_status":"processing"},"Status":{"video_status":"ready"}}`, "was not ready within"},
		{"thumbnails", "thumbnails", `{"data":[],"Data":[{"uri":"https://scontent.example/t.jpg","is_preferred":true}]}`, "has no usable auto-generated thumbnail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vs := newVideoServer(t, "ready")
			inner := vs.handler()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				isStatus := r.Method == http.MethodGet && r.URL.Query().Get("fields") == "status"
				isThumbs := r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/thumbnails")
				if (tc.path == "status" && isStatus) || (tc.path == "thumbnails" && isThumbs) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, tc.body)
					return
				}
				inner.ServeHTTP(w, r)
			}))
			defer srv.Close()
			var sleeps int32
			in := metaFieldInput()
			in.Variants = []AdVariant{videoVariant("Keynote")}
			res, err := videoClient(srv.URL, &sleeps).CreateCampaign(context.Background(), in)
			if err != nil {
				t.Fatalf("CreateCampaign: %v", err)
			}
			vs.mu.Lock()
			defer vs.mu.Unlock()
			if res.AdCount != 0 || len(vs.creatives) != 0 {
				t.Fatalf("AdCount=%d creatives=%d; want no creative from an unchecked body", res.AdCount, len(vs.creatives))
			}
			if steps := strings.Join(res.Steps, "\n"); !strings.Contains(steps, tc.want) {
				t.Fatalf("steps %q do not contain %q", steps, tc.want)
			}
		})
	}
}

// TestCreateCampaign_VideoUploadErrorReadsOnlyACheckedEnvelope: a refused upload whose body carries
// two error objects has no envelope read, so neither message reaches Steps (PR #653 review).
func TestCreateCampaign_VideoUploadErrorReadsOnlyACheckedEnvelope(t *testing.T) {
	vs := newVideoServer(t, "ready")
	vs.uploadStatus = http.StatusBadRequest
	vs.uploadBody = `{"error":{"message":"bad video","code":100},"Error":{"message":"PRIVATE-ECHO-MARKER","code":100}}`
	srv := httptest.NewServer(vs.handler())
	defer srv.Close()
	var sleeps int32
	in := metaFieldInput()
	in.Variants = []AdVariant{videoVariant("Keynote")}
	res, err := videoClient(srv.URL, &sleeps).CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	steps := strings.Join(res.Steps, "\n")
	if strings.Contains(steps, "PRIVATE-ECHO-MARKER") || strings.Contains(steps, "bad video") || !strings.Contains(steps, "no creative or ad was created for variant 1") {
		t.Fatalf("steps %q: want the failure without either envelope message", steps)
	}
}
