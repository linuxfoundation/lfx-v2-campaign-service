// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const imgLeakText = "UPSTREAM-IMAGE-TEXT-must-not-leak"

func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func testJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// pngHeaderOnly builds a PNG signature plus a valid IHDR for w x h. image.DecodeConfig reads
// only this much, so huge dimensions can be tested without allocating the pixels.
func pngHeaderOnly(w, h uint32) []byte {
	var b bytes.Buffer
	b.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	data := make([]byte, 13)
	binary.BigEndian.PutUint32(data[0:], w)
	binary.BigEndian.PutUint32(data[4:], h)
	data[8], data[9] = 8, 0 // 8-bit grayscale
	chunk := append([]byte("IHDR"), data...)
	_ = binary.Write(&b, binary.BigEndian, uint32(len(data)))
	b.Write(chunk)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return b.Bytes()
}

func TestValidateImage(t *testing.T) {
	cases := []struct {
		name string
		in   ImageUpload
		ok   bool
	}{
		{"landscape png", ImageUpload{Data: testPNG(t, 1200, 627), MimeType: "image/png"}, true},
		{"square jpeg", ImageUpload{Data: testJPEG(t, 400, 400), MimeType: "image/jpeg"}, true},
		{"vertical png", ImageUpload{Data: testPNG(t, 720, 900), MimeType: "image/png"}, true},
		{"landscape at the maximum", ImageUpload{Data: pngHeaderOnly(7680, 4320), MimeType: "image/png"}, true},
		{"empty", ImageUpload{MimeType: "image/png"}, false},
		{"over 5 MB", ImageUpload{Data: make([]byte, MaxImageBytes+1), MimeType: "image/png"}, false},
		{"gif mime", ImageUpload{Data: testPNG(t, 1200, 627), MimeType: "image/gif"}, false},
		{"bytes disagree with mime", ImageUpload{Data: testPNG(t, 1200, 627), MimeType: "image/jpeg"}, false},
		{"undecodable", ImageUpload{Data: []byte("not an image"), MimeType: "image/png"}, false},
		{"landscape too small", ImageUpload{Data: testPNG(t, 639, 360), MimeType: "image/png"}, false},
		{"square too small", ImageUpload{Data: testPNG(t, 359, 359), MimeType: "image/png"}, false},
		{"vertical too short", ImageUpload{Data: testPNG(t, 360, 639), MimeType: "image/png"}, false},
		{"landscape too wide", ImageUpload{Data: pngHeaderOnly(7681, 4000), MimeType: "image/png"}, false},
		{"vertical too wide", ImageUpload{Data: pngHeaderOnly(2431, 4320), MimeType: "image/png"}, false},
		{"square too large", ImageUpload{Data: pngHeaderOnly(4321, 4321), MimeType: "image/png"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateImage(tc.in)
			if tc.ok && err != nil {
				t.Fatalf("ValidateImage: %v, want nil", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("ValidateImage: want an error")
				}
				if !errors.Is(err, ErrImageInvalid) {
					t.Errorf("error %v must wrap ErrImageInvalid", err)
				}
			}
		})
	}
}

// imageAPI is a fake LinkedIn REST API covering the campaign tree and the Images API. It
// records every call, the initializeUpload bodies, the PUTs and each dark post's body.
type imageAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	calls      []string
	initBodies []map[string]any
	puts       []struct {
		auth, ctype, path string
		body              []byte
	}
	posts     []map[string]any
	nextImage int

	initStatus int    // non-zero: initializeUpload answers this status
	initBody   string // non-empty: initializeUpload answers this 2xx body verbatim
	uploadHost string // non-empty: the uploadUrl returned points here instead of the server
	putStatus  int    // non-zero: the PUT answers this status
	pollStatus []string
	pollErrs   []int // per GET: a non-zero entry answers that HTTP status instead
	pollCalls  int
	pollDelay  time.Duration // set before use: every status GET sleeps this long first
}

func newImageAPI(t *testing.T) *imageAPI {
	a := &imageAPI{t: t}
	a.srv = httptest.NewServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *imageAPI) serve(w http.ResponseWriter, r *http.Request) {
	if a.pollDelay > 0 && r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/images/") {
		select {
		case <-time.After(a.pollDelay):
		case <-r.Context().Done():
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	a.calls = append(a.calls, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/dms-uploads/"):
		a.puts = append(a.puts, struct {
			auth, ctype, path string
			body              []byte
		}{r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.URL.Path, body})
		if a.putStatus != 0 {
			w.WriteHeader(a.putStatus)
			_, _ = io.WriteString(w, imgLeakText)
			return
		}
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images") && r.URL.Query().Get("action") == "initializeUpload":
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		a.initBodies = append(a.initBodies, m)
		if a.initStatus != 0 {
			w.WriteHeader(a.initStatus)
			_, _ = io.WriteString(w, `{"message":"`+imgLeakText+`"}`)
			return
		}
		if a.initBody != "" {
			_, _ = io.WriteString(w, a.initBody)
			return
		}
		a.nextImage++
		id := "C4E10AQimg" + string(rune('A'+a.nextImage-1))
		host := a.srv.URL
		if a.uploadHost != "" {
			host = a.uploadHost
		}
		_, _ = io.WriteString(w, `{"value":{"uploadUrlExpiresAt":1,"uploadUrl":"`+host+`/dms-uploads/`+id+`/uploaded-image/0?ut=secret-sig","image":"urn:li:image:`+id+`"}}`)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/images/"):
		i := a.pollCalls
		a.pollCalls++
		if i < len(a.pollErrs) && a.pollErrs[i] != 0 {
			w.WriteHeader(a.pollErrs[i])
			_, _ = io.WriteString(w, imgLeakText)
			return
		}
		st := "AVAILABLE"
		if len(a.pollStatus) > 0 {
			st = a.pollStatus[min(i, len(a.pollStatus)-1)]
		}
		_, _ = io.WriteString(w, `{"id":"`+strings.TrimPrefix(r.URL.Path, "/images/")+`","status":"`+st+`"}`)
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
		http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
	}
}

func (a *imageAPI) count(prefix string) int {
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

func (a *imageAPI) client(opts ...Option) *Client {
	base := []Option{WithBaseURL(a.srv.URL), WithClock(fixedClock()), WithImagePolling(time.Millisecond, 200*time.Millisecond)}
	return NewClient(Credentials{AccessToken: "tok-1"}, testConfig(), append(base, opts...)...)
}

func imageCampaignInput(images map[string]ImageUpload, variants ...CreativeVariant) CampaignInput {
	return CampaignInput{
		ManualCPCBid:     testManualBid, // LFXV2-2665: automatic is refused for this objective
		EventName:        "KubeCon",
		RegistrationURL:  "https://events.example.org/kubecon",
		BudgetUSD:        100,
		StartDate:        "2099-01-01",
		EndDate:          "2099-02-01",
		GeoTargets:       []GeoTarget{{Label: "United States", URN: "urn:li:geo:103644278"}},
		TargetingProfile: "cloud-native",
		Variants:         variants,
		Project:          "CNCF",
		Images:           images,
	}
}

func thumbnail(t *testing.T, post map[string]any) any {
	t.Helper()
	content, _ := post["content"].(map[string]any)
	article, _ := content["article"].(map[string]any)
	return article["thumbnail"]
}

// TestCreateCampaign_StoredImages_UploadOncePerAssetAndUseAsThumbnail is the happy path: two
// variants share asset A and a third names B, so exactly two images are uploaded, each once,
// owned by the connection's organization, and each dark post's thumbnail is its asset's URN.
func TestCreateCampaign_StoredImages_UploadOncePerAssetAndUseAsThumbnail(t *testing.T) {
	api := newImageAPI(t)
	pngA := testPNG(t, 1200, 627)
	jpgB := testJPEG(t, 400, 400)
	in := imageCampaignInput(
		map[string]ImageUpload{"asset-a": {Data: pngA, MimeType: "image/png"}, "asset-b": {Data: jpgB, MimeType: "image/jpeg"}},
		CreativeVariant{IntroText: "Intro one", Headline: "One", ImageAssetID: "asset-a"},
		CreativeVariant{IntroText: "Intro two", Headline: "Two", ImageAssetID: "asset-a"},
		CreativeVariant{IntroText: "Intro three", Headline: "Three", ImageAssetID: "asset-b"},
	)
	res, err := api.client().CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if got := api.count("POST /images"); got != 2 {
		t.Fatalf("initializeUpload calls = %d, want 2 (one per DISTINCT asset)", got)
	}
	if len(api.puts) != 2 {
		t.Fatalf("PUTs = %d, want 2", len(api.puts))
	}
	for _, b := range api.initBodies {
		req, _ := b["initializeUploadRequest"].(map[string]any)
		if req["owner"] != "urn:li:organization:987654321" {
			t.Errorf("initializeUpload owner = %v, want the connection's organization URN", req["owner"])
		}
	}
	if api.puts[0].auth != "Bearer tok-1" || api.puts[0].ctype != "image/png" || !bytes.Equal(api.puts[0].body, pngA) {
		t.Errorf("first PUT: auth=%q ctype=%q bytes-equal=%v; want the bearer, image/png and the stored bytes", api.puts[0].auth, api.puts[0].ctype, bytes.Equal(api.puts[0].body, pngA))
	}
	if api.puts[1].ctype != "image/jpeg" || !bytes.Equal(api.puts[1].body, jpgB) {
		t.Error("second PUT must carry asset B's bytes as image/jpeg")
	}
	wantA, wantB := "urn:li:image:C4E10AQimgA", "urn:li:image:C4E10AQimgB"
	if res.ImageURNs["asset-a"] != wantA || res.ImageURNs["asset-b"] != wantB {
		t.Errorf("ImageURNs = %v, want asset-a=%s asset-b=%s", res.ImageURNs, wantA, wantB)
	}
	if len(api.posts) != 3 {
		t.Fatalf("dark posts = %d, want 3", len(api.posts))
	}
	for i, want := range []string{wantA, wantA, wantB} {
		if got := thumbnail(t, api.posts[i]); got != want {
			t.Errorf("variant-%d thumbnail = %v, want %s", i+1, got, want)
		}
	}
	// Ordering: every image call sits after the campaign create and before the first post.
	var seq []string
	api.mu.Lock()
	for _, c := range api.calls {
		if strings.HasPrefix(c, "GET /adCampaign") || strings.HasPrefix(c, "GET /adAccounts") {
			continue
		}
		seq = append(seq, c)
	}
	api.mu.Unlock()
	camp, firstImg, lastImg, firstPost := -1, -1, -1, -1
	for i, c := range seq {
		switch {
		case strings.Contains(c, "adCampaigns") && camp < 0 && strings.HasPrefix(c, "POST"):
			camp = i
		case strings.Contains(c, "/images") || strings.Contains(c, "/dms-uploads/"):
			if firstImg < 0 {
				firstImg = i
			}
			lastImg = i
		case strings.HasSuffix(c, "/posts") && firstPost < 0:
			firstPost = i
		}
	}
	if camp < 0 || firstImg < camp || lastImg > firstPost {
		t.Errorf("call order %v: images must be uploaded after the campaign and before the first dark post", seq)
	}
}

// TestCreateCampaign_StoredImage_InitializeUploadFailureClasses: a definite rejection reports
// NOT uploaded; a 5xx, 429 or unreadable 2xx reports UNCONFIRMED. In every case initializeUpload
// is sent exactly once (never retried, since each call mints a new URN), nothing is PUT, no dark
// post exists, and the result is a non-nil partial carrying the ensured campaign.
func TestCreateCampaign_StoredImage_InitializeUploadFailureClasses(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		body        string
		unconfirmed bool
	}{
		{"403 forbidden", http.StatusForbidden, "", false},
		{"400 bad request", http.StatusBadRequest, "", false},
		{"500", http.StatusInternalServerError, "", true},
		{"429 not retried", http.StatusTooManyRequests, "", true},
		{"2xx without a usable URN", 0, `{"value":{"uploadUrl":"x"}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newImageAPI(t)
			api.initStatus, api.initBody = tc.status, tc.body
			in := imageCampaignInput(map[string]ImageUpload{"a": {Data: testPNG(t, 1200, 627), MimeType: "image/png"}},
				CreativeVariant{IntroText: "Intro", Headline: "Head", ImageAssetID: "a"})
			res, err := api.client(withRetryBaseDelay(time.Millisecond)).CreateCampaign(context.Background(), in)
			if err == nil || res == nil {
				t.Fatalf("want a partial result and an error, got res=%v err=%v", res, err)
			}
			if res.CampaignID != "200" || res.CreativeCount != 0 {
				t.Errorf("partial = campaign %q creatives %d, want the ensured campaign 200 with 0 creatives", res.CampaignID, res.CreativeCount)
			}
			if got := api.count("POST /images"); got != 1 {
				t.Errorf("initializeUpload sent %d times, want exactly 1", got)
			}
			if len(api.puts) != 0 || len(api.posts) != 0 {
				t.Errorf("PUTs=%d posts=%d, want none after a failed initializeUpload", len(api.puts), len(api.posts))
			}
			if got := IsOutcomeUnconfirmed(err); got != tc.unconfirmed {
				t.Errorf("IsOutcomeUnconfirmed = %v, want %v (err %v)", got, tc.unconfirmed, err)
			}
			word := "NOT uploaded"
			if tc.unconfirmed {
				word = "UNCONFIRMED"
			}
			if !strings.Contains(strings.Join(res.Steps, "\n"), word) {
				t.Errorf("steps %v must say %q", res.Steps, word)
			}
			assertNoLeak(t, err, res)
		})
	}
}

// TestCreateCampaign_StoredImage_PutFailureMidway: initializeUpload succeeded (the URN is known
// and recorded) and the byte upload failed. It is never retried, nothing is polled or posted, a
// 4xx is definite and a 5xx is UNCONFIRMED, and the signed upload URL never reaches the error.
func TestCreateCampaign_StoredImage_PutFailureMidway(t *testing.T) {
	for _, tc := range []struct {
		status      int
		unconfirmed bool
	}{{http.StatusBadRequest, false}, {http.StatusBadGateway, true}} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			api := newImageAPI(t)
			api.putStatus = tc.status
			in := imageCampaignInput(map[string]ImageUpload{"a": {Data: testPNG(t, 1200, 627), MimeType: "image/png"}},
				CreativeVariant{IntroText: "Intro", Headline: "Head", ImageAssetID: "a"})
			res, err := api.client().CreateCampaign(context.Background(), in)
			if err == nil || res == nil {
				t.Fatalf("want a partial result and an error, got res=%v err=%v", res, err)
			}
			if len(api.puts) != 1 {
				t.Errorf("PUT sent %d times, want exactly 1", len(api.puts))
			}
			if api.pollCalls != 0 || len(api.posts) != 0 {
				t.Errorf("polls=%d posts=%d, want none after a failed upload", api.pollCalls, len(api.posts))
			}
			if res.ImageURNs["a"] != "urn:li:image:C4E10AQimgA" {
				t.Errorf("ImageURNs = %v, want the minted URN recorded", res.ImageURNs)
			}
			if got := IsOutcomeUnconfirmed(err); got != tc.unconfirmed {
				t.Errorf("IsOutcomeUnconfirmed = %v, want %v", got, tc.unconfirmed)
			}
			assertNoLeak(t, err, res)
		})
	}
}

// TestCreateCampaign_StoredImage_AvailabilityTimeout: an image that never becomes AVAILABLE is
// never referenced; the wait is bounded and the failure is definite (only GETs were sent).
func TestCreateCampaign_StoredImage_AvailabilityTimeout(t *testing.T) {
	api := newImageAPI(t)
	api.pollStatus = []string{"PROCESSING"}
	in := imageCampaignInput(map[string]ImageUpload{"a": {Data: testPNG(t, 1200, 627), MimeType: "image/png"}},
		CreativeVariant{IntroText: "Intro", Headline: "Head", ImageAssetID: "a"})
	start := time.Now()
	res, err := api.client(WithImagePolling(time.Millisecond, 30*time.Millisecond)).CreateCampaign(context.Background(), in)
	if err == nil || res == nil {
		t.Fatalf("want a partial result and an error, got res=%v err=%v", res, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("the availability wait was not bounded (%s)", time.Since(start))
	}
	if !strings.Contains(err.Error(), "not AVAILABLE") || !strings.Contains(err.Error(), "PROCESSING") {
		t.Errorf("error %q must say the image was not AVAILABLE and its last status", err)
	}
	if len(api.posts) != 0 {
		t.Error("no dark post may reference an image that is not AVAILABLE")
	}
	if IsOutcomeUnconfirmed(err) {
		t.Error("a poll timeout sent only GETs; it must be a definite failure")
	}
}

// TestCreateCampaign_StoredImage_ProcessingFailedAndTransientPolls: PROCESSING_FAILED stops at
// once; a transient 503 or 429 on a status GET is retried at the next tick.
func TestCreateCampaign_StoredImage_ProcessingFailedAndTransientPolls(t *testing.T) {
	in := func(t *testing.T) CampaignInput {
		return imageCampaignInput(map[string]ImageUpload{"a": {Data: testPNG(t, 1200, 627), MimeType: "image/png"}},
			CreativeVariant{IntroText: "Intro", Headline: "Head", ImageAssetID: "a"})
	}
	t.Run("processing failed", func(t *testing.T) {
		api := newImageAPI(t)
		api.pollStatus = []string{"PROCESSING", "PROCESSING_FAILED"}
		_, err := api.client().CreateCampaign(context.Background(), in(t))
		if err == nil || !strings.Contains(err.Error(), "PROCESSING_FAILED") {
			t.Fatalf("err = %v, want PROCESSING_FAILED", err)
		}
		if api.pollCalls != 2 || len(api.posts) != 0 {
			t.Errorf("polls=%d posts=%d, want 2 polls and no post", api.pollCalls, len(api.posts))
		}
	})
	t.Run("transient poll errors then available", func(t *testing.T) {
		api := newImageAPI(t)
		api.pollErrs = []int{http.StatusServiceUnavailable, http.StatusTooManyRequests}
		api.pollStatus = []string{"", "", "PROCESSING", "AVAILABLE"}
		res, err := api.client().CreateCampaign(context.Background(), in(t))
		if err != nil {
			t.Fatalf("CreateCampaign: %v", err)
		}
		if res.CreativeCount != 1 || len(api.posts) != 1 {
			t.Errorf("creatives=%d posts=%d, want 1", res.CreativeCount, len(api.posts))
		}
	})
	t.Run("definite poll 404 stops", func(t *testing.T) {
		api := newImageAPI(t)
		api.pollErrs = []int{http.StatusNotFound}
		_, err := api.client().CreateCampaign(context.Background(), in(t))
		if err == nil || api.pollCalls != 1 {
			t.Fatalf("err=%v polls=%d, want a failure after one poll", err, api.pollCalls)
		}
	})
}

// TestCreateCampaign_StoredImage_UntrustedUploadURLGetsNoToken: the PUT carries the bearer
// token, so an upload URL outside LinkedIn is refused and nothing is sent to it.
func TestCreateCampaign_StoredImage_UntrustedUploadURLGetsNoToken(t *testing.T) {
	var hits int
	var mu sync.Mutex
	evil := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer evil.Close()
	api := newImageAPI(t)
	api.uploadHost = evil.URL
	in := imageCampaignInput(map[string]ImageUpload{"a": {Data: testPNG(t, 1200, 627), MimeType: "image/png"}},
		CreativeVariant{IntroText: "Intro", Headline: "Head", ImageAssetID: "a"})
	res, err := api.client().CreateCampaign(context.Background(), in)
	if err == nil || res == nil {
		t.Fatal("want a partial result and an error")
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Errorf("the untrusted upload host received %d request(s); the bearer token must never be sent there", hits)
	}
	if IsOutcomeUnconfirmed(err) {
		t.Error("nothing was uploaded; the failure is definite")
	}
}

func TestTrustedUploadURL(t *testing.T) {
	c := NewClient(Credentials{AccessToken: "t"}, testConfig())
	for raw, want := range map[string]bool{
		"https://www.linkedin.com/dms-uploads/x?ut=1": true,
		"https://linkedin.com/x":                      true,
		"http://www.linkedin.com/x":                   false,
		"https://evil.com/www.linkedin.com":           false,
		"https://linkedin.com.evil.com/x":             false,
		"https://user@www.linkedin.com/x":             false,
		"not a url":                                   false,
	} {
		if got := c.trustedUploadURL(raw); got != want {
			t.Errorf("trustedUploadURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

// TestCreateCampaign_StoredImage_RefusedBeforeAnyRequest: a stored image the client cannot use
// is refused with a nil result before the first request, like every other input defect.
func TestCreateCampaign_StoredImage_RefusedBeforeAnyRequest(t *testing.T) {
	good := ImageUpload{Data: testPNG(t, 1200, 627), MimeType: "image/png"}
	cases := map[string]CampaignInput{
		"both URN and asset": imageCampaignInput(map[string]ImageUpload{"a": good},
			CreativeVariant{IntroText: "I", Headline: "H", ImageAssetID: "a", ImageURN: "urn:li:image:X"}),
		"asset not supplied": imageCampaignInput(nil, CreativeVariant{IntroText: "I", Headline: "H", ImageAssetID: "a"}),
		"invalid image": imageCampaignInput(map[string]ImageUpload{"a": {Data: testPNG(t, 100, 100), MimeType: "image/png"}},
			CreativeVariant{IntroText: "I", Headline: "H", ImageAssetID: "a"}),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			api := newImageAPI(t)
			res, err := api.client().CreateCampaign(context.Background(), in)
			if err == nil || res != nil {
				t.Fatalf("want (nil, err), got res=%v err=%v", res, err)
			}
			if n := api.count(""); n != 0 {
				t.Errorf("%d request(s) sent; a refused input must send none", n)
			}
		})
	}
}

func assertNoLeak(t *testing.T, err error, res *CampaignResult) {
	t.Helper()
	all := err.Error() + strings.Join(res.Steps, "\n")
	for _, bad := range []string{imgLeakText, "ut=secret-sig", "tok-1"} {
		if strings.Contains(all, bad) {
			t.Errorf("error/steps leak %q: %s", bad, all)
		}
	}
}

// TestCreateCampaign_StoredImage_SlowPollsStayWithinTheSharedWait: every status GET is slow and
// answers AVAILABLE. Two images must still be bounded by the ONE shared wait — a deadline checked
// only between polls let each slow GET (and an AVAILABLE reply) run past it.
func TestCreateCampaign_StoredImage_SlowPollsStayWithinTheSharedWait(t *testing.T) {
	api := newImageAPI(t)
	api.pollDelay = 300 * time.Millisecond
	in := imageCampaignInput(
		map[string]ImageUpload{"a": {Data: testPNG(t, 1200, 627), MimeType: "image/png"}, "b": {Data: testPNG(t, 1200, 627), MimeType: "image/png"}},
		CreativeVariant{IntroText: "I", Headline: "H", ImageAssetID: "a"},
		CreativeVariant{IntroText: "I", Headline: "H", ImageAssetID: "b"})
	start := time.Now()
	_, err := api.client(WithImagePolling(time.Millisecond, 100*time.Millisecond)).CreateCampaign(context.Background(), in)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "not AVAILABLE within") {
		t.Fatalf("err = %v, want the shared wait to expire", err)
	}
	if len(api.posts) != 0 {
		t.Error("no dark post may be created when the availability wait expired")
	}
	if elapsed > 250*time.Millisecond {
		t.Errorf("availability phase took %s; the shared 100ms wait must bound the slow GETs too", elapsed)
	}
}

// TestCreateCampaign_StoredImage_UnexpectedStatusIsNotEchoed: an undocumented status value is
// reported as "unknown", never copied into the error or Steps.
func TestCreateCampaign_StoredImage_UnexpectedStatusIsNotEchoed(t *testing.T) {
	api := newImageAPI(t)
	api.pollStatus = []string{imgLeakText}
	in := imageCampaignInput(map[string]ImageUpload{"a": {Data: testPNG(t, 1200, 627), MimeType: "image/png"}},
		CreativeVariant{IntroText: "I", Headline: "H", ImageAssetID: "a"})
	res, err := api.client(WithImagePolling(time.Millisecond, 30*time.Millisecond)).CreateCampaign(context.Background(), in)
	if err == nil || res == nil {
		t.Fatal("want a partial result and an error")
	}
	if !strings.Contains(err.Error(), "last status unknown") {
		t.Errorf("err = %v, want the unexpected status reported as unknown", err)
	}
	assertNoLeak(t, err, res)
}

// refuseFirstTransport fails the first request matching match with a PRE-SEND dial error
// (connection refused), then delegates.
type refuseFirstTransport struct {
	mu      sync.Mutex
	match   func(*http.Request) bool
	refused int
}

func (rt *refuseFirstTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	if rt.refused == 0 && rt.match(r) {
		rt.refused++
		rt.mu.Unlock()
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	}
	rt.mu.Unlock()
	return http.DefaultTransport.RoundTrip(r)
}

// TestCreateCampaign_StoredImage_PreSendFailures: a pre-send failure on a status GET is retried
// at the next tick, while one on initializeUpload stays a definite NOT uploaded.
func TestCreateCampaign_StoredImage_PreSendFailures(t *testing.T) {
	in := func(t *testing.T) CampaignInput {
		return imageCampaignInput(map[string]ImageUpload{"a": {Data: testPNG(t, 1200, 627), MimeType: "image/png"}},
			CreativeVariant{IntroText: "I", Headline: "H", ImageAssetID: "a"})
	}
	t.Run("status GET is retried", func(t *testing.T) {
		api := newImageAPI(t)
		rt := &refuseFirstTransport{match: func(r *http.Request) bool {
			return r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/images/")
		}}
		res, err := api.client(WithHTTPClient(&http.Client{Transport: rt})).CreateCampaign(context.Background(), in(t))
		if err != nil {
			t.Fatalf("CreateCampaign: %v; a pre-send poll failure must be retried", err)
		}
		if rt.refused != 1 || res.CreativeCount != 1 {
			t.Errorf("refused=%d creatives=%d, want 1 and 1", rt.refused, res.CreativeCount)
		}
	})
	t.Run("initializeUpload is definite", func(t *testing.T) {
		api := newImageAPI(t)
		rt := &refuseFirstTransport{match: func(r *http.Request) bool {
			return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/images")
		}}
		res, err := api.client(WithHTTPClient(&http.Client{Transport: rt})).CreateCampaign(context.Background(), in(t))
		if err == nil || res == nil {
			t.Fatal("want a partial result and an error")
		}
		if IsOutcomeUnconfirmed(err) || !strings.Contains(strings.Join(res.Steps, "\n"), "NOT uploaded") {
			t.Errorf("a pre-send initializeUpload failure sent nothing; it must be definite (err %v, steps %v)", err, res.Steps)
		}
		if api.count("POST /images") != 0 {
			t.Error("the refused initializeUpload must not have reached the server")
		}
	})
}
