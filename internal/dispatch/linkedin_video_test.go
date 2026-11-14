// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"bytes"
	"context"
	"encoding/binary"
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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// liVideoSize is the synthetic MP4's size: above LinkedIn's 75 KiB minimum, split by the fake
// API into two parts at liVideoPartSplit.
const (
	liVideoSize      = 100_000
	liVideoPartSplit = 60_000
)

// liTestMP4 builds a structurally valid MP4 of exactly n bytes: ftyp(isom), moov, mdat.
func liTestMP4(n int) []byte {
	box := func(typ string, payload []byte) []byte {
		b := make([]byte, 8, 8+len(payload))
		binary.BigEndian.PutUint32(b, uint32(8+len(payload)))
		copy(b[4:], typ)
		return append(b, payload...)
	}
	out := box("ftyp", []byte("isom\x00\x00\x02\x00isommp41"))
	out = append(out, box("moov", bytes.Repeat([]byte{1}, 32))...)
	mdat := make([]byte, n-len(out)-8)
	for i := range mdat {
		mdat[i] = byte(i % 251) // distinct bytes, so a mis-sliced part is visible
	}
	return append(out, box("mdat", mdat)...)
}

// liVideoCall is one request the fake LinkedIn API received.
type liVideoCall struct {
	method, path string
	body         []byte
}

// liVideoAPI is a fake LinkedIn REST API for the campaign tree and the Videos API.
type liVideoAPI struct {
	srv *httptest.Server

	mu          sync.Mutex
	calls       []liVideoCall
	statuses    []string // successive GET /videos/{urn} answers; the last repeats
	statusCalls int
	noETag      bool
}

func newLIVideoAPI(t *testing.T) *liVideoAPI {
	t.Helper()
	a := &liVideoAPI{statuses: []string{"PROCESSING", "AVAILABLE"}}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		a.calls = append(a.calls, liVideoCall{method: r.Method, path: r.URL.Path + "?" + r.URL.RawQuery, body: body})
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/dms-uploads/"):
			if !a.noETag {
				w.Header().Set("ETag", "etag-"+strings.TrimPrefix(r.URL.Path, "/dms-uploads/VID1/"))
			}
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/videos" && r.URL.Query().Get("action") == "initializeUpload":
			_, _ = io.WriteString(w, `{"value":{"uploadUrlsExpireAt":1,"video":"urn:li:video:VID1","uploadToken":"tok-1","uploadInstructions":[`+
				`{"uploadUrl":"`+a.srv.URL+`/dms-uploads/VID1/1?sig=x","firstByte":0,"lastByte":59999},`+
				`{"uploadUrl":"`+a.srv.URL+`/dms-uploads/VID1/2?sig=y","firstByte":60000,"lastByte":99999}]}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/videos" && r.URL.Query().Get("action") == "finalizeUpload":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/videos/"):
			i := a.statusCalls
			if i >= len(a.statuses) {
				i = len(a.statuses) - 1
			}
			a.statusCalls++
			_, _ = io.WriteString(w, `{"status":"`+a.statuses[i]+`"}`)
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"elements":[],"metadata":{}}`)
		case strings.Contains(r.URL.Path, "adCampaignGroups"):
			_, _ = io.WriteString(w, `{"id":"urn:li:sponsoredCampaignGroup:100"}`)
		case strings.Contains(r.URL.Path, "adCampaigns"):
			w.Header().Set("x-restli-id", "urn:li:sponsoredCampaign:200")
			_, _ = io.WriteString(w, `{}`)
		case strings.Contains(r.URL.Path, "posts"):
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

// find returns every call whose "METHOD path?query" starts with prefix, in order.
func (a *liVideoAPI) find(prefix string) []liVideoCall {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []liVideoCall
	for _, c := range a.calls {
		if strings.HasPrefix(c.method+" "+c.path, prefix) {
			out = append(out, c)
		}
	}
	return out
}

// index returns the position of the first call starting with prefix, or -1.
func (a *liVideoAPI) index(prefix string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, c := range a.calls {
		if strings.HasPrefix(c.method+" "+c.path, prefix) {
			return i
		}
	}
	return -1
}

func (a *liVideoAPI) total() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

func newLIVideoDispatcher(api *liVideoAPI, assets creativeAssetReader, reserver *AssetReserver) *LinkedInDispatcher {
	clock := func() time.Time { return time.Date(2098, 1, 1, 0, 0, 0, 0, time.UTC) }
	d := NewLinkedInDispatcher(fakeConnReader{conn: activeLinkedInConn(goodLinkedInCreds)}, identityEncryptor{},
		linkedin.WithBaseURL(api.srv.URL), linkedin.WithClock(clock),
		linkedin.WithImagePolling(time.Millisecond, time.Second), linkedin.WithVideoPolling(time.Millisecond, time.Second))
	if assets != nil {
		d.SetCreativeAssetRepo(assets)
	}
	d.SetAssetReserver(reserver)
	return d
}

func liMP4Asset() *scopedCreativeAssets {
	brief := testBrief()
	return &scopedCreativeAssets{
		projectID: brief.ProjectID, briefID: brief.ID, assetID: msAssetID,
		asset: &model.CreativeAsset{ID: msAssetID, MimeType: model.MimeTypeMP4, Bytes: liTestMP4(liVideoSize)},
	}
}

func decodeJSON(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return m
}

// TestLinkedInVideo_DispatchEndToEnd drives Dispatch against a fake LinkedIn API and asserts the
// whole create: a SINGLE_VIDEO campaign named "Video", ONE Videos API upload for a video two
// variants share (initializeUpload -> each part PUT with exactly its byte range -> finalizeUpload
// with the ETags in instruction order -> status polled to AVAILABLE), then per variant a dark post
// carrying the video as content.media and a creative referencing it, in that order.
func TestLinkedInVideo_DispatchEndToEnd(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsVideoEnabled, "true")
	api := newLIVideoAPI(t)
	assets := liMP4Asset()
	d := newLIVideoDispatcher(api, assets, nil)
	cfg := liImageConfig(liVariant(`"videoAssetId":"`+msAssetID+`"`), liVariant(`"videoAssetId":"`+strings.ToUpper(msAssetID)+`"`))

	if err := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, cfg); !errors.Is(err, domain.ErrPreflightNotApplicable) {
		t.Fatalf("PreflightCreate on a valid video config = %v, want ErrPreflightNotApplicable", err)
	}
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want %q", camp.Status, campaignStatusCreated)
	}

	// The campaign: format SINGLE_VIDEO, and the name's Ad Format segment says Video.
	camps := api.find("POST /adAccounts/")
	var campaignBody map[string]any
	for _, c := range camps {
		if strings.Contains(c.path, "/adCampaigns?") {
			campaignBody = decodeJSON(t, c.body)
		}
	}
	if campaignBody == nil {
		t.Fatal("no campaign was created")
	}
	if campaignBody["format"] != "SINGLE_VIDEO" {
		t.Errorf("campaign format = %v, want SINGLE_VIDEO", campaignBody["format"])
	}
	if name, _ := campaignBody["name"].(string); !strings.Contains(name, " | Video | ") || strings.Contains(name, "Static") {
		t.Errorf("campaign name = %q, want the Ad Format segment Video", name)
	}

	// initializeUpload: once, with the organization owner and the exact file size.
	inits := api.find("POST /videos?action=initializeUpload")
	if len(inits) != 1 {
		t.Fatalf("initializeUpload calls = %d, want 1 for one asset shared by two variants", len(inits))
	}
	req, _ := decodeJSON(t, inits[0].body)["initializeUploadRequest"].(map[string]any)
	if req["owner"] != "urn:li:organization:"+linkedInTestOrgID(t) || req["fileSizeBytes"] != float64(liVideoSize) ||
		req["uploadCaptions"] != false || req["uploadThumbnail"] != false {
		t.Errorf("initializeUploadRequest = %v", req)
	}

	// Each part carries exactly its byte range.
	puts := api.find("PUT /dms-uploads/VID1/")
	if len(puts) != 2 {
		t.Fatalf("part PUTs = %d, want 2", len(puts))
	}
	data := assets.asset.Bytes
	if !bytes.Equal(puts[0].body, data[:liVideoPartSplit]) || !bytes.Equal(puts[1].body, data[liVideoPartSplit:]) {
		t.Errorf("part bodies are %d and %d bytes and do not match their byte ranges", len(puts[0].body), len(puts[1].body))
	}

	// finalizeUpload: the ETags in instruction order, and the upload token.
	fins := api.find("POST /videos?action=finalizeUpload")
	if len(fins) != 1 {
		t.Fatalf("finalizeUpload calls = %d, want 1", len(fins))
	}
	fin, _ := decodeJSON(t, fins[0].body)["finalizeUploadRequest"].(map[string]any)
	ids, _ := fin["uploadedPartIds"].([]any)
	if fin["video"] != "urn:li:video:VID1" || fin["uploadToken"] != "tok-1" || len(ids) != 2 || ids[0] != "etag-1" || ids[1] != "etag-2" {
		t.Errorf("finalizeUploadRequest = %v", fin)
	}
	if got := len(api.find("GET /videos/urn:li:video:VID1")); got != 2 {
		t.Errorf("status polls = %d, want 2 (PROCESSING then AVAILABLE)", got)
	}

	// Two video dark posts and two creatives.
	posts := api.find("POST /posts")
	if len(posts) != 2 {
		t.Fatalf("dark posts = %d, want 2", len(posts))
	}
	for i, p := range posts {
		m := decodeJSON(t, p.body)
		content, _ := m["content"].(map[string]any)
		media, _ := content["media"].(map[string]any)
		if media["id"] != "urn:li:video:VID1" || media["title"] != "KubeCon 2099" {
			t.Errorf("post %d content.media = %v", i, media)
		}
		if _, hasArticle := content["article"]; hasArticle {
			t.Errorf("post %d carries an article; a video post must not", i)
		}
		if lp, _ := m["contentLandingPage"].(string); !strings.HasPrefix(lp, "https://events.example/kc?") || !strings.Contains(lp, "utm_source=linkedin") {
			t.Errorf("post %d contentLandingPage = %q, want the UTM-tagged registration URL", i, lp)
		}
		if m["contentCallToActionLabel"] != "REGISTER" {
			t.Errorf("post %d contentCallToActionLabel = %v, want REGISTER", i, m["contentCallToActionLabel"])
		}
		dist, _ := m["distribution"].(map[string]any)
		if dist["feedDistribution"] != "NONE" {
			t.Errorf("post %d is not a dark post: %v", i, dist)
		}
	}
	if got := len(api.find("POST /adAccounts/123456789/creatives")); got != 2 {
		t.Errorf("creatives = %d, want 2", got)
	}

	// Order: campaign before the upload, the upload finalized and AVAILABLE before any post.
	order := []string{"POST /adAccounts/123456789/adCampaigns?", "POST /videos?action=initializeUpload", "PUT /dms-uploads/VID1/1",
		"PUT /dms-uploads/VID1/2", "POST /videos?action=finalizeUpload", "GET /videos/", "POST /posts", "POST /adAccounts/123456789/creatives"}
	last := -1
	for _, step := range order {
		i := api.index(step)
		if i <= last {
			t.Errorf("%q is at position %d, want after position %d", step, i, last)
		}
		last = i
	}

	var res struct {
		VideoURNs map[string]string `json:"videoUrns"`
	}
	if err := json.Unmarshal(camp.Result, &res); err != nil || res.VideoURNs[msAssetID] != "urn:li:video:VID1" {
		t.Errorf("result videoUrns = %v (err %v), want %s -> urn:li:video:VID1", res.VideoURNs, err, msAssetID)
	}
	if strings.Contains(string(camp.ConfigSnapshot), strings.ToUpper(msAssetID)) {
		t.Error("the config snapshot must store the canonical asset id, not the caller's spelling")
	}
	if strings.Contains(string(camp.Result), "sig=") {
		t.Error("a signed upload URL leaked into the result")
	}
}

// linkedInTestOrgID reads the org id activeLinkedInConn gives the fixture connection.
func linkedInTestOrgID(t *testing.T) string {
	t.Helper()
	org := activeLinkedInConn(goodLinkedInCreds).ProviderConfig["org_id"]
	if org == "" {
		t.Fatal("fixture connection has no org_id")
	}
	return org
}

// TestLinkedInVideo_RefusedBeforeAnythingIsCreated: the flag, the request shape and the asset
// are all decided before any LinkedIn request, the claim is released, and the shape refusals are
// also PreflightCreate's synchronous 400.
func TestLinkedInVideo_RefusedBeforeAnythingIsCreated(t *testing.T) {
	video := liVariant(`"videoAssetId":"` + msAssetID + `"`)
	cases := []struct {
		name      string
		flag      string
		assets    func() *scopedCreativeAssets
		variants  []string
		preflight bool // PreflightCreate refuses it too
	}{
		{"feature off", "", liMP4Asset, []string{video}, true},
		{"feature misspelled", "TRUE", liMP4Asset, []string{video}, true},
		{"malformed id", "true", liMP4Asset, []string{liVariant(`"videoAssetId":"not-a-uuid"`)}, true},
		{"video and image on one variant", "true", liMP4Asset, []string{liVariant(`"videoAssetId":"` + msAssetID + `","imageUrn":"urn:li:image:X"`)}, true},
		{"video mixed with an image variant", "true", liMP4Asset, []string{video, liVariant("")}, true},
		{"image asset named as a video", "true", func() *scopedCreativeAssets {
			a := liMP4Asset()
			a.asset.MimeType = model.MimeTypePNG
			return a
		}, []string{video}, false},
		{"video below LinkedIn's minimum size", "true", func() *scopedCreativeAssets {
			a := liMP4Asset()
			a.asset.Bytes = liTestMP4(linkedin.MinVideoBytes - 1)
			return a
		}, []string{video}, false},
		{"asset of another brief", "true", func() *scopedCreativeAssets {
			a := liMP4Asset()
			a.briefID = "other-brief"
			return a
		}, []string{video}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(constants.EnvLinkedInAdsVideoEnabled, tc.flag)
			api := newLIVideoAPI(t)
			reserver := NewAssetReserver(10<<20, testAssetReserveWait)
			d := newLIVideoDispatcher(api, tc.assets(), reserver)
			cfg := liImageConfig(tc.variants...)
			perr := d.PreflightCreate(context.Background(), "cncf", model.ProviderLinkedInAds, cfg)
			if tc.preflight != !errors.Is(perr, domain.ErrPreflightNotApplicable) {
				t.Errorf("PreflightCreate = %v, want refused=%v", perr, tc.preflight)
			}
			if tc.preflight && !errors.Is(perr, domain.ErrCreateConfigInvalid) {
				t.Errorf("PreflightCreate = %v, want ErrCreateConfigInvalid (a 400)", perr)
			}
			camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, cfg)
			if camp != nil || !isNoUpstreamCreate(err) {
				t.Fatalf("want a NoUpstreamCreate refusal, got camp=%v err=%v", camp, err)
			}
			if n := api.total(); n != 0 {
				t.Errorf("%d request(s) reached LinkedIn; a refused video must send none", n)
			}
			if rel, ok := reserver.reserve(context.Background(), 10<<20); !ok {
				t.Error("a refused dispatch leaked its asset reservation")
			} else {
				rel()
			}
		})
	}
}

// TestLinkedInVideo_ProcessingFailureRetainsClaimAndDegrades: a video LinkedIn fails to process
// after the campaign exists leaves the campaign created_degraded with zero creatives, the claim
// retained (so no re-dispatch can upload the video or post it twice), the URN recorded, and no
// dark post.
func TestLinkedInVideo_ProcessingFailureRetainsClaimAndDegrades(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsVideoEnabled, "true")
	api := newLIVideoAPI(t)
	api.statuses = []string{"PROCESSING", "PROCESSING_FAILED"}
	d := newLIVideoDispatcher(api, liMP4Asset(), nil)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, liImageConfig(liVariant(`"videoAssetId":"`+msAssetID+`"`)))
	if err == nil || camp == nil {
		t.Fatalf("want a retained partial (campaign + error), got camp=%v err=%v", camp, err)
	}
	if isNoUpstreamCreate(err) {
		t.Error("a video failure after the campaign exists must RETAIN the claim, not release it")
	}
	if camp.PlatformCampaignID != "200" || camp.Status != campaignStatusCreatedDegraded {
		t.Errorf("campaign = %q status %q, want 200 created_degraded", camp.PlatformCampaignID, camp.Status)
	}
	if got := len(api.find("POST /videos?action=initializeUpload")); got != 1 {
		t.Errorf("initializeUpload sent %d times, want exactly 1", got)
	}
	if got := len(api.find("POST /posts")); got != 0 {
		t.Errorf("%d dark post(s) created; none may be when the video failed", got)
	}
	if !strings.Contains(string(camp.Result), "PROCESSING_FAILED") || !strings.Contains(string(camp.Result), "urn:li:video:VID1") {
		t.Errorf("result must name the failed video and its status: %s", camp.Result)
	}
}

// TestLinkedInVideo_MissingETagNeverFinalizes: a part answered without an ETag cannot be named in
// finalizeUpload, so the upload stops there — no finalize, no post — with the campaign retained.
func TestLinkedInVideo_MissingETagNeverFinalizes(t *testing.T) {
	t.Setenv(constants.EnvLinkedInAdsVideoEnabled, "true")
	api := newLIVideoAPI(t)
	api.noETag = true
	d := newLIVideoDispatcher(api, liMP4Asset(), nil)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderLinkedInAds, liImageConfig(liVariant(`"videoAssetId":"`+msAssetID+`"`)))
	if err == nil || camp == nil || isNoUpstreamCreate(err) {
		t.Fatalf("want a retained partial, got camp=%v err=%v", camp, err)
	}
	if got := len(api.find("PUT /dms-uploads/")); got != 1 {
		t.Errorf("part PUTs = %d, want 1 (stop at the first part without an ETag)", got)
	}
	if got := len(api.find("POST /videos?action=finalizeUpload")) + len(api.find("POST /posts")); got != 0 {
		t.Errorf("%d finalize/post request(s) sent after a part without an ETag", got)
	}
	if !strings.Contains(string(camp.Result), "NOT uploaded") {
		t.Errorf("result steps must say NOT uploaded: %s", camp.Result)
	}
}
