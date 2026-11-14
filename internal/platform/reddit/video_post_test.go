// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// videoPostServer is a fake Reddit API that records the request order and the authored post body.
type videoPostServer struct {
	mu       sync.Mutex
	events   []string
	post     map[string]any
	postCode int
	mutated  bool
}

func (v *videoPostServer) record(e string) {
	v.mu.Lock()
	v.events = append(v.events, e)
	v.mu.Unlock()
}

func newVideoPostClient(t *testing.T, v *videoPostServer) *Client {
	t.Helper()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tokenSrv.Close)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			v.mu.Lock()
			v.mutated = true
			v.mu.Unlock()
		}
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/ad_accounts/"):
			v.record("verify")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_acct", "name": "acct"}})
		case strings.HasSuffix(r.URL.Path, "/posts"):
			v.record("post")
			var body struct {
				Data map[string]any `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			v.mu.Lock()
			v.post = body.Data
			code := v.postCode
			v.mu.Unlock()
			if code != 0 {
				w.WriteHeader(code)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "refused"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t3_video"}})
		case strings.HasSuffix(r.URL.Path, "/campaigns"):
			v.record("campaign")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "c_1"}})
		case strings.HasSuffix(r.URL.Path, "/ad_groups"):
			v.record("adgroup")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		case strings.HasSuffix(r.URL.Path, "/ads"):
			v.record("ad")
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ad_1"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "x_1"}})
		}
	}))
	t.Cleanup(apiSrv.Close)
	return NewClient(testCreds, testAccount, WithBaseURL(apiSrv.URL+"/api/v3"), WithTokenURL(tokenSrv.URL), WithNowFunc(fixedRedditClock()))
}

const (
	lateVideo = "https://lfx-api.example.org/_campaigns/creative-assets/tlf/b/v?exp=1&kind=video&sig=s"
	lateThumb = "https://lfx-api.example.org/_campaigns/creative-assets/tlf/b/t?exp=1&sig=s"
)

// TestCreateCampaign_AuthorsAVideoPost pins the #549 VIDEO post: both late URLs are minted after
// account verification and immediately before the post (video first), the post body is the
// documented VIDEO shape (type VIDEO, content[0].media_url = the video, top-level thumbnail_url,
// the CTA and the UTM destination), and the post comes BEFORE the campaign, so a post failure can
// only degrade to an ad-less campaign.
func TestCreateCampaign_AuthorsAVideoPost(t *testing.T) {
	v := &videoPostServer{}
	c := newVideoPostClient(t, v)
	in := lateSourceInput()
	in.CallToAction = "sign up"
	in.VideoURLSource = func(context.Context) (string, error) { v.record("mint-video"); return lateVideo, nil }
	in.ThumbnailURLSource = func(context.Context) (string, error) { v.record("mint-thumb"); return lateThumb, nil }
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if got := strings.Join(v.events[:5], ","); got != "verify,mint-video,mint-thumb,post,campaign" {
		t.Fatalf("event order = %v, want verify,mint-video,mint-thumb,post,campaign first", v.events)
	}
	if v.post["type"] != "VIDEO" {
		t.Fatalf("post type = %v, want VIDEO", v.post["type"])
	}
	if v.post["thumbnail_url"] != lateThumb {
		t.Fatalf("thumbnail_url = %v, want the late thumbnail URL", v.post["thumbnail_url"])
	}
	content, _ := v.post["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v, want exactly one item", v.post["content"])
	}
	item, _ := content[0].(map[string]any)
	if item["media_url"] != lateVideo {
		t.Fatalf("media_url = %v, want the late video URL", item["media_url"])
	}
	if item["call_to_action"] != "Sign Up" {
		t.Fatalf("call_to_action = %v, want Sign Up", item["call_to_action"])
	}
	if dest, _ := item["destination_url"].(string); !strings.Contains(dest, "utm_source=reddit") {
		t.Fatalf("destination_url = %q, want the UTM'd registration URL", dest)
	}
	if v.post["allow_comments"] != false {
		t.Fatalf("allow_comments = %v, want false", v.post["allow_comments"])
	}
	if res.AdID != "ad_1" || res.AdWarning != "" {
		t.Fatalf("result ad = %q warning %q, want a clean ad", res.AdID, res.AdWarning)
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "Authored promoted video post: t3_video") {
		t.Fatalf("steps do not record the video post: %s", joined)
	}
	if strings.Contains(joined, "sig=") {
		t.Fatalf("steps carry a signed URL: %s", joined)
	}
}

// TestCreateCampaign_VideoPostRefusalIsDegradedNotFatal: a definite 4xx on the VIDEO post (the
// shape Reddit would answer if the unprobed schema were wrong) keeps the image path's partial
// rule — the campaign and ad group are still created, with no ad and an AdWarning, so the
// dispatcher records created_degraded and retains the claim.
func TestCreateCampaign_VideoPostRefusalIsDegradedNotFatal(t *testing.T) {
	v := &videoPostServer{postCode: http.StatusBadRequest}
	c := newVideoPostClient(t, v)
	in := lateSourceInput()
	in.VideoURLSource = func(context.Context) (string, error) { return lateVideo, nil }
	in.ThumbnailURLSource = func(context.Context) (string, error) { return lateThumb, nil }
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v (a post refusal must degrade, not fail)", err)
	}
	if res == nil || res.CampaignID == "" || res.AdGroupID == "" {
		t.Fatalf("want the campaign and ad group created, got %+v", res)
	}
	if res.AdID != "" || strings.TrimSpace(res.AdWarning) == "" {
		t.Fatalf("want no ad and an AdWarning, got ad %q warning %q", res.AdID, res.AdWarning)
	}
}

// TestCreateCampaign_VideoInputsRefusedBeforeAnyMutation covers every pre-create refusal: one
// source without the other, a video combined with an image or a post URL, a source error and a
// source returning a bad URL. Each is (nil, err) with no mutating request.
func TestCreateCampaign_VideoInputsRefusedBeforeAnyMutation(t *testing.T) {
	good := func(context.Context) (string, error) { return lateVideo, nil }
	cases := map[string]func(*CampaignInput){
		"video without thumbnail": func(in *CampaignInput) { in.VideoURLSource = good },
		"thumbnail without video": func(in *CampaignInput) { in.ThumbnailURLSource = good },
		"video with an image URL": func(in *CampaignInput) {
			in.VideoURLSource, in.ThumbnailURLSource = good, good
			in.ImageURL = "https://example.com/x.jpg"
		},
		"video with an image source": func(in *CampaignInput) {
			in.VideoURLSource, in.ThumbnailURLSource, in.ImageURLSource = good, good, good
		},
		"video with a post URL": func(in *CampaignInput) {
			in.VideoURLSource, in.ThumbnailURLSource = good, good
			in.PostURL = "https://www.reddit.com/r/test/comments/abc123/x/"
		},
		"video source error": func(in *CampaignInput) {
			in.VideoURLSource = func(context.Context) (string, error) { return "", errors.New("mint failed") }
			in.ThumbnailURLSource = good
		},
		"thumbnail source returns a bad URL": func(in *CampaignInput) {
			in.VideoURLSource = good
			in.ThumbnailURLSource = func(context.Context) (string, error) { return "ftp://x/y", nil }
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			v := &videoPostServer{}
			c := newVideoPostClient(t, v)
			in := lateSourceInput()
			mut(&in)
			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil || res != nil {
				t.Fatalf("want (nil, err), got (%+v, %v)", res, err)
			}
			if v.mutated {
				t.Fatal("a mutating request was made")
			}
			if strings.Contains(err.Error(), "sig=") {
				t.Fatalf("error quotes a signed URL: %v", err)
			}
		})
	}
}
