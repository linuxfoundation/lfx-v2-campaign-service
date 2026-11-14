// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// Website card on the authored tweet (#564 gap 6): twitterConfig.card, {"headline": ...}.
//
// X Ads API v12 cards (docs.x.com/x-ads-api/creatives/reference, "Cards"): POST
// accounts/:account_id/cards takes a JSON body naming the card and its COMPONENTS. A website card
// is a MEDIA component (a media_key from the account's Media Library — the image this create
// already uploads) and a DETAILS component carrying the title (the headline shown under the
// media) and a WEBSITE destination (the registration URL with this service's UTMs). The response
// carries a card_uri ("card://..."), which POST accounts/:account_id/tweet takes as card_uri to
// attach the card to the tweet it authors; the media then rides in the card, so media_keys is not
// also sent.
//
// WHEN AND HOW OFTEN. The card is created once per create, only when a tweet will actually be
// authored (after the reused-line-item checks), immediately before the first authoring write: a
// card is not public and not billable, so a card left by a create that then fails is clutter, not
// spend, and nothing is created for a create that reuses an earlier attempt's promoted tweet. A
// card POST that fails publishes nothing: the create degrades exactly as an authoring failure does
// (PromotedTweetWarning, created_degraded), with an UNCONFIRMED wording when the card may exist.
// Every variant's tweet (#564 gap 10) carries the same card, whose destination is tagged
// utm_content=website-card; each variant's inline link keeps its own utm_content=variant-N.
//
// NOT VERIFIED LIVE: built from the published reference; TWITTER_WEBSITE_CARD_ENABLED keeps it off
// the shared ad account until a live create confirms the body and the card_uri echo.

const (
	// FieldWebsiteCard is the twitterConfig key, named in refusals.
	FieldWebsiteCard = "card"
	// MaxWebsiteCardHeadline is X's website card title limit (70 characters).
	MaxWebsiteCardHeadline = 70
	// websiteCardUTMContent tags the card's destination.
	websiteCardUTMContent = "website-card"
	// websiteCardNamePrefix prefixes the card's deterministic name.
	websiteCardNamePrefix = "lfx-card-"
)

// cardURIRe is the shape of a card_uri X returns ("card://" and an opaque id). It is sent back as a
// query parameter and persisted, so any other shape from a 2xx is treated as no card at all.
var cardURIRe = regexp.MustCompile(`^card://[0-9A-Za-z]{1,64}$`)

// WebsiteCardInput is the website card requested for the authored tweet(s).
type WebsiteCardInput struct {
	// Headline is the card's title (DETAILS component), 1-70 characters.
	Headline string
}

// NormalizeWebsiteCardHeadline applies the gap 6 rule (#564): the headline is trimmed and must be
// 1-70 characters with no control character. Pure, so the dispatcher's synchronous preflight and
// CreateCampaign apply the same rule.
func NormalizeWebsiteCardHeadline(h string) (string, error) {
	t := strings.TrimSpace(h)
	if t == "" {
		return "", &TargetingError{Field: FieldWebsiteCard, Msg: "headline is blank"}
	}
	if utf8.RuneCountInString(t) > MaxWebsiteCardHeadline || strings.IndexFunc(t, unicode.IsControl) >= 0 {
		return "", &TargetingError{Field: FieldWebsiteCard, Msg: fmt.Sprintf("headline must be at most %d characters with no control character", MaxWebsiteCardHeadline)}
	}
	return t, nil
}

// websiteCardName is the card's name: deterministic from the campaign name, so the cards one
// campaign's creates leave are recognisable in X Ads Manager.
func websiteCardName(campaignName string) string {
	sum := sha256.Sum256([]byte(campaignName))
	return websiteCardNamePrefix + hex.EncodeToString(sum[:8])
}

// cardComponent is one entry of a card's components (only the fields a website card uses).
type cardComponent struct {
	Type        string           `json:"type"`
	MediaKey    string           `json:"media_key,omitempty"`
	Title       string           `json:"title,omitempty"`
	Destination *cardDestination `json:"destination,omitempty"`
}

type cardDestination struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// createWebsiteCard POSTs the website card and returns its card_uri. Paced and sent ONCE (a card
// has no idempotency key, and the caller's create is not retried in-call); the outcome classes are
// doBodyRequest's: a definite refusal, or an ambiguous one createOutcomeAmbiguous reports. A 2xx
// without a usable card_uri is an ambiguous transportError: the card may exist.
func (c *Client) createWebsiteCard(ctx context.Context, name, headline, mediaKey, destURL string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"name": name,
		"components": []cardComponent{
			{Type: "MEDIA", MediaKey: mediaKey},
			{Type: "DETAILS", Title: headline, Destination: &cardDestination{Type: "WEBSITE", URL: destURL}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("%w: encode website card: %w", ErrWriteNotSent, err)
	}
	if err := c.pace(ctx); err != nil {
		return "", fmt.Errorf("%w: %w", ErrWriteNotSent, err)
	}
	resp, err := c.doBodyRequest(ctx, c.accountURL()+"/cards", "cards", "application/json", body)
	if err != nil {
		return "", err
	}
	var echo struct {
		CardURI string `json:"card_uri"`
	}
	if resp == nil || len(resp.Data) == 0 || identityjson.Check(resp.raw) != nil || json.Unmarshal(resp.Data, &echo) != nil || !cardURIRe.MatchString(strings.TrimSpace(echo.CardURI)) {
		return "", &transportError{Method: http.MethodPost, Path: "cards", err: fmt.Errorf("acknowledged without a usable card_uri")}
	}
	return strings.TrimSpace(echo.CardURI), nil
}

// buildTwitterCardUTMURL is the card's destination: the registration URL with this service's UTMs
// and utm_content=website-card. validateRegistrationURL must be called before this.
func buildTwitterCardUTMURL(in CampaignInput) (string, error) {
	return buildTwitterUTMURLWithContent(in, websiteCardUTMContent)
}

// websiteCardFailure is the PromotedTweetWarning for a website card that could not be created:
// nothing was authored or promoted, and an ambiguous outcome says the card may exist (harmless: a
// card is neither public nor billable).
func websiteCardFailure(err error) string {
	if createOutcomeAmbiguous(err) {
		return fmt.Sprintf("website card creation UNCONFIRMED (%s) — a card may exist (cards are neither public nor billable); no tweet was authored or promoted; retry, or post the tweet manually and add it as a promoted tweet in X Ads Manager", err.Error())
	}
	return fmt.Sprintf("website card creation failed (%s) — no tweet was authored or promoted; retry, or post the tweet manually and add it as a promoted tweet in X Ads Manager", err.Error())
}
