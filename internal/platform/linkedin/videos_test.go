// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

const vidLeakText = "UPSTREAM-VIDEO-TEXT-must-not-leak"

// testMP4 builds an n-byte buffer that opens with an MP4 ftyp box (all ValidateVideo reads).
func testMP4(n int) []byte {
	b := make([]byte, n)
	binary.BigEndian.PutUint32(b, 16)
	copy(b[4:], "ftypisom")
	return b
}

// videoAPI is a fake LinkedIn REST API for the campaign tree and the Videos API. Each knob
// changes one answer, so a test can break exactly one step.
type videoAPI struct {
	srv *httptest.Server

	mu           sync.Mutex
	calls        []string
	instructions string // the uploadInstructions JSON array; defaults to two parts
	initStatus   int
	putStatus    int
	finStatus    int
	pollStatus   []string
	pollCalls    int
	campaign     map[string]any
	posts        []map[string]any
	finBody      map[string]any
}

func newVideoAPI(t *testing.T) *videoAPI {
	t.Helper()
	a := &videoAPI{}
	a.srv = httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *videoAPI) twoParts(size int) string {
	half := size / 2
	return fmt.Sprintf(`[{"uploadUrl":"%[1]s/dms-uploads/V/1","firstByte":0,"lastByte":%[2]d},{"uploadUrl":"%[1]s/dms-uploads/V/2","firstByte":%[3]d,"lastByte":%[4]d}]`,
		a.srv.URL, half-1, half, size-1)
}

func (a *videoAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	a.calls = append(a.calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/dms-uploads/"):
		if a.putStatus != 0 {
			w.WriteHeader(a.putStatus)
			_, _ = io.WriteString(w, vidLeakText)
			return
		}
		w.Header().Set("ETag", "etag-"+strings.TrimPrefix(r.URL.Path, "/dms-uploads/V/"))
	case r.Method == http.MethodPost && r.URL.Path == "/videos" && r.URL.Query().Get("action") == "initializeUpload":
		if a.initStatus != 0 {
			w.WriteHeader(a.initStatus)
			_, _ = io.WriteString(w, vidLeakText)
			return
		}
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		req, _ := m["initializeUploadRequest"].(map[string]any)
		size, _ := req["fileSizeBytes"].(float64)
		instr := a.instructions
		if instr == "" {
			instr = a.twoParts(int(size))
		}
		_, _ = io.WriteString(w, `{"value":{"video":"urn:li:video:V1","uploadToken":"t","uploadInstructions":`+instr+`}}`)
	case r.Method == http.MethodPost && r.URL.Path == "/videos" && r.URL.Query().Get("action") == "finalizeUpload":
		_ = json.Unmarshal(body, &a.finBody)
		if a.finStatus != 0 {
			w.WriteHeader(a.finStatus)
			_, _ = io.WriteString(w, vidLeakText)
		}
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/videos/"):
		st := "AVAILABLE"
		if len(a.pollStatus) > 0 {
			st = a.pollStatus[min(a.pollCalls, len(a.pollStatus)-1)]
		}
		a.pollCalls++
		_, _ = io.WriteString(w, `{"status":"`+st+`"}`)
	case r.Method == http.MethodGet:
		_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
	case strings.Contains(r.URL.Path, "adCampaignGroups"):
		_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
	case strings.Contains(r.URL.Path, "adCampaigns"):
		_ = json.Unmarshal(body, &a.campaign)
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
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
	}
}

func (a *videoAPI) count(prefix string) int {
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

func (a *videoAPI) client() *Client {
	return NewClient(Credentials{AccessToken: "tok-1"}, testConfig(), WithBaseURL(a.srv.URL), WithClock(fixedClock()),
		WithVideoPolling(time.Millisecond, 200*time.Millisecond))
}

func videoCampaignInput(variants ...CreativeVariant) CampaignInput {
	in := imageCampaignInput(nil, variants...)
	in.Videos = map[string]VideoUpload{"vid-a": {Data: testMP4(MinVideoBytes + 10), MimeType: "video/mp4"}}
	return in
}

func videoVariant() CreativeVariant {
	return CreativeVariant{IntroText: "Intro", Headline: "Headline", VideoAssetID: "vid-a"}
}

func TestValidateVideo(t *testing.T) {
	cases := []struct {
		name string
		in   VideoUpload
		ok   bool
	}{
		{"mp4 at the minimum", VideoUpload{Data: testMP4(MinVideoBytes), MimeType: "video/mp4"}, true},
		{"empty", VideoUpload{MimeType: "video/mp4"}, false},
		{"below the minimum", VideoUpload{Data: testMP4(MinVideoBytes - 1), MimeType: "video/mp4"}, false},
		{"image mime", VideoUpload{Data: testMP4(MinVideoBytes), MimeType: "image/png"}, false},
		{"no ftyp", VideoUpload{Data: make([]byte, MinVideoBytes), MimeType: "video/mp4"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateVideo(tc.in)
			if (err == nil) != tc.ok {
				t.Fatalf("ValidateVideo = %v, want ok=%v", err, tc.ok)
			}
			if err != nil && !errors.Is(err, ErrVideoInvalid) {
				t.Errorf("error %v does not wrap ErrVideoInvalid", err)
			}
		})
	}
}

// TestCreateCampaign_Video_HappyPath pins the client-level wire shape: SINGLE_VIDEO on the
// campaign, the Video name segment, the finalize part ids in order, and the video post body.
func TestCreateCampaign_Video_HappyPath(t *testing.T) {
	api := newVideoAPI(t)
	res, err := api.client().CreateCampaign(context.Background(), videoCampaignInput(videoVariant(), videoVariant()))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if api.campaign["format"] != "SINGLE_VIDEO" {
		t.Errorf("campaign format = %v, want SINGLE_VIDEO", api.campaign["format"])
	}
	if !strings.Contains(res.CampaignName, " | Video | ") {
		t.Errorf("campaign name = %q, want the Video Ad Format segment", res.CampaignName)
	}
	if n := api.count("POST /videos?action=initializeUpload"); n != 1 {
		t.Errorf("initializeUpload = %d, want 1 for one shared asset", n)
	}
	fin, _ := api.finBody["finalizeUploadRequest"].(map[string]any)
	if ids, _ := fin["uploadedPartIds"].([]any); len(ids) != 2 || ids[0] != "etag-1" || ids[1] != "etag-2" {
		t.Errorf("uploadedPartIds = %v, want [etag-1 etag-2]", fin["uploadedPartIds"])
	}
	if len(api.posts) != 2 || res.CreativeCount != 2 {
		t.Fatalf("posts = %d, creatives = %d, want 2 and 2", len(api.posts), res.CreativeCount)
	}
	content, _ := api.posts[0]["content"].(map[string]any)
	media, _ := content["media"].(map[string]any)
	if media["id"] != "urn:li:video:V1" || media["title"] != "Headline" || api.posts[0]["contentCallToActionLabel"] != "REGISTER" {
		t.Errorf("video post = %v", api.posts[0])
	}
	if res.VideoURNs["vid-a"] != "urn:li:video:V1" || res.ImageURNs != nil {
		t.Errorf("result URNs: video %v image %v", res.VideoURNs, res.ImageURNs)
	}
}

// TestCreateCampaign_ImageCampaignUnchanged pins that a config without videos keeps its old
// wire shape: no campaign format, the Static name segment, an article post.
func TestCreateCampaign_ImageCampaignUnchanged(t *testing.T) {
	api := newVideoAPI(t)
	in := imageCampaignInput(nil, CreativeVariant{IntroText: "Intro", Headline: "Headline"})
	res, err := api.client().CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if _, has := api.campaign["format"]; has {
		t.Errorf("an image campaign sent format %v; it must leave the format to LinkedIn as before", api.campaign["format"])
	}
	if !strings.Contains(res.CampaignName, " | Static | ") {
		t.Errorf("campaign name = %q, want Static", res.CampaignName)
	}
	content, _ := api.posts[0]["content"].(map[string]any)
	if _, ok := content["article"]; !ok || api.posts[0]["contentLandingPage"] != nil {
		t.Errorf("image post = %v, want an article and no landing page field", api.posts[0])
	}
	if api.count("POST /videos") != 0 {
		t.Error("an image campaign called the Videos API")
	}
}

// TestCreateCampaign_Video_RefusedBeforeAnyRequest: every input defect is a preflight refusal
// with no request sent.
func TestCreateCampaign_Video_RefusedBeforeAnyRequest(t *testing.T) {
	mixed := videoCampaignInput(videoVariant(), CreativeVariant{IntroText: "Intro", Headline: "Headline"})
	mixedFirstImage := videoCampaignInput(CreativeVariant{IntroText: "Intro", Headline: "Headline"}, videoVariant())
	both := videoVariant()
	both.ImageURN = "urn:li:image:X"
	missing := videoCampaignInput(videoVariant())
	missing.Videos = nil
	tooSmall := videoCampaignInput(videoVariant())
	tooSmall.Videos["vid-a"] = VideoUpload{Data: testMP4(100), MimeType: "video/mp4"}
	for name, in := range map[string]CampaignInput{
		"video then image":   mixed,
		"image then video":   mixedFirstImage,
		"video and image":    videoCampaignInput(both),
		"video not supplied": missing,
		"video too small":    tooSmall,
	} {
		t.Run(name, func(t *testing.T) {
			api := newVideoAPI(t)
			res, err := api.client().CreateCampaign(context.Background(), in)
			if _, refused := domain.AsPreflightRefusal(err); res != nil || !refused {
				t.Fatalf("want a preflight refusal, got res=%v err=%v", res, err)
			}
			if n := api.count(""); n != 0 {
				t.Errorf("%d request(s) sent", n)
			}
		})
	}
}

// TestCreateCampaign_Video_BadUploadPlanSendsNoBytes: an instruction set that does not tile the
// file, or points off LinkedIn, uploads nothing and never finalizes; the URN is still recorded.
func TestCreateCampaign_Video_BadUploadPlanSendsNoBytes(t *testing.T) {
	size := MinVideoBytes + 10
	for name, instr := range map[string]func(a *videoAPI) string{
		"empty": func(*videoAPI) string { return `[]` },
		"gap": func(a *videoAPI) string {
			return fmt.Sprintf(`[{"uploadUrl":"%s/dms-uploads/V/1","firstByte":0,"lastByte":9},{"uploadUrl":"%s/dms-uploads/V/2","firstByte":11,"lastByte":%d}]`, a.srv.URL, a.srv.URL, size-1)
		},
		"short": func(a *videoAPI) string {
			return fmt.Sprintf(`[{"uploadUrl":"%s/dms-uploads/V/1","firstByte":0,"lastByte":%d}]`, a.srv.URL, size-2)
		},
		"past end": func(a *videoAPI) string {
			return fmt.Sprintf(`[{"uploadUrl":"%s/dms-uploads/V/1","firstByte":0,"lastByte":%d}]`, a.srv.URL, size)
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := newVideoAPI(t)
			api.instructions = instr(api)
			res, err := api.client().CreateCampaign(context.Background(), videoCampaignInput(videoVariant()))
			if err == nil || res == nil {
				t.Fatalf("want a partial result and an error, got res=%v err=%v", res, err)
			}
			if n := api.count("PUT ") + api.count("POST /videos?action=finalizeUpload") + api.count("POST /posts"); n != 0 {
				t.Errorf("%d PUT/finalize/post request(s) sent for a bad upload plan", n)
			}
			if res.VideoURNs["vid-a"] != "urn:li:video:V1" || res.CampaignID != "200" {
				t.Errorf("partial result = %+v, want the campaign and the minted URN", res)
			}
		})
	}

	// An upload URL on any other host never receives the bearer token or a byte.
	t.Run("off-domain", func(t *testing.T) {
		var mu sync.Mutex
		hits := 0
		evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			hits++
			mu.Unlock()
			w.Header().Set("ETag", "evil")
		}))
		t.Cleanup(evil.Close)
		api := newVideoAPI(t)
		api.instructions = fmt.Sprintf(`[{"uploadUrl":"%s/u","firstByte":0,"lastByte":%d}]`, evil.URL, size-1)
		res, err := api.client().CreateCampaign(context.Background(), videoCampaignInput(videoVariant()))
		if err == nil || res == nil || !strings.Contains(err.Error(), "outside LinkedIn") {
			t.Fatalf("want an 'outside LinkedIn' failure with a partial result, got res=%v err=%v", res, err)
		}
		mu.Lock()
		defer mu.Unlock()
		if hits != 0 || api.count("POST /videos?action=finalizeUpload") != 0 {
			t.Errorf("the untrusted host got %d request(s); finalize sent %d", hits, api.count("POST /videos?action=finalizeUpload"))
		}
	})
}

// TestCreateCampaign_Video_FailureClasses: a refused initializeUpload or part is definite (NOT
// uploaded); a 5xx finalize is UNCONFIRMED; a video still PROCESSING when the wait ends is
// reported. None creates a post, none is retried, and no upstream text leaks.
func TestCreateCampaign_Video_FailureClasses(t *testing.T) {
	cases := []struct {
		name  string
		setup func(a *videoAPI)
		step  string
		sends map[string]int
	}{
		{"initializeUpload refused", func(a *videoAPI) { a.initStatus = http.StatusForbidden }, "NOT uploaded",
			map[string]int{"POST /videos?action=initializeUpload": 1, "PUT ": 0}},
		{"part refused", func(a *videoAPI) { a.putStatus = http.StatusBadRequest }, "NOT uploaded",
			map[string]int{"PUT ": 1, "POST /videos?action=finalizeUpload": 0}},
		{"finalize 5xx", func(a *videoAPI) { a.finStatus = http.StatusInternalServerError }, "UNCONFIRMED",
			map[string]int{"POST /videos?action=finalizeUpload": 1, "GET /videos/": 0}},
		{"still processing", func(a *videoAPI) { a.pollStatus = []string{"PROCESSING"} }, "was not AVAILABLE",
			map[string]int{"POST /videos?action=finalizeUpload": 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newVideoAPI(t)
			tc.setup(api)
			res, err := api.client().CreateCampaign(context.Background(), videoCampaignInput(videoVariant()))
			if err == nil || res == nil || res.CampaignID != "200" {
				t.Fatalf("want a partial result for campaign 200 and an error, got res=%v err=%v", res, err)
			}
			if !strings.Contains(strings.Join(res.Steps, "\n"), tc.step) {
				t.Errorf("steps %q do not contain %q", res.Steps, tc.step)
			}
			for prefix, want := range tc.sends {
				if got := api.count(prefix); got != want {
					t.Errorf("%q sent %d time(s), want %d", prefix, got, want)
				}
			}
			if api.count("POST /posts") != 0 {
				t.Error("a dark post was created for a failed video")
			}
			if strings.Contains(err.Error()+strings.Join(res.Steps, ""), vidLeakText) {
				t.Error("upstream response text leaked")
			}
		})
	}
}
