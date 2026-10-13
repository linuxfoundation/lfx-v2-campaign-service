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
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
)

const xAssetID = "6f1c2d3e-4a5b-4c6d-8e7f-90a1b2c3d4e5"

func xTestPNG(n int) []byte {
	b := make([]byte, n)
	copy(b, "\x89PNG\r\n\x1a\n")
	return b
}

// xScopedCreativeAssets serves assets keyed by project/brief/id, so a lookup under any other
// project or brief is ErrNotFound — the ownership property the real repository's scoped
// queries provide. It counts loads so a test can prove a refusal happened before one.
type xScopedCreativeAssets struct {
	mu     sync.Mutex
	assets map[string]*model.CreativeAsset
	loads  int
	sizes  int
	// scopes records "size:project/brief" and "load:project/brief" per call.
	scopes []string
	// failLoads makes the next N GetAsset calls fail with a non-not-found error.
	failLoads int
}

func (s *xScopedCreativeAssets) GetAsset(_ context.Context, projectID, briefID, assetID string) (*model.CreativeAsset, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	s.scopes = append(s.scopes, "load:"+projectID+"/"+briefID)
	if s.failLoads > 0 {
		s.failLoads--
		return nil, errors.New("transient read failure")
	}
	a, ok := s.assets[projectID+"/"+briefID+"/"+assetID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return a, nil
}

func (s *xScopedCreativeAssets) GetAssetSize(_ context.Context, projectID, briefID, assetID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sizes++
	s.scopes = append(s.scopes, "size:"+projectID+"/"+briefID)
	a, ok := s.assets[projectID+"/"+briefID+"/"+assetID]
	if !ok {
		return 0, domain.ErrNotFound
	}
	return int64(len(a.Bytes)), nil
}

func (s *xScopedCreativeAssets) loadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads
}

func briefAsset(img []byte) *xScopedCreativeAssets {
	b := testBrief()
	return &xScopedCreativeAssets{assets: map[string]*model.CreativeAsset{
		b.ProjectID + "/" + b.ID + "/" + xAssetID: {ID: xAssetID, ProjectID: b.ProjectID, BriefID: b.ID, MimeType: model.MimeTypePNG, Bytes: img, ByteSize: int64(len(img))},
	}}
}

// xCreativeServer stands in for the Ads API and the media upload endpoints. It keeps a Media
// Library keyed by name, so a second dispatch of the same image can find the first's entry.
type xCreativeServer struct {
	mu         sync.Mutex
	hits       map[string]int
	library    map[string]string
	tweetQuery string
	initStatus int
	total      int
}

func (x *xCreativeServer) count(label string) int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.hits[label]
}

func newXCreativeServer(t *testing.T) (*xCreativeServer, *httptest.Server) {
	x := &xCreativeServer{hits: map[string]int{}, library: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.mu.Lock()
		defer x.mu.Unlock()
		x.total++
		p := r.URL.Path
		hit := func(l string) { x.hits[l]++ }
		switch {
		case r.Method == http.MethodPost && p == "/2/media/upload/initialize":
			hit("init")
			if x.initStatus != 0 {
				w.WriteHeader(x.initStatus)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"id":"42","media_key":"3_42"}}`))
		case r.Method == http.MethodPost && p == "/2/media/upload/42/append":
			hit("append")
			_, _ = w.Write([]byte(`{"data":{}}`))
		case r.Method == http.MethodPost && p == "/2/media/upload/42/finalize":
			hit("finalize")
			_, _ = w.Write([]byte(`{"data":{"id":"42","media_key":"3_42"}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/media_library"):
			hit("lookup")
			q := r.URL.Query().Get("q")
			if key, ok := x.library[q]; ok {
				_, _ = fmt.Fprintf(w, `{"data":[{"name":%q,"media_key":%q,"media_type":"IMAGE"}],"next_cursor":null}`, q, key)
				return
			}
			_, _ = w.Write([]byte(`{"data":[],"next_cursor":null}`))
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/media_library"):
			hit("library")
			x.library[r.URL.Query().Get("name")] = r.URL.Query().Get("media_key")
			_, _ = fmt.Fprintf(w, `{"data":{"media_key":%q}}`, r.URL.Query().Get("media_key"))
		case r.Method == http.MethodGet && strings.HasSuffix(p, "promotable_users"):
			_, _ = w.Write([]byte(`{"data":[{"user_id":"u1"}],"next_cursor":null}`))
		case r.Method == http.MethodGet && strings.HasSuffix(p, "/accounts/acc1"):
			_, _ = w.Write([]byte(`{"data":{"name":"LF Events"}}`))
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"data":[]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(p, "campaigns"):
			hit("campaign")
			_, _ = w.Write([]byte(`{"data":{"id":"cmp1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(p, "line_items"):
			_, _ = w.Write([]byte(`{"data":{"id":"li1"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(p, "/tweet"):
			hit("tweet")
			x.tweetQuery = r.URL.RawQuery
			_, _ = w.Write([]byte(`{"data":{"id":123456789,"id_str":"123456789"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(p, "promoted_tweets"):
			_, _ = w.Write([]byte(`{"data":[{"id":"pt1"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return x, srv
}

func newXCreativeDispatcher(srvURL string, assets creativeAssetReader, budget *AssetReserver) *TwitterDispatcher {
	d := NewTwitterDispatcher(
		fakeConnReader{conn: activeTwitterConn(goodTwitterCreds)}, identityEncryptor{},
		twitter.WithBaseURL(srvURL), twitter.WithMediaUploadBaseURL(srvURL), twitter.WithWriteDelay(0),
	)
	d.SetCreativeAssetRepo(assets)
	d.SetAssetReserver(budget)
	return d
}

func xImageConfig(extra string) json.RawMessage {
	return json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetText":"Join us at KubeCon","asUserId":"u1","imageAssetId":"` + xAssetID + `"` + extra + `}}`)
}

func requireNoUpstreamCreate(t *testing.T, err error) {
	t.Helper()
	var nuc interface{ NoUpstreamCreate() bool }
	if err == nil || !errors.As(err, &nuc) || !nuc.NoUpstreamCreate() {
		t.Fatalf("want a pre-create (NoUpstreamCreate) error, got %T: %v", err, err)
	}
}

// TestTwitter_DispatchWithImageAssetAttachesMedia: the stored asset is uploaded, added to the
// library and attached to the authored tweet; the result records the media key and the row is
// a clean create.
func TestTwitter_DispatchWithImageAssetAttachesMedia(t *testing.T) {
	x, srv := newXCreativeServer(t)
	d := newXCreativeDispatcher(srv.URL, briefAsset(xTestPNG(900)), nil)

	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, xImageConfig(""))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	x.mu.Lock()
	q := x.tweetQuery
	x.mu.Unlock()
	if !strings.Contains(q, "media_keys=3_42") {
		t.Errorf("the authored tweet must carry the uploaded media key, got %q", q)
	}
	var blob struct{ MediaKey string }
	if err := json.Unmarshal(camp.Result, &blob); err != nil || blob.MediaKey != "3_42" {
		t.Errorf("the persisted result must record MediaKey, got %q (%v)", blob.MediaKey, err)
	}
	if camp.Status != campaignStatusCreated {
		t.Errorf("status = %q, want %q", camp.Status, campaignStatusCreated)
	}
}

// TestTwitter_DispatchWithoutImageAssetTouchesNoMedia: no imageAssetId, no store read and no
// media request.
func TestTwitter_DispatchWithoutImageAssetTouchesNoMedia(t *testing.T) {
	x, srv := newXCreativeServer(t)
	assets := briefAsset(xTestPNG(900))
	d := newXCreativeDispatcher(srv.URL, assets, nil)
	cfg := json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetText":"Join us at KubeCon","asUserId":"u1"}}`)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, cfg); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if assets.loadCount() != 0 || x.count("lookup") != 0 || x.count("init") != 0 {
		t.Errorf("no asset may be read or uploaded without imageAssetId: loads=%d lookups=%d inits=%d", assets.loadCount(), x.count("lookup"), x.count("init"))
	}
	if strings.Contains(x.tweetQuery, "media_keys") {
		t.Errorf("no media may be attached: %q", x.tweetQuery)
	}
}

// TestTwitter_ImageRetryReusesLibraryEntry: a second dispatch of the same asset to the same
// account (an operator retry) reuses the first's library entry and uploads nothing.
func TestTwitter_ImageRetryReusesLibraryEntry(t *testing.T) {
	x, srv := newXCreativeServer(t)
	d := newXCreativeDispatcher(srv.URL, briefAsset(xTestPNG(900)), nil)
	for i := 0; i < 2; i++ {
		if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, xImageConfig("")); err != nil {
			t.Fatalf("Dispatch #%d: %v", i+1, err)
		}
	}
	if x.count("init") != 1 || x.count("library") != 1 || x.count("tweet") != 2 {
		t.Errorf("one upload for two dispatches: init=%d library=%d tweets=%d", x.count("init"), x.count("library"), x.count("tweet"))
	}
}

// TestTwitter_ImageUpstreamRejectionIsPreCreate: X refuses the upload; nothing billable was
// created, so the claim is released and no campaign is POSTed.
func TestTwitter_ImageUpstreamRejectionIsPreCreate(t *testing.T) {
	x, srv := newXCreativeServer(t)
	x.initStatus = http.StatusBadRequest
	d := newXCreativeDispatcher(srv.URL, briefAsset(xTestPNG(900)), nil)
	camp, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, xImageConfig(""))
	requireNoUpstreamCreate(t, err)
	if camp != nil || x.count("campaign") != 0 {
		t.Errorf("no campaign may be created after a refused upload: camp=%v campaigns=%d", camp, x.count("campaign"))
	}
}

// TestTwitter_ImageRefusalsPrecedeEveryRequest covers the dispatcher-side refusals: each is
// pre-create and contacts X not at all.
func TestTwitter_ImageRefusalsPrecedeEveryRequest(t *testing.T) {
	otherBrief := testBrief()
	otherBrief.ID = "brief-2"
	otherProject := testBrief()
	otherProject.ProjectID = "other-project"
	cases := []struct {
		name    string
		brief   *model.CampaignBrief
		cfg     json.RawMessage
		assets  creativeAssetReader
		noLoads bool
	}{
		{"another brief's asset", otherBrief, xImageConfig(""), briefAsset(xTestPNG(900)), false},
		{"another project's asset", otherProject, xImageConfig(""), briefAsset(xTestPNG(900)), false},
		{"with tweetId", testBrief(), xImageConfig(`,"tweetId":"123"`), briefAsset(xTestPNG(900)), true},
		{"without tweetText", testBrief(), json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","imageAssetId":"` + xAssetID + `"}}`), briefAsset(xTestPNG(900)), true},
		{"malformed id", testBrief(), json.RawMessage(`{"twitterConfig":{"budgetAmount":500,"startDate":"2099-03-01","endDate":"2099-03-10","tweetText":"x","imageAssetId":"not-a-uuid"}}`), briefAsset(xTestPNG(900)), true},
		{"over X's limit", testBrief(), xImageConfig(""), briefAsset(xTestPNG(twitter.MaxImageBytes + 1)), true},
		{"store not bound", testBrief(), xImageConfig(""), nil, true},
		{"empty blob", testBrief(), xImageConfig(""), briefAsset([]byte{}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x, srv := newXCreativeServer(t)
			d := newXCreativeDispatcher(srv.URL, tc.assets, nil)
			if tc.assets == nil {
				d.creatives = nil
			}
			_, err := d.Dispatch(context.Background(), tc.brief, model.ProviderTwitterAds, tc.cfg)
			requireNoUpstreamCreate(t, err)
			x.mu.Lock()
			total := x.total
			x.mu.Unlock()
			if total != 0 {
				t.Errorf("a refused image must not contact X, saw %d requests", total)
			}
			if s, ok := tc.assets.(*xScopedCreativeAssets); ok && tc.noLoads && s.loadCount() != 0 {
				t.Errorf("the refusal must precede loading the blob, loads=%d", s.loadCount())
			}
		})
	}
}

// TestTwitter_ImageReservationIsReleased: with a budget that fits exactly one image, two
// sequential dispatches both succeed — the first gave its reservation back — and so does one
// after a failed upload.
func TestTwitter_ImageReservationIsReleased(t *testing.T) {
	img := xTestPNG(900)
	x, srv := newXCreativeServer(t)
	budget := NewAssetReserver(int64(len(img)), 0)
	d := newXCreativeDispatcher(srv.URL, briefAsset(img), budget)

	x.mu.Lock()
	x.initStatus = http.StatusBadRequest
	x.mu.Unlock()
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, xImageConfig(""))
	requireNoUpstreamCreate(t, err)
	x.mu.Lock()
	x.initStatus = 0
	x.mu.Unlock()

	for i := 0; i < 2; i++ {
		if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, xImageConfig("")); err != nil {
			t.Fatalf("Dispatch #%d after release: %v", i+1, err)
		}
	}
	rel, ok := budget.reserve(context.Background(), int64(len(img)))
	if !ok {
		t.Fatal("the whole budget must be free after the dispatches return")
	}
	rel()
}

// TestTwitter_ImageReadsAreScopedToTheBrief pins ownership at the call site: both the size
// read and the blob read are asked for under the dispatching brief's own project and brief.
func TestTwitter_ImageReadsAreScopedToTheBrief(t *testing.T) {
	_, srv := newXCreativeServer(t)
	assets := briefAsset(xTestPNG(900))
	b := testBrief()
	b.ProjectID, b.ID = "proj-z", "brief-z"
	assets.assets["proj-z/brief-z/"+xAssetID] = assets.assets["cncf/brief-1/"+xAssetID]
	d := newXCreativeDispatcher(srv.URL, assets, nil)
	if _, err := d.Dispatch(context.Background(), b, model.ProviderTwitterAds, xImageConfig("")); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	assets.mu.Lock()
	defer assets.mu.Unlock()
	want := []string{"size:proj-z/brief-z", "load:proj-z/brief-z"}
	if strings.Join(assets.scopes, ",") != strings.Join(want, ",") {
		t.Errorf("asset reads must be scoped to the dispatching brief: got %v, want %v", assets.scopes, want)
	}
}

// TestTwitter_ImageReservationReleasedWhenLoadFails: the blob read fails after the reservation
// was taken; the budget must come back, or the next dispatch cannot get it.
func TestTwitter_ImageReservationReleasedWhenLoadFails(t *testing.T) {
	img := xTestPNG(900)
	_, srv := newXCreativeServer(t)
	assets := briefAsset(img)
	assets.failLoads = 1
	budget := NewAssetReserver(int64(len(img)), 0)
	d := newXCreativeDispatcher(srv.URL, assets, budget)
	_, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, xImageConfig(""))
	requireNoUpstreamCreate(t, err)
	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderTwitterAds, xImageConfig("")); err != nil {
		t.Fatalf("the reservation taken before a failed load must be released: %v", err)
	}
}
