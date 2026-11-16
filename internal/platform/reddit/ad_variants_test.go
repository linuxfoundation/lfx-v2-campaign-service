// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// variantAPI is a Reddit API stand-in for one-ad-per-variant creates. failPost / failAd name the
// 1-based variant (by request order) whose post or ad Reddit refuses with a 400.
type variantAPI struct {
	mu       sync.Mutex
	failPost int
	failAd   int
	// failCampaign makes Reddit refuse the campaign create with a 400.
	failCampaign bool
	// cancelOn / cancelAt cancel the create's context on the cancelAt-th request whose path ends
	// in cancelOn, and hold that request until the client gives up: an in-flight cancellation.
	cancelOn string
	cancelAt int
	cancel   context.CancelFunc
	seen     map[string]int
	requests []string
	posts    []map[string]any
	ads      []map[string]any
}

func (a *variantAPI) client(t *testing.T) *Client {
	t.Helper()
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
	}))
	t.Cleanup(tok.Close)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.requests = append(a.requests, r.Method+" "+r.URL.Path)
		var env struct {
			Data map[string]any `json:"data"`
		}
		if r.Method != http.MethodGet {
			_ = json.NewDecoder(r.Body).Decode(&env)
		}
		path := r.URL.Path
		if a.cancelOn != "" && strings.HasSuffix(path, a.cancelOn) {
			if a.seen == nil {
				a.seen = map[string]int{}
			}
			a.seen[a.cancelOn]++
			if a.seen[a.cancelOn] == a.cancelAt {
				a.cancel()
				<-r.Context().Done()
				return
			}
		}
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t2_test"}})
		case strings.HasSuffix(path, "/posts"):
			a.posts = append(a.posts, env.Data)
			if len(a.posts) == a.failPost {
				http.Error(w, `{"error":"bad post"}`, http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": fmt.Sprintf("t3_p%d", len(a.posts))}})
		case strings.HasSuffix(path, "/campaigns"):
			if a.failCampaign {
				http.Error(w, `{"error":"bad campaign"}`, http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "camp_1"}})
		case strings.HasSuffix(path, "/ad_groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "ag_1"}})
		case strings.HasSuffix(path, "/ads"):
			a.ads = append(a.ads, env.Data)
			if len(a.ads) == a.failAd {
				http.Error(w, `{"error":"bad ad"}`, http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": fmt.Sprintf("ad_%d", len(a.ads))}})
		case r.Method == http.MethodPatch:
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{}})
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(api.Close)
	return NewClient(testCreds, testAccount, WithBaseURL(api.URL+"/api/v3"), WithTokenURL(tok.URL), WithNowFunc(fixedRedditClock()))
}

// lock takes the handler's mutex for the rest of a test, so the test goroutine reads what the
// handler wrote with a happens-before edge. Call it only once the test sends no more requests.
func (a *variantAPI) lock(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	t.Cleanup(a.mu.Unlock)
}

// takeRequests returns the requests so far and clears them, under the handler's mutex.
func (a *variantAPI) takeRequests() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	got := a.requests
	a.requests = nil
	return got
}

func variantInput(n int) CampaignInput {
	in := CampaignInput{
		EventName:         "Open Source Summit",
		Project:           "tlf",
		EventSlug:         "oss-2026",
		RegistrationURL:   "https://events.linuxfoundation.org/oss/",
		BudgetUSD:         500,
		StartDate:         "2026-08-01",
		EndDate:           "2026-08-31",
		GeoTargets:        []string{"us"},
		Objective:         "conversions",
		ConversionPixelID: "pixel_abc",
		ImageURL:          "https://events.linuxfoundation.org/wp-content/uploads/banner.jpg",
		AdPerVariant:      true,
	}
	for i := 0; i < n; i++ {
		in.Variants = append(in.Variants, AdVariant{Headline: fmt.Sprintf("Headline %d", i+1)})
	}
	return in
}

func utmContent(t *testing.T, raw any) string {
	t.Helper()
	s, _ := raw.(string)
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return u.Query().Get("utm_content")
}

// Every variant gets its own post (its headline, its utm_content) and its own ad, and every ad id
// is recorded: AdIDs in variant order, AdID the first.
func TestAdPerVariant_CreatesOnePostAndOneAdPerVariant(t *testing.T) {
	a := &variantAPI{}
	res, err := a.client(t).CreateCampaign(context.Background(), variantInput(3))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	a.lock(t)
	if want := []string{"ad_1", "ad_2", "ad_3"}; !reflect.DeepEqual(res.AdIDs, want) {
		t.Fatalf("AdIDs = %v, want %v", res.AdIDs, want)
	}
	if res.AdID != "ad_1" || res.AdCount != 3 || res.AdWarning != "" {
		t.Fatalf("AdID/AdCount/AdWarning = %q/%d/%q, want ad_1/3/empty", res.AdID, res.AdCount, res.AdWarning)
	}
	if len(a.posts) != 3 || len(a.ads) != 3 {
		t.Fatalf("posts/ads = %d/%d, want 3/3", len(a.posts), len(a.ads))
	}
	for i := 0; i < 3; i++ {
		if got, want := a.posts[i]["headline"], fmt.Sprintf("Headline %d", i+1); got != want {
			t.Errorf("post %d headline = %v, want %s", i+1, got, want)
		}
		content, _ := a.posts[i]["content"].([]any)
		item, _ := content[0].(map[string]any)
		want := fmt.Sprintf("variant-%d", i+1)
		if got := utmContent(t, item["destination_url"]); got != want {
			t.Errorf("post %d utm_content = %q, want %q", i+1, got, want)
		}
		if got := utmContent(t, a.ads[i]["click_url"]); got != want {
			t.Errorf("ad %d utm_content = %q, want %q", i+1, got, want)
		}
		if got := a.ads[i]["post_id"]; got != fmt.Sprintf("t3_p%d", i+1) {
			t.Errorf("ad %d post_id = %v", i+1, got)
		}
		if res.VariantAds[i].Outcome != PromoAdCreated || res.VariantAds[i].Variant != i+1 {
			t.Errorf("VariantAds[%d] = %+v", i, res.VariantAds[i])
		}
	}
	// Every post is authored before the paid campaign exists.
	firstCampaign := -1
	lastPost := -1
	for i, r := range a.requests {
		if strings.HasSuffix(r, "/posts") {
			lastPost = i
		}
		if strings.HasSuffix(r, "/campaigns") && firstCampaign < 0 {
			firstCampaign = i
		}
	}
	if lastPost > firstCampaign {
		t.Fatalf("a post was authored after the campaign create: %v", a.requests)
	}
}

// Off, the same input is the one-ad create: one post from the first variant, one ad, no AdIDs.
func TestAdPerVariant_OffKeepsTheOneAdCreate(t *testing.T) {
	a := &variantAPI{}
	in := variantInput(3)
	in.AdPerVariant = false
	res, err := a.client(t).CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	a.lock(t)
	if len(a.posts) != 1 || len(a.ads) != 1 || res.AdIDs != nil || res.VariantAds != nil || res.AdID != "ad_1" {
		t.Fatalf("posts/ads = %d/%d AdIDs=%v VariantAds=%v AdID=%q, want the one-ad create", len(a.posts), len(a.ads), res.AdIDs, res.VariantAds, res.AdID)
	}
}

// A refused ad for one variant keeps the others (no rollback) and marks the create degraded,
// naming the failed variant.
func TestAdPerVariant_FailedAdIsDegradedAndNamed(t *testing.T) {
	a := &variantAPI{failAd: 2}
	res, err := a.client(t).CreateCampaign(context.Background(), variantInput(3))
	if err != nil {
		t.Fatalf("a variant failure must not fail the create: %v", err)
	}
	if want := []string{"ad_1", "ad_3"}; !reflect.DeepEqual(res.AdIDs, want) {
		t.Fatalf("AdIDs = %v, want %v", res.AdIDs, want)
	}
	if !strings.Contains(res.AdWarning, "variant(s) 2 ") || !strings.Contains(res.AdWarning, "2 of 3") {
		t.Fatalf("AdWarning %q must name variant 2 and the count", res.AdWarning)
	}
	if res.VariantAds[1].Outcome != PromoAdRejected || res.VariantAds[1].PostID != "t3_p2" {
		t.Fatalf("VariantAds[1] = %+v, want ad_rejected with its post", res.VariantAds[1])
	}
	steps := strings.Join(res.Steps, "\n")
	if !strings.Contains(steps, "Variant 2: Ad creation failed: Reddit rejected the ad") || !strings.Contains(steps, "no confirmed ad for variant(s) 2 ") {
		t.Fatalf("steps do not name variant 2:\n%s", steps)
	}
}

// A refused post for one variant: that variant has no ad, the others do.
func TestAdPerVariant_FailedPostIsDegradedAndNamed(t *testing.T) {
	a := &variantAPI{failPost: 1}
	res, err := a.client(t).CreateCampaign(context.Background(), variantInput(2))
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if want := []string{"ad_1"}; !reflect.DeepEqual(res.AdIDs, want) || res.AdID != "ad_1" {
		t.Fatalf("AdIDs = %v AdID = %q, want [ad_1] (variant 2's ad)", res.AdIDs, res.AdID)
	}
	if res.VariantAds[0].Outcome != PromoAuthorRejected || res.VariantAds[1].AdID != "ad_1" {
		t.Fatalf("VariantAds = %+v", res.VariantAds)
	}
	if !strings.Contains(res.AdWarning, "variant(s) 1 ") {
		t.Fatalf("AdWarning %q must name variant 1", res.AdWarning)
	}
}

// Over the cap, or a blank headline, is refused before any request.
func TestAdPerVariant_RefusedBeforeAnyRequest(t *testing.T) {
	blank := variantInput(2)
	blank.Variants[1].Headline = "  "
	long := variantInput(2)
	long.Variants[1].Headline = strings.Repeat("x", maxRedditHeadlineRunes+1)
	for name, tc := range map[string]struct {
		in   CampaignInput
		want string
	}{
		"six variants":  {variantInput(MaxAdVariants + 1), "at most 5"},
		"blank":         {blank, "variants[2]: headline is required"},
		"long headline": {long, "variants[2]: headline is too long"},
	} {
		t.Run(name, func(t *testing.T) {
			a := &variantAPI{}
			res, err := a.client(t).CreateCampaign(context.Background(), tc.in)
			a.lock(t)
			var pr *domain.PreflightRefusal
			if res != nil || !errors.As(err, &pr) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("res=%v err=%v, want a PreflightRefusal naming %q", res, err, tc.want)
			}
			if len(a.requests) != 0 {
				t.Fatalf("requests sent before the refusal: %v", a.requests)
			}
		})
	}
	// Five variants is the cap, not over it.
	if _, err := ValidateAdVariants(variantInput(MaxAdVariants).Variants); err != nil {
		t.Fatalf("five variants must pass: %v", err)
	}
}

// A late image URL is obtained once per post, so no signed URL waits behind another variant.
func TestAdPerVariant_LateImageURLPerPost(t *testing.T) {
	a := &variantAPI{}
	in := variantInput(3)
	in.ImageURL = ""
	calls := 0
	in.ImageURLSource = func(context.Context) (string, error) {
		calls++
		return fmt.Sprintf("https://assets.example.org/img.png?sig=%d", calls), nil
	}
	if _, err := a.client(t).CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	a.lock(t)
	if calls != 3 {
		t.Fatalf("ImageURLSource called %d times, want 3", calls)
	}
	for i, p := range a.posts {
		content, _ := p["content"].([]any)
		item, _ := content[0].(map[string]any)
		if want := fmt.Sprintf("https://assets.example.org/img.png?sig=%d", i+1); item["media_url"] != want {
			t.Errorf("post %d media_url = %v, want %s", i+1, item["media_url"], want)
		}
	}
}

// The toggle cascades over every ad: activate lifts all ads, then the ad group, then the
// campaign; pause flips the campaign first, then the ad group, then all ads.
func TestUpdateCampaignTreeStatus_EveryAd(t *testing.T) {
	a := &variantAPI{}
	c := a.client(t)
	if err := c.UpdateCampaignTreeStatus(context.Background(), "camp_1", "ag_1", []string{"ad_1", " ", "ad_3"}, StatusActive); err != nil {
		t.Fatalf("activate: %v", err)
	}
	want := []string{
		"PATCH /api/v3/ad_accounts/t2_test/ads/ad_1",
		"PATCH /api/v3/ad_accounts/t2_test/ads/ad_3",
		"PATCH /api/v3/ad_accounts/t2_test/ad_groups/ag_1",
		"PATCH /api/v3/ad_accounts/t2_test/campaigns/camp_1",
	}
	if got := a.takeRequests(); !reflect.DeepEqual(got, want) {
		t.Fatalf("activate order = %v, want %v", got, want)
	}
	if err := c.UpdateCampaignTreeStatus(context.Background(), "camp_1", "ag_1", []string{"ad_1", "ad_3"}, StatusPaused); err != nil {
		t.Fatalf("pause: %v", err)
	}
	want = []string{want[3], want[2], want[0], want[1]}
	if got := a.takeRequests(); !reflect.DeepEqual(got, want) {
		t.Fatalf("pause order = %v, want %v", got, want)
	}
	if err := c.UpdateCampaignTreeStatus(context.Background(), "camp_1", "ag_1", []string{" "}, StatusActive); err == nil {
		t.Fatal("activate with no ad id must be refused")
	}
}

// A definite refusal of the campaign create after the variants' posts were authored returns them
// as a partial result, not (nil, err): the posts exist upstream, so the caller must keep the claim
// and report them instead of authoring them again on retry.
func TestAdPerVariant_CampaignRefusedAfterPostsKeepsThePosts(t *testing.T) {
	a := &variantAPI{failCampaign: true}
	res, err := a.client(t).CreateCampaign(context.Background(), variantInput(2))
	if err == nil {
		t.Fatal("a refused campaign create must fail the create")
	}
	if res == nil {
		t.Fatalf("res = nil, want a partial result naming the authored posts (err=%v)", err)
	}
	if len(res.VariantAds) != 2 || res.VariantAds[0].PostID != "t3_p1" || res.VariantAds[1].PostID != "t3_p2" {
		t.Fatalf("VariantAds = %+v, want both authored posts", res.VariantAds)
	}
	if res.CampaignID != "" || res.CampaignName == "" {
		t.Fatalf("CampaignID/CampaignName = %q/%q, want no id and the composed name", res.CampaignID, res.CampaignName)
	}
	if steps := strings.Join(res.Steps, "\n"); !strings.Contains(steps, "not attached to an ad") {
		t.Fatalf("steps do not say the posts are unattached:\n%s", steps)
	}
}

// The one-ad create keeps (nil, err) on a refused campaign: nothing the caller must track exists.
func TestAdPerVariant_OffCampaignRefusedIsNilResult(t *testing.T) {
	a := &variantAPI{failCampaign: true}
	in := variantInput(2)
	in.AdPerVariant = false
	in.ImageURL = ""
	in.PostURL = "https://www.reddit.com/r/linux/comments/abc123/x/"
	res, err := a.client(t).CreateCampaign(context.Background(), in)
	if err == nil || res != nil {
		t.Fatalf("res=%v err=%v, want (nil, err)", res, err)
	}
}

// A caller cancellation while variant 2's post is in flight stops the create before the campaign
// with a partial naming variant 1's post and variant 2's unconfirmed one.
func TestAdPerVariant_CancelDuringPostAuthoringKeepsThePosts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &variantAPI{cancelOn: "/posts", cancelAt: 2, cancel: cancel}
	res, err := a.client(t).CreateCampaign(ctx, variantInput(3))
	a.lock(t)
	if err == nil || res == nil {
		t.Fatalf("res=%v err=%v, want a partial and an error", res, err)
	}
	if res.VariantAds[0].PostID != "t3_p1" || res.VariantAds[1].Outcome != PromoAuthorCanceledInFlight || res.VariantAds[2].Outcome != "" {
		t.Fatalf("VariantAds = %+v", res.VariantAds)
	}
	for _, r := range a.requests {
		if strings.HasSuffix(r, "/campaigns") {
			t.Fatalf("the campaign was created after the cancellation: %v", a.requests)
		}
	}
}

// A cancellation before variant 2's post is sent, with variant 1's post authored, returns the
// partial and records a step for variant 2.
func TestAdPerVariant_CancelBeforeSecondPostKeepsTheFirst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &variantAPI{}
	in := variantInput(3)
	in.ImageURL = ""
	calls := 0
	in.ImageURLSource = func(context.Context) (string, error) {
		calls++
		if calls == 2 {
			cancel()
		}
		return "https://assets.example.org/img.png", nil
	}
	res, err := a.client(t).CreateCampaign(ctx, in)
	a.lock(t)
	if err == nil || res == nil {
		t.Fatalf("res=%v err=%v, want a partial and an error", res, err)
	}
	if res.VariantAds[0].PostID != "t3_p1" || res.VariantAds[1].Outcome != PromoAuthorCanceledBeforeSend {
		t.Fatalf("VariantAds = %+v", res.VariantAds)
	}
	if steps := strings.Join(res.Steps, "\n"); !strings.Contains(steps, "Variant 2: ") {
		t.Fatalf("no step for variant 2:\n%s", steps)
	}
	if len(a.posts) != 1 {
		t.Fatalf("posts = %d, want 1", len(a.posts))
	}
}

// A cancellation while variant 2's ad is in flight returns the partial with variant 1's ad, and
// the warning names every variant left without a confirmed ad, not only the cancelled one.
func TestAdPerVariant_CancelDuringAdCreationNamesEveryUnattachedVariant(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &variantAPI{cancelOn: "/ads", cancelAt: 2, cancel: cancel}
	res, err := a.client(t).CreateCampaign(ctx, variantInput(3))
	a.lock(t)
	if err == nil || res == nil {
		t.Fatalf("res=%v err=%v, want a partial and an error", res, err)
	}
	if want := []string{"ad_1"}; !reflect.DeepEqual(res.AdIDs, want) || res.AdID != "ad_1" || res.AdCount != 1 {
		t.Fatalf("AdIDs/AdID/AdCount = %v/%q/%d, want [ad_1]/ad_1/1", res.AdIDs, res.AdID, res.AdCount)
	}
	if !strings.Contains(res.AdWarning, "variant(s) 2, 3 ") {
		t.Fatalf("AdWarning %q must name variants 2 and 3", res.AdWarning)
	}
	if res.VariantAds[2].PostID != "t3_p3" {
		t.Fatalf("VariantAds[2] = %+v, want its authored post", res.VariantAds[2])
	}
}
