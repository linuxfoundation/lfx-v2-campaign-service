// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testPNG returns n bytes that http.DetectContentType sniffs as image/png.
func testPNG(n int) []byte {
	b := make([]byte, n)
	copy(b, "\x89PNG\r\n\x1a\n")
	for i := 8; i < n; i++ {
		b[i] = byte(i % 251)
	}
	return b
}

// mediaFakeX is a stand-in for both the Ads API and the v2 media upload endpoints, with the
// failure knobs the tests need. Every field is guarded by mu.
type mediaFakeX struct {
	mu sync.Mutex

	library map[string]string // name -> media_key
	calls   map[string]int    // endpoint label -> count
	order   []string          // endpoint labels in arrival order

	segments     map[int][]byte
	tweetQuery   string
	libraryQuery string

	initStatus     int
	failAppendSeg  int // -1: none
	failAppendCode int
	finalizeBody   string
	statusBody     string
	libraryStatus  int
	libraryBody    string
	lookupBody     string // overrides the computed lookup page when set
	// initWaitsForLookups, when > 0, holds each initialize until that many lookups have
	// arrived (or initWaitMax passes), so an unserialized pair of uploads is caught every time.
	initWaitsForLookups int
	initWaitMax         time.Duration
}

func newMediaFakeX() *mediaFakeX {
	return &mediaFakeX{library: map[string]string{}, calls: map[string]int{}, segments: map[int][]byte{}, failAppendSeg: -1}
}

func (f *mediaFakeX) hit(label string) {
	f.calls[label]++
	f.order = append(f.order, label)
}

func (f *mediaFakeX) count(label string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[label]
}

func (f *mediaFakeX) uploaded() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []byte
	for i := 0; i < len(f.segments); i++ {
		out = append(out, f.segments[i]...)
	}
	return out
}

func (f *mediaFakeX) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/2/media/upload/initialize" && f.initWaitsForLookups > 0 {
			deadline := time.Now().Add(f.initWaitMax)
			for f.count("lookup") < f.initWaitsForLookups && time.Now().Before(deadline) {
				time.Sleep(2 * time.Millisecond)
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		p := r.URL.Path
		switch {
		case r.Method == http.MethodPost && p == "/2/media/upload/initialize":
			f.hit("init")
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("initialize body is not JSON: %v", err)
			}
			if body["media_category"] != "tweet_image" {
				t.Errorf("initialize must send media_category=tweet_image, got %v", body["media_category"])
			}
			if f.initStatus != 0 {
				w.WriteHeader(f.initStatus)
				_, _ = w.Write([]byte(`{"title":"SECRET-UPSTREAM-DETAIL"}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"id":"777","media_key":"3_777","expires_after_secs":86400}}`))
		case r.Method == http.MethodPost && p == "/2/media/upload/777/append":
			f.hit("append")
			if err := r.ParseMultipartForm(16 << 20); err != nil {
				t.Errorf("append body is not multipart: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			seg, _ := strconv.Atoi(r.FormValue("segment_index"))
			file, _, err := r.FormFile("media")
			if err != nil {
				t.Errorf("append carries no media part: %v", err)
				return
			}
			chunk, _ := io.ReadAll(file)
			if seg == f.failAppendSeg {
				w.WriteHeader(f.failAppendCode)
				return
			}
			f.segments[seg] = chunk
			_, _ = w.Write([]byte(`{"data":{"expires_at":1}}`))
		case r.Method == http.MethodPost && p == "/2/media/upload/777/finalize":
			f.hit("finalize")
			if f.finalizeBody != "" {
				_, _ = w.Write([]byte(f.finalizeBody))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"id":"777","media_key":"3_777","size":1}}`))
		case r.Method == http.MethodGet && p == "/2/media/upload":
			f.hit("status")
			_, _ = w.Write([]byte(f.statusBody))
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/media_library"):
			f.hit("lookup")
			if f.lookupBody != "" {
				_, _ = w.Write([]byte(f.lookupBody))
				return
			}
			q := r.URL.Query().Get("q")
			if key, ok := f.library[q]; ok {
				_, _ = fmt.Fprintf(w, `{"data":[{"name":%q,"media_key":%q,"media_type":"IMAGE","deleted":false}],"next_cursor":null}`, q, key)
				return
			}
			_, _ = w.Write([]byte(`{"data":[],"next_cursor":null}`))
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/media_library"):
			f.hit("library")
			f.libraryQuery = r.URL.RawQuery
			if f.libraryStatus != 0 {
				w.WriteHeader(f.libraryStatus)
				return
			}
			f.library[r.URL.Query().Get("name")] = r.URL.Query().Get("media_key")
			if f.libraryBody != "" {
				_, _ = w.Write([]byte(f.libraryBody))
				return
			}
			_, _ = fmt.Fprintf(w, `{"data":{"media_key":%q,"media_type":"IMAGE"}}`, r.URL.Query().Get("media_key"))
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(p, "promotable_users"):
			_, _ = w.Write([]byte(`{"data":[{"user_id":"u1"}],"next_cursor":null}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(p, "campaigns"):
			f.hit("campaign")
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(p, "line_items"):
			f.hit("lineitem")
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/tweet"):
			f.hit("tweet")
			f.tweetQuery = r.URL.RawQuery
			_, _ = w.Write([]byte(`{"data":{"id":123456789,"id_str":"123456789"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(p, "promoted_tweets"):
			f.hit("promoted")
			_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func newMediaTestClient(t *testing.T, f *mediaFakeX) *Client {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	c := newAuthorTweetTestClient(srv.URL)
	c.uploadBaseURL = srv.URL
	c.mediaPollCap = time.Millisecond
	return c
}

func imageInput(img []byte) CampaignInput {
	in := baseAuthorInput("Join us at KubeCon")
	in.Image = &ImageInput{Bytes: img, MIME: "image/png"}
	return in
}

// TestCreateCampaign_ImageChunkedUploadAttachesMediaKey is the happy path: no library entry
// exists, so the image is uploaded in several segments, added to the Media Library under its
// deterministic name, and its key is sent as media_keys on the authored tweet.
func TestCreateCampaign_ImageChunkedUploadAttachesMediaKey(t *testing.T) {
	f := newMediaFakeX()
	c := newMediaTestClient(t, f)
	c.mediaChunkSize = 1000
	img := testPNG(2500)

	res, err := c.CreateCampaign(context.Background(), imageInput(img))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if got := f.count("append"); got != 3 {
		t.Errorf("a 2500-byte image in 1000-byte segments is 3 appends, got %d", got)
	}
	if !bytes.Equal(f.uploaded(), img) {
		t.Error("the reassembled segments must equal the image byte-for-byte, in segment order")
	}
	if res.MediaKey != "3_777" {
		t.Errorf("MediaKey = %q, want 3_777", res.MediaKey)
	}
	f.mu.Lock()
	tweetQuery, libraryQuery := f.tweetQuery, f.libraryQuery
	order := append([]string(nil), f.order...)
	f.mu.Unlock()
	if !strings.Contains(tweetQuery, "media_keys=3_777") {
		t.Errorf("the authored tweet must carry media_keys, got %q", tweetQuery)
	}
	if !strings.Contains(libraryQuery, "name="+mediaLibraryName(img)) {
		t.Errorf("the library entry must be named from the image's SHA-256, got %q", libraryQuery)
	}
	// The image is settled BEFORE anything billable exists.
	if order[0] != "lookup" || order[len(order)-1] != "promoted" {
		t.Errorf("unexpected order %v", order)
	}
	for i, l := range order {
		if l == "campaign" {
			for _, before := range order[:i] {
				if before == "library" {
					return
				}
			}
			t.Fatalf("the library add must precede the campaign POST, order %v", order)
		}
	}
}

// TestCreateCampaign_ImageReusedFromLibraryIsNotReuploaded pins the idempotency: a library
// entry under the image's deterministic name is reused, and nothing is uploaded or added.
func TestCreateCampaign_ImageReusedFromLibraryIsNotReuploaded(t *testing.T) {
	f := newMediaFakeX()
	c := newMediaTestClient(t, f)
	img := testPNG(600)
	f.library[mediaLibraryName(img)] = "3_555"

	res, err := c.CreateCampaign(context.Background(), imageInput(img))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	for _, l := range []string{"init", "append", "finalize", "library"} {
		if n := f.count(l); n != 0 {
			t.Errorf("%s called %d times; an image already in the library must not be uploaded again", l, n)
		}
	}
	if res.MediaKey != "3_555" || !strings.Contains(f.tweetQuery, "media_keys=3_555") {
		t.Errorf("the existing library key must be attached, got MediaKey=%q tweet=%q", res.MediaKey, f.tweetQuery)
	}
}

// TestCreateCampaign_ImageRetryAfterSuccessUploadsOnce runs the same create twice against one
// account: the second finds the first's library entry by name.
func TestCreateCampaign_ImageRetryAfterSuccessUploadsOnce(t *testing.T) {
	f := newMediaFakeX()
	c := newMediaTestClient(t, f)
	img := testPNG(600)
	for i := 0; i < 2; i++ {
		if _, err := c.CreateCampaign(context.Background(), imageInput(img)); err != nil {
			t.Fatalf("CreateCampaign #%d: %v", i+1, err)
		}
	}
	if f.count("init") != 1 || f.count("library") != 1 {
		t.Errorf("two creates of one image must upload once: init=%d library=%d", f.count("init"), f.count("library"))
	}
}

// TestCreateCampaign_ImageValidationPrecedesEveryRequest covers the up-front refusals.
func TestCreateCampaign_ImageValidationPrecedesEveryRequest(t *testing.T) {
	cases := map[string]func(*CampaignInput){
		"with tweetId":     func(in *CampaignInput) { in.TweetID = "123" },
		"without text":     func(in *CampaignInput) { in.TweetText = "" },
		"oversize":         func(in *CampaignInput) { in.Image.Bytes = testPNG(MaxImageBytes + 1) },
		"empty":            func(in *CampaignInput) { in.Image.Bytes = nil },
		"unsupported type": func(in *CampaignInput) { in.Image.MIME = "image/gif" },
		"type mismatch":    func(in *CampaignInput) { in.Image.MIME = "image/jpeg" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var hits int
			var mu sync.Mutex
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				hits++
				mu.Unlock()
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer srv.Close()
			c := newAuthorTweetTestClient(srv.URL)
			c.uploadBaseURL = srv.URL
			in := imageInput(testPNG(600))
			mutate(&in)
			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil || res != nil {
				t.Fatalf("want (nil, err), got (%v, %v)", res, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if hits != 0 {
				t.Errorf("an invalid image must be refused before any request, saw %d", hits)
			}
		})
	}
}

// TestCreateCampaign_ImageUpstreamRejectionCreatesNothing: X refuses the upload; the create
// fails with (nil, err), no campaign is POSTed, and X's body text is not in the error.
func TestCreateCampaign_ImageUpstreamRejectionCreatesNothing(t *testing.T) {
	f := newMediaFakeX()
	f.initStatus = http.StatusBadRequest
	c := newMediaTestClient(t, f)

	res, err := c.CreateCampaign(context.Background(), imageInput(testPNG(600)))
	if err == nil || res != nil {
		t.Fatalf("want (nil, err), got (%v, %v)", res, err)
	}
	if strings.Contains(err.Error(), "SECRET-UPSTREAM-DETAIL") {
		t.Errorf("upstream body text must not reach the error: %v", err)
	}
	if !strings.Contains(err.Error(), "failed before any campaign was created") {
		t.Errorf("a definite rejection must say so: %v", err)
	}
	if f.count("campaign") != 0 || f.count("library") != 0 {
		t.Error("nothing may be created after a refused upload")
	}
}

// TestCreateCampaign_ImageChunkFailureMidwayStops: the second of three segments fails with a
// 5xx. Nothing after it is sent — no further segment, no finalize, no library add, no campaign
// — and the outcome is reported UNCONFIRMED, as every ambiguous write here is.
func TestCreateCampaign_ImageChunkFailureMidwayStops(t *testing.T) {
	f := newMediaFakeX()
	f.failAppendSeg = 1
	f.failAppendCode = http.StatusInternalServerError
	c := newMediaTestClient(t, f)
	c.mediaChunkSize = 1000

	res, err := c.CreateCampaign(context.Background(), imageInput(testPNG(2500)))
	if err == nil || res != nil {
		t.Fatalf("want (nil, err), got (%v, %v)", res, err)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") || !strings.Contains(err.Error(), "segment 1") {
		t.Errorf("a mid-upload 5xx must be UNCONFIRMED and name the segment: %v", err)
	}
	if f.count("append") != 2 {
		t.Errorf("the upload must stop at the failing segment, appends=%d", f.count("append"))
	}
	for _, l := range []string{"finalize", "library", "campaign"} {
		if f.count(l) != 0 {
			t.Errorf("%s must not run after a failed segment", l)
		}
	}
}

// TestCreateCampaign_ImageLibraryAddAmbiguousCreatesNothing: the library add 5xxs. No
// campaign is created and the wording points the retry at the by-name reuse.
func TestCreateCampaign_ImageLibraryAddAmbiguousCreatesNothing(t *testing.T) {
	f := newMediaFakeX()
	f.libraryStatus = http.StatusBadGateway
	c := newMediaTestClient(t, f)
	res, err := c.CreateCampaign(context.Background(), imageInput(testPNG(600)))
	if err == nil || res != nil {
		t.Fatalf("want (nil, err), got (%v, %v)", res, err)
	}
	if !strings.Contains(err.Error(), "UNCONFIRMED") || f.count("campaign") != 0 {
		t.Errorf("ambiguous library add: err=%v campaigns=%d", err, f.count("campaign"))
	}
}

// TestCreateCampaign_ImageLibraryAddWithoutKeyIsRefused: a 2xx that does not name the
// uploaded key is not taken as confirmation.
func TestCreateCampaign_ImageLibraryAddWithoutKeyIsRefused(t *testing.T) {
	f := newMediaFakeX()
	f.libraryBody = `{"data":{}}`
	c := newMediaTestClient(t, f)
	res, err := c.CreateCampaign(context.Background(), imageInput(testPNG(600)))
	if err == nil || res != nil || f.count("campaign") != 0 {
		t.Fatalf("a keyless library 2xx must refuse before the campaign: res=%v err=%v", res, err)
	}
}

// TestCreateCampaign_ImageLookupInconclusiveRefusesUpload: a FULL lookup page with no cursor
// cannot rule out an existing upload, so nothing is uploaded.
func TestCreateCampaign_ImageLookupInconclusiveRefusesUpload(t *testing.T) {
	f := newMediaFakeX()
	var entries []string
	for i := 0; i < mediaLibraryPageSize; i++ {
		entries = append(entries, fmt.Sprintf(`{"name":"other%d","media_key":"3_%d","media_type":"IMAGE"}`, i, i+1))
	}
	f.lookupBody = `{"data":[` + strings.Join(entries, ",") + `]}`
	c := newMediaTestClient(t, f)
	res, err := c.CreateCampaign(context.Background(), imageInput(testPNG(600)))
	if err == nil || res != nil || f.count("init") != 0 {
		t.Fatalf("an inconclusive lookup must refuse the upload: res=%v err=%v init=%d", res, err, f.count("init"))
	}
}

// TestCreateCampaign_ImageProcessingPolledThenAttached / Failed cover a finalize that reports
// processing.
func TestCreateCampaign_ImageProcessingPolledThenAttached(t *testing.T) {
	f := newMediaFakeX()
	f.finalizeBody = `{"data":{"id":"777","media_key":"3_777","processing_info":{"state":"pending","check_after_secs":1}}}`
	f.statusBody = `{"data":{"id":"777","media_key":"3_777","processing_info":{"state":"succeeded"}}}`
	c := newMediaTestClient(t, f)
	res, err := c.CreateCampaign(context.Background(), imageInput(testPNG(600)))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if f.count("status") != 1 || res.MediaKey != "3_777" {
		t.Errorf("status polls=%d MediaKey=%q", f.count("status"), res.MediaKey)
	}
}

func TestCreateCampaign_ImageProcessingFailedCreatesNothing(t *testing.T) {
	f := newMediaFakeX()
	f.finalizeBody = `{"data":{"id":"777","media_key":"3_777","processing_info":{"state":"failed"}}}`
	c := newMediaTestClient(t, f)
	res, err := c.CreateCampaign(context.Background(), imageInput(testPNG(600)))
	if err == nil || res != nil || f.count("library") != 0 || f.count("campaign") != 0 {
		t.Fatalf("a failed processing state must refuse before the library add: res=%v err=%v", res, err)
	}
}

// TestCreateCampaign_ImageWritesArePaced: every media POST takes a slot from the account
// pacer, like every other write — init, two appends, finalize, library add, then campaign,
// line item, tweet, promoted tweet.
func TestCreateCampaign_ImageWritesArePaced(t *testing.T) {
	f := newMediaFakeX()
	c := newMediaTestClient(t, f)
	c.writeDelay = time.Millisecond
	c.timeFn = time.Now
	c.mediaChunkSize = 400
	var admits int
	var mu sync.Mutex
	c.onAdmit = func(context.Context, time.Time) {
		mu.Lock()
		admits++
		mu.Unlock()
	}
	if _, err := c.CreateCampaign(context.Background(), imageInput(testPNG(600))); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := 1 + 2 + 1 + 1 + 4; admits != want {
		t.Errorf("paced writes = %d, want %d", admits, want)
	}
}

// TestCreateCampaign_NoImageMakesNoMediaRequests: the existing flow is untouched.
func TestCreateCampaign_NoImageMakesNoMediaRequests(t *testing.T) {
	f := newMediaFakeX()
	c := newMediaTestClient(t, f)
	res, err := c.CreateCampaign(context.Background(), baseAuthorInput("Join us at KubeCon"))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	for _, l := range []string{"lookup", "init", "library"} {
		if f.count(l) != 0 {
			t.Errorf("%s must not run without an image", l)
		}
	}
	if res.MediaKey != "" || strings.Contains(f.tweetQuery, "media_keys") {
		t.Errorf("no media may be attached without an image: %q", f.tweetQuery)
	}
}

// TestCreateCampaign_ImageMediaStateMustBeReported: a finalize or STATUS body that does not
// say what state the media is in, or a state that never becomes "succeeded", is never read as
// ready. The create fails before the library add and the campaign; where the body is
// unreadable the outcome is UNCONFIRMED, as for any unreadable 2xx.
func TestCreateCampaign_ImageMediaStateMustBeReported(t *testing.T) {
	const pending = `{"data":{"id":"777","media_key":"3_777","processing_info":{"state":"pending","check_after_secs":1}}}`
	cases := []struct {
		name, finalize, status string
		unconfirmed            bool
		statusPolls            int
	}{
		{"finalize without data", `{}`, "", true, 0},
		{"finalize with null data", `{"data":null}`, "", true, 0},
		{"status without data", pending, `{}`, true, 1},
		{"status with null data", pending, `{"data":null}`, true, 1},
		{"status without processing_info", pending, `{"data":{"id":"777","media_key":"3_777"}}`, true, 1},
		{"empty processing state", `{"data":{"id":"777","media_key":"3_777","processing_info":{"state":""}}}`, `{"data":{"processing_info":{"state":""}}}`, false, maxMediaStatusPolls},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newMediaFakeX()
			f.finalizeBody, f.statusBody = tc.finalize, tc.status
			c := newMediaTestClient(t, f)
			res, err := c.CreateCampaign(context.Background(), imageInput(testPNG(600)))
			if err == nil || res != nil {
				t.Fatalf("an unreported media state must not be treated as ready: res=%v err=%v", res, err)
			}
			if got := strings.Contains(err.Error(), "UNCONFIRMED"); got != tc.unconfirmed {
				t.Errorf("UNCONFIRMED=%v, want %v: %v", got, tc.unconfirmed, err)
			}
			if f.count("status") != tc.statusPolls {
				t.Errorf("status polls = %d, want %d", f.count("status"), tc.statusPolls)
			}
			if f.count("library") != 0 || f.count("campaign") != 0 || f.count("tweet") != 0 {
				t.Error("nothing may be added or created for media X never reported ready")
			}
		})
	}
}

// TestCreateCampaign_ConcurrentSameImageUploadsOnce: two clients for ONE account (sharing the
// dispatcher's AccountPacers registry, as two projects on the shared LF connection do) create
// with identical image bytes at the same time. Exactly one upload and one library add happen;
// the other create reuses the entry. The fake holds the first initialize until a second lookup
// arrives, so an unserialized pair is caught deterministically.
func TestCreateCampaign_ConcurrentSameImageUploadsOnce(t *testing.T) {
	f := newMediaFakeX()
	f.initWaitsForLookups = 2
	f.initWaitMax = 300 * time.Millisecond
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	reg := NewAccountPacers()
	mk := func() *Client {
		c := NewClient(
			Credentials{ConsumerKey: "ck", ConsumerSecret: "cs", AccessToken: "at", AccessTokenSecret: "ats"},
			AccountConfig{AccountID: "acc1", FundingInstrumentID: "fi1"},
			WithBaseURL(srv.URL), WithMediaUploadBaseURL(srv.URL), WithWriteDelay(0), WithAccountPacers(reg),
		)
		c.timeFn = staticTime
		return c
	}
	clients := []*Client{mk(), mk()}
	img := testPNG(600)
	var wg sync.WaitGroup
	errs := make([]error, len(clients))
	keys := make([]string, len(clients))
	for i, c := range clients {
		wg.Add(1)
		go func(i int, c *Client) {
			defer wg.Done()
			res, err := c.CreateCampaign(context.Background(), imageInput(img))
			errs[i] = err
			if res != nil {
				keys[i] = res.MediaKey
			}
		}(i, c)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if f.count("init") != 1 || f.count("library") != 1 {
		t.Errorf("concurrent creates of one image must upload once: init=%d library=%d", f.count("init"), f.count("library"))
	}
	if keys[0] != "3_777" || keys[1] != "3_777" {
		t.Errorf("both creates must attach the one library entry, got %v", keys)
	}
}

// TestMediaLocks_ContextBoundsTheWaitAndEntriesAreDropped: a waiter whose context ends gives
// up without the lock, and the map is empty once every holder and waiter has left.
func TestMediaLocks_ContextBoundsTheWaitAndEntriesAreDropped(t *testing.T) {
	l := newMediaLocks()
	rel, err := l.acquire(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := l.acquire(ctx, "k"); err == nil {
		t.Fatal("a waiter whose context ends must not get the lock")
	}
	rel()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.m) != 0 {
		t.Errorf("entries must be dropped when unused, %d left", len(l.m))
	}
}
