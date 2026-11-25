// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// carouselCards returns n cards whose sources record their minting on v.
func carouselCards(v *videoPostServer, n int) []CarouselCard {
	cards := make([]CarouselCard, n)
	for i := range cards {
		cards[i] = CarouselCard{
			ImageURLSource: func(context.Context) (string, error) {
				v.record(fmt.Sprintf("mint-%d", i+1))
				return fmt.Sprintf("https://lfx-api.example.org/_campaigns/creative-assets/tlf/b/c%d?exp=1&sig=s", i+1), nil
			},
			Headline:       fmt.Sprintf("Speaker %d", i+1),
			DestinationURL: fmt.Sprintf("https://events.example.org/s%d", i+1),
		}
	}
	return cards
}

// TestCreateCampaign_AuthorsACarouselPost pins the #661 CAROUSEL post: every card's late URL is
// minted after account verification, in card order, immediately before the post, which comes
// BEFORE the campaign; the body is the documented CAROUSEL shape with one content entry per card
// (media_url, caption, the card's UTM'd destination_url and the post's CTA); and it is one post
// and one ad even with one ad per variant on.
func TestCreateCampaign_AuthorsACarouselPost(t *testing.T) {
	v := &videoPostServer{}
	c := newVideoPostClient(t, v)
	in := lateSourceInput()
	in.CallToAction = "buy tickets"
	in.AdPerVariant = true
	in.Variants = []AdVariant{{Headline: "First"}, {Headline: "Second"}}
	in.CarouselCards = carouselCards(v, 3)
	if AdPerVariantApplies(in) {
		t.Fatal("AdPerVariantApplies = true for a carousel create, want false")
	}
	res, err := c.CreateCampaign(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if got := strings.Join(v.events[:6], ","); got != "verify,mint-1,mint-2,mint-3,post,campaign" {
		t.Fatalf("event order = %v, want verify,mint-1..3,post,campaign first", v.events)
	}
	ads := 0
	for _, e := range v.events {
		if e == "ad" {
			ads++
		}
	}
	if ads != 1 {
		t.Fatalf("ads = %d, want one", ads)
	}
	if v.post["type"] != "CAROUSEL" || v.post["headline"] != "First" || v.post["allow_comments"] != false {
		t.Fatalf("post = %v, want a CAROUSEL post headlined First without comments", v.post)
	}
	if _, ok := v.post["thumbnail_url"]; ok {
		t.Fatal("a carousel post must not carry thumbnail_url")
	}
	content, _ := v.post["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content = %v, want three entries", v.post["content"])
	}
	for i, raw := range content {
		item, _ := raw.(map[string]any)
		if !strings.Contains(fmt.Sprint(item["media_url"]), fmt.Sprintf("/c%d?", i+1)) {
			t.Fatalf("card %d media_url = %v", i+1, item["media_url"])
		}
		if item["caption"] != fmt.Sprintf("Speaker %d", i+1) || item["call_to_action"] != "Buy Tickets" {
			t.Fatalf("card %d = %v, want its headline as caption and the Buy Tickets CTA", i+1, item)
		}
		dest, _ := item["destination_url"].(string)
		if !strings.HasPrefix(dest, fmt.Sprintf("https://events.example.org/s%d?", i+1)) || !strings.Contains(dest, "utm_source=reddit") ||
			!strings.Contains(dest, fmt.Sprintf("utm_content=card-%d", i+1)) {
			t.Fatalf("card %d destination_url = %q, want the card URL with the UTM parameters", i+1, dest)
		}
	}
	joined := strings.Join(res.Steps, "\n")
	if !strings.Contains(joined, "Authored promoted carousel post: t3_video (3 cards") || strings.Contains(joined, "sig=") {
		t.Fatalf("steps do not record the carousel post, or carry a signed URL: %s", joined)
	}
}

// TestCreateCampaign_CarouselPostOutcomes: a refused post degrades (campaign and ad group, no
// ad); a campaign refused after the post, or after an UNCONFIRMED post, returns a partial result
// naming it, never (nil, err), so the claim is kept.
func TestCreateCampaign_CarouselPostOutcomes(t *testing.T) {
	cases := []struct {
		name               string
		postCode, campCode int
		wantErr            bool
		wantStep           string
	}{
		{"post refused degrades", http.StatusBadRequest, 0, false, ""},
		{"campaign refused after the post", 0, http.StatusBadRequest, true, "promoted carousel post t3_video above exists and is not attached"},
		{"campaign refused after an unconfirmed post", http.StatusInternalServerError, http.StatusBadRequest, true, "promoted carousel post above may exist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &videoPostServer{postCode: tc.postCode, campaignCode: tc.campCode}
			c := newVideoPostClient(t, v)
			in := lateSourceInput()
			in.CarouselCards = carouselCards(v, 2)
			res, err := c.CreateCampaign(context.Background(), in)
			if res == nil {
				t.Fatalf("result = nil with %v; the post would be forgotten", err)
			}
			if !tc.wantErr {
				if err != nil || res.CampaignID == "" || res.AdID != "" || strings.TrimSpace(res.AdWarning) == "" {
					t.Fatalf("want a degraded campaign with no ad, got %+v, %v", res, err)
				}
				return
			}
			if err == nil {
				t.Fatal("want the campaign refusal as an error")
			}
			if joined := strings.Join(res.Steps, "\n"); !strings.Contains(joined, tc.wantStep) {
				t.Fatalf("steps do not say %q: %s", tc.wantStep, joined)
			}
		})
	}
}

// TestCreateCampaign_CarouselInputsRefusedBeforeAnyMutation covers every pre-create refusal: a
// card count outside 2..6, a card without a source, a blank or over-long headline, a bad
// destination, a mix with another creative, a source error and a source returning a bad URL.
// Each is (nil, err) with no mutating request and no URL or headline echoed.
func TestCreateCampaign_CarouselInputsRefusedBeforeAnyMutation(t *testing.T) {
	good := func(context.Context) (string, error) { return lateThumb, nil }
	cases := []struct {
		name string
		mut  func(*CampaignInput, *videoPostServer)
		want string
	}{
		{"one card", func(in *CampaignInput, v *videoPostServer) { in.CarouselCards = carouselCards(v, 1) }, "1 cards supplied"},
		{"seven cards", func(in *CampaignInput, v *videoPostServer) { in.CarouselCards = carouselCards(v, 7) }, "7 cards supplied"},
		{"card without a source", func(in *CampaignInput, v *videoPostServer) {
			in.CarouselCards = carouselCards(v, 2)
			in.CarouselCards[1].ImageURLSource = nil
		}, "carouselCards[2]: an image is required"},
		{"blank headline", func(in *CampaignInput, v *videoPostServer) {
			in.CarouselCards = carouselCards(v, 2)
			in.CarouselCards[0].Headline = "  "
		}, "carouselCards[1]: headline is required"},
		{"headline over 180", func(in *CampaignInput, v *videoPostServer) {
			in.CarouselCards = carouselCards(v, 2)
			in.CarouselCards[0].Headline = strings.Repeat("é", 181)
		}, "headline is 181 characters (max 180)"},
		{"non-http destination", func(in *CampaignInput, v *videoPostServer) {
			in.CarouselCards = carouselCards(v, 2)
			in.CarouselCards[1].DestinationURL = "javascript:alert(1)"
		}, "absolute http or https URL"},
		{"malformed destination query", func(in *CampaignInput, v *videoPostServer) {
			in.CarouselCards = carouselCards(v, 2)
			in.CarouselCards[1].DestinationURL = "https://e.example/r?token=%zz"
		}, "malformed query string"},
		{"with an image source", func(in *CampaignInput, v *videoPostServer) {
			in.CarouselCards = carouselCards(v, 2)
			in.ImageURLSource = good
		}, "cannot be combined"},
		{"with a video", func(in *CampaignInput, v *videoPostServer) {
			in.CarouselCards = carouselCards(v, 2)
			in.VideoURLSource, in.ThumbnailURLSource = good, good
		}, "cannot be combined"},
		{"with a post URL", func(in *CampaignInput, v *videoPostServer) {
			in.CarouselCards = carouselCards(v, 2)
			in.PostURL = "https://www.reddit.com/r/test/comments/abc123/x/"
		}, "cannot be combined"},
		{"source error", func(in *CampaignInput, v *videoPostServer) {
			in.CarouselCards = carouselCards(v, 2)
			in.CarouselCards[1].ImageURLSource = func(context.Context) (string, error) { return "", errors.New("mint failed") }
		}, "carousel card 2 image"},
		{"source returns a bad URL", func(in *CampaignInput, v *videoPostServer) {
			in.CarouselCards = carouselCards(v, 2)
			in.CarouselCards[0].ImageURLSource = func(context.Context) (string, error) { return "ftp://x/y?sig=s", nil }
		}, "carousel card 1 image"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := &videoPostServer{}
			c := newVideoPostClient(t, v)
			in := lateSourceInput()
			tc.mut(&in, v)
			res, err := c.CreateCampaign(context.Background(), in)
			if err == nil || res != nil {
				t.Fatalf("want (nil, err), got (%+v, %v)", res, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not say %q", err, tc.want)
			}
			v.mu.Lock()
			mutated := v.mutated
			v.mu.Unlock()
			if mutated {
				t.Fatal("a mutating request was made")
			}
			if msg := err.Error(); strings.Contains(msg, "sig=") || strings.Contains(msg, "%zz") || strings.Contains(msg, "éé") {
				t.Fatalf("error echoes caller input: %v", err)
			}
		})
	}
}

// TestCarouselCardDestinations_TagEachCard pins #661's attribution decision: every card's
// destination carries the post's UTM parameters with utm_content=card-<k> (1-based), never the
// post-level variant-1, and keeps the card's own path, query and fragment.
func TestCarouselCardDestinations_TagEachCard(t *testing.T) {
	cases := []struct {
		name, dest, wantPrefix, wantKeep string
	}{
		{"plain", "https://events.example.org/speakers/a", "https://events.example.org/speakers/a?", ""},
		{"existing query kept", "https://events.example.org/s?track=ai", "https://events.example.org/s?", "track=ai"},
		{"fragment stays last", "https://events.example.org/s#tickets", "https://events.example.org/s?", "#tickets"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := lateSourceInput()
			in.CarouselCards = make([]CarouselCard, 4)
			for i := range in.CarouselCards {
				in.CarouselCards[i] = CarouselCard{
					ImageURLSource: func(context.Context) (string, error) { return lateThumb, nil },
					Headline:       "Card",
					DestinationURL: tc.dest,
				}
			}
			cards, err := resolveCarouselCards(context.Background(), in)
			if err != nil {
				t.Fatalf("resolveCarouselCards: %v", err)
			}
			for k, c := range cards {
				u, perr := url.Parse(c.DestinationURL)
				if perr != nil {
					t.Fatalf("card %d destination %q does not parse: %v", k+1, c.DestinationURL, perr)
				}
				q := u.Query()
				if got, want := q.Get("utm_content"), fmt.Sprintf("card-%d", k+1); got != want {
					t.Fatalf("card %d utm_content = %q, want %q", k+1, got, want)
				}
				if q.Get("utm_source") != "reddit" || q.Get("utm_medium") != "paid-social" || q.Get("utm_campaign") == "" {
					t.Fatalf("card %d lost the post's UTM parameters: %q", k+1, c.DestinationURL)
				}
				if !strings.HasPrefix(c.DestinationURL, tc.wantPrefix) || (tc.wantKeep != "" && !strings.Contains(c.DestinationURL, tc.wantKeep)) {
					t.Fatalf("card %d destination = %q, want prefix %q keeping %q", k+1, c.DestinationURL, tc.wantPrefix, tc.wantKeep)
				}
				if tc.wantKeep == "#tickets" && !strings.HasSuffix(c.DestinationURL, "#tickets") {
					t.Fatalf("card %d fragment is not last: %q", k+1, c.DestinationURL)
				}
			}
		})
	}
}
