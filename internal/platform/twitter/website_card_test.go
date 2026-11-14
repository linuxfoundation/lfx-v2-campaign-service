// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func cardInput() CampaignInput {
	in := imageInput(testPNG(600))
	in.WebsiteCard = &WebsiteCardInput{Headline: "  KubeCon NA — register today "}
	return in
}

func TestNormalizeWebsiteCardHeadline(t *testing.T) {
	if got, err := NormalizeWebsiteCardHeadline("  Hi "); err != nil || got != "Hi" {
		t.Fatalf("got (%q, %v)", got, err)
	}
	for in, want := range map[string]string{
		" ":                     "headline is blank",
		strings.Repeat("a", 71): "at most 70 characters",
		"line\nbreak":           "no control character",
	} {
		_, err := NormalizeWebsiteCardHeadline(in)
		var tErr *TargetingError
		if !errors.As(err, &tErr) || tErr.Field != FieldWebsiteCard || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", in, err, want)
		}
	}
	if _, err := NormalizeWebsiteCardHeadline(strings.Repeat("é", 70)); err != nil {
		t.Errorf("70 characters must be accepted: %v", err)
	}
}

// The card is created once, after the line item and immediately before the authored tweet, with
// the image as its MEDIA and the headline and UTM'd destination as its DETAILS; the tweet carries
// card_uri (and not media_keys), and the result records the card.
func TestCreateCampaign_WebsiteCardAttachedToTheAuthoredTweet(t *testing.T) {
	f := newMediaFakeX()
	c := newMediaTestClient(t, f)
	res, err := c.CreateCampaign(context.Background(), cardInput())
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	f.mu.Lock()
	order, tweetQuery, cardBody := append([]string(nil), f.order...), f.tweetQuery, f.cardBody
	f.mu.Unlock()
	joined := strings.Join(order, ",")
	if !strings.Contains(joined, "lineitem,card,tweet,promoted") || f.count("card") != 1 {
		t.Fatalf("order = %v, want the card between the line item and the tweet, once", order)
	}
	if !strings.Contains(tweetQuery, "card_uri=card%3A%2F%2F1250013836219531264") || strings.Contains(tweetQuery, "media_keys") {
		t.Errorf("tweet query = %q, want card_uri and no media_keys", tweetQuery)
	}
	var body struct {
		Name       string `json:"name"`
		Components []struct {
			Type        string `json:"type"`
			MediaKey    string `json:"media_key"`
			Title       string `json:"title"`
			Destination struct {
				Type string `json:"type"`
				URL  string `json:"url"`
			} `json:"destination"`
		} `json:"components"`
	}
	if err := json.Unmarshal([]byte(cardBody), &body); err != nil {
		t.Fatalf("card body %q: %v", cardBody, err)
	}
	if !strings.HasPrefix(body.Name, "lfx-card-") || len(body.Components) != 2 ||
		body.Components[0].Type != "MEDIA" || body.Components[0].MediaKey != "3_777" ||
		body.Components[1].Type != "DETAILS" || body.Components[1].Title != "KubeCon NA — register today" ||
		body.Components[1].Destination.Type != "WEBSITE" ||
		!strings.HasPrefix(body.Components[1].Destination.URL, "https://events.lf.org/kubecon?") ||
		!strings.Contains(body.Components[1].Destination.URL, "utm_content=website-card") {
		t.Fatalf("card body = %+v", body)
	}
	if res.CardURI != "card://1250013836219531264" || res.PromotedTweetID != "pt1" {
		t.Errorf("CardURI=%q PromotedTweetID=%q", res.CardURI, res.PromotedTweetID)
	}
}

// A card that cannot be created publishes nothing: the create degrades as an authoring failure
// does, UNCONFIRMED when the card may exist.
func TestCreateCampaign_WebsiteCardFailureAuthorsNothing(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		response    string
		unconfirmed bool
	}{
		{"refused", http.StatusBadRequest, "", false},
		{"ambiguous", http.StatusBadGateway, "", true},
		{"no card_uri", 0, `{"data":{"name":"x"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMediaFakeX()
			f.cardStatus, f.cardResponse = tc.status, tc.response
			c := newMediaTestClient(t, f)
			res, err := c.CreateCampaign(context.Background(), cardInput())
			if err != nil {
				t.Fatalf("CreateCampaign: %v (a card failure is a degraded success)", err)
			}
			if f.count("tweet") != 0 || f.count("promoted") != 0 {
				t.Fatalf("a tweet was authored or promoted after the card failed")
			}
			if !strings.Contains(res.PromotedTweetWarning, "no tweet was authored or promoted") ||
				strings.Contains(res.PromotedTweetWarning, "UNCONFIRMED") != tc.unconfirmed {
				t.Errorf("warning = %q (unconfirmed want %v)", res.PromotedTweetWarning, tc.unconfirmed)
			}
			if res.CardURI != "" || res.PromotedTweetID != "" {
				t.Errorf("CardURI=%q PromotedTweetID=%q, want none", res.CardURI, res.PromotedTweetID)
			}
		})
	}
}

// Every variant's tweet carries the one card, created once.
func TestCreateCampaign_WebsiteCardOnEveryVariant(t *testing.T) {
	f := newMediaFakeX()
	c := newMediaTestClient(t, f)
	in := cardInput()
	in.TweetText = ""
	in.TweetVariants = []string{"one", "two"}
	if _, err := c.CreateCampaign(context.Background(), in); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	if f.count("card") != 1 || f.count("tweet") != 2 {
		t.Fatalf("cards=%d tweets=%d, want 1 and 2", f.count("card"), f.count("tweet"))
	}
	f.mu.Lock()
	q := f.tweetQuery
	f.mu.Unlock()
	if !strings.Contains(q, "card_uri=") {
		t.Errorf("the last variant's tweet lacks card_uri: %q", q)
	}
}

// A card without an image, without an authored tweet, or with a bad headline is refused before
// any request.
func TestCreateCampaign_WebsiteCardRefusedBeforeAnyRequest(t *testing.T) {
	for name, mut := range map[string]func(*CampaignInput){
		"no image":       func(in *CampaignInput) { in.Image = nil },
		"explicit tweet": func(in *CampaignInput) { in.TweetID = "123"; in.Image = nil },
		"blank headline": func(in *CampaignInput) { in.WebsiteCard.Headline = " " },
		"long headline":  func(in *CampaignInput) { in.WebsiteCard.Headline = strings.Repeat("h", 71) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newMediaFakeX()
			c := newMediaTestClient(t, f)
			in := cardInput()
			mut(&in)
			res, err := c.CreateCampaign(context.Background(), in)
			if res != nil || !isRefusal(err) || !strings.Contains(err.Error(), "card") {
				t.Fatalf("res=%v err=%v, want a card refusal", res, err)
			}
			f.mu.Lock()
			n := len(f.order)
			f.mu.Unlock()
			if n != 0 {
				t.Fatalf("%d requests before the refusal; want 0", n)
			}
		})
	}
}
