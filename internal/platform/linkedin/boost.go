// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// Boosting an existing organization post (#673).
//
// A boost is a SPONSORED_UPDATES campaign whose one creative references a post the organization
// already published (content.reference), instead of a dark post this service authors. Sources,
// li-lms-2026-11, consulted 2026-11-26 (the client sends LinkedIn-Version 202611):
//
//   - "Create and Manage Creatives"
//     (https://learn.microsoft.com/en-us/linkedin/marketing/integrations/ads/account-structure/create-and-manage-creatives?view=li-lms-2026-11):
//     content "A reference must be a adInMailContent{id}, share{id}, or ugcPost{id}". Its error
//     table names what LinkedIn refuses only at the creative POST, after the campaign group and
//     campaign exist: UGC_RESHARE_CANNOT_BE_SPONSORED ("sponsoring a reshared UGC post is not
//     allowed"), UGC_INVALID_VISIBILITY_SETTING ("needs to be public to be sponsored"),
//     UGC_INVALID_FEED_DISTRIBUTION (MAIN_FEED for organic posts, NONE for dark posts),
//     DIRECT_SPONSORED_CONTENT_MISMATCH, REFERENCE_MEDIA_CATEGORY_AND_CAMPAIGN_FORMAT_MISMATCH,
//     UGC_EXCEEDING_IMAGE_LIMIT ("Image UGCs should have exactly one image to be sponsor").
//   - "Posts API"
//     (https://learn.microsoft.com/en-us/linkedin/marketing/community-management/shares/posts-api?view=li-lms-2026-11):
//     GET /rest/posts/{encoded ugcPostUrn|shareUrn} ("URNs included in the URL params must be URL
//     encoded"), whose body carries id, author, lifecycleState (DRAFT, PUBLISHED,
//     PUBLISH_REQUESTED, PUBLISH_FAILED), visibility, distribution.feedDistribution,
//     reshareContext (present on a reshare) and content. Its content table marks MultiImage, Poll
//     and Celebration posts as not sponsorable. Reading an organization's posts needs
//     r_organization_social and an ADMINISTRATOR, DIRECT_SPONSORED_CONTENT_POSTER or
//     CONTENT_ADMIN page role.
//
// So every one of those creative refusals that the post itself decides is checked here, with one
// GET, before the campaign group POST: a post that fails any of them would otherwise leave a
// campaign group and a PAUSED campaign with no creative.

// FormatBoost is a SPONSORED_UPDATES campaign whose single creative sponsors CampaignInput.BoostPost.
const FormatBoost = "boost"

// ErrBoostPostIneligible marks a boost post LinkedIn would refuse to sponsor from this ad account,
// found by reading the post before anything is created. Its sentences are service-authored; the
// only values they quote pass through domain.CallerValue.
var ErrBoostPostIneligible = errors.New("linkedin: the post cannot be boosted from this ad account")

// ValidateBoostPostURN reports whether urn is a post URN a boost may name: exactly
// urn:li:share:<digits> or urn:li:ugcPost:<digits>, the two references a creative accepts.
func ValidateBoostPostURN(urn string) error {
	if !isValidPostURN(urn) {
		return fmt.Errorf("%w: boostPost must be a LinkedIn post URN, urn:li:share:<id> or urn:li:ugcPost:<id>", ErrFormatInvalid)
	}
	return nil
}

// boostPostRead is the part of a Posts API GET the boost checks read.
type boostPostRead struct {
	ID             string          `json:"id"`
	Author         string          `json:"author"`
	LifecycleState string          `json:"lifecycleState"`
	Visibility     string          `json:"visibility"`
	ReshareContext json.RawMessage `json:"reshareContext"`
	Distribution   *struct {
		FeedDistribution string `json:"feedDistribution"`
	} `json:"distribution"`
	Content map[string]json.RawMessage `json:"content"`
}

// boostIneligible is a refusal of the post: nothing was created.
func boostIneligible(format string, args ...any) error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: "+format, append([]any{ErrBoostPostIneligible}, args...)...))
}

// verifyBoostPost reads postURN with the Posts API and refuses it unless LinkedIn would let this
// ad account sponsor it: authored by orgURN (the organization the account serves), PUBLISHED,
// PUBLIC, an organic MAIN_FEED post (not a dark post or a container post), not a reshare, and of
// a content type this service boosts — text only, an article, or a single image or video. It
// returns the campaign format the post needs: campaignFormatSingleVideo for a video post, which
// LinkedIn requires to be set on the campaign, and "" otherwise (LinkedIn sets it from the
// creative, as for every image campaign).
//
// It sends one GET and nothing else. A 404 or 403 is a refusal of the post (it does not exist, or
// this connection cannot read it); any other failure is returned wrapped, so a 401 still reaches
// the dispatcher as the connection's expiry. Either way nothing was created.
func (c *Client) verifyBoostPost(ctx context.Context, accountID, orgURN, postURN string) (string, error) {
	shown := domain.CallerValue(postURN)
	raw, err := c.doEntityGET(ctx, "posts/"+encodeURNForPath(postURN))
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) {
			switch apiErr.StatusCode {
			case http.StatusNotFound:
				return "", boostIneligible("post %s was not found (LinkedIn answered 404); check the URN, and that the post has not been deleted", shown)
			case http.StatusForbidden:
				return "", boostIneligible("this connection cannot read post %s (LinkedIn answered 403); reading an organization's posts needs the r_organization_social scope and an ADMINISTRATOR, DIRECT_SPONSORED_CONTENT_POSTER or CONTENT_ADMIN role on its page", shown)
			}
		}
		return "", fmt.Errorf("reading boost post %s failed before anything was created; a retry reads it again: %w", shown, err)
	}
	if err := identityjson.Check(raw); err != nil {
		return "", fmt.Errorf("LinkedIn's answer for boost post %s could not be trusted; nothing was created: %w", shown, err)
	}
	var post boostPostRead
	if err := json.Unmarshal(raw, &post); err != nil {
		return "", fmt.Errorf("LinkedIn's answer for boost post %s is not a post; nothing was created", shown)
	}
	if post.ID != postURN {
		return "", fmt.Errorf("LinkedIn's answer for boost post %s describes post %s; nothing was created", shown, domain.CallerValue(post.ID))
	}
	if post.Author != orgURN {
		return "", boostIneligible("post %s is authored by %s, not by %s, the organization ad account %s serves; only that organization's own posts can be boosted here", shown, domain.CallerValue(post.Author), orgURN, accountID)
	}
	if post.LifecycleState != "PUBLISHED" {
		return "", boostIneligible("post %s has lifecycleState %s; only a PUBLISHED post can be boosted", shown, domain.CallerValue(post.LifecycleState))
	}
	if post.Visibility != "PUBLIC" {
		return "", boostIneligible("post %s has visibility %s; LinkedIn sponsors only a PUBLIC post", shown, domain.CallerValue(post.Visibility))
	}
	if len(post.ReshareContext) > 0 && !isJSONNullRaw(post.ReshareContext) {
		return "", boostIneligible("post %s is a reshare; LinkedIn does not allow sponsoring a reshared post (UGC_RESHARE_CANNOT_BE_SPONSORED), so boost the original post instead", shown)
	}
	feed := ""
	if post.Distribution != nil {
		feed = post.Distribution.FeedDistribution
	}
	if feed != "MAIN_FEED" {
		return "", boostIneligible("post %s has feedDistribution %s; a boost sponsors an organic MAIN_FEED post (a dark post is created with variants instead)", shown, domain.CallerValue(feed))
	}
	return boostCampaignFormat(shown, post.Content)
}

// boostCampaignFormat decides the campaign format a post's content needs, and refuses content
// this service does not boost. LinkedIn does not sponsor multi-image, poll or celebration posts;
// documents, carousels and other kinds are sponsorable but not built here yet.
func boostCampaignFormat(shown string, content map[string]json.RawMessage) (string, error) {
	if len(content) == 0 {
		return "", nil // text only
	}
	if len(content) != 1 {
		return "", boostIneligible("post %s carries more than one content kind; a boost supports a text-only, article, single-image or video post", shown)
	}
	for kind, raw := range content {
		switch kind {
		case "article":
			return "", nil
		case "media":
			var media struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw, &media) != nil {
				return "", boostIneligible("post %s has media content that could not be read; a boost supports a text-only, article, single-image or video post", shown)
			}
			switch {
			case strings.HasPrefix(media.ID, "urn:li:video:"):
				return campaignFormatSingleVideo, nil
			case strings.HasPrefix(media.ID, "urn:li:image:"):
				return "", nil
			}
			return "", boostIneligible("post %s carries media %s; a boost supports a text-only, article, single-image or video post", shown, domain.CallerValue(media.ID))
		case "multiImage", "poll", "celebration":
			return "", boostIneligible("post %s is a %s post, which LinkedIn does not sponsor", shown, kind)
		}
		return "", boostIneligible("post %s carries %s content; a boost supports a text-only, article, single-image or video post", shown, domain.CallerValue(kind))
	}
	return "", nil
}

// isJSONNullRaw reports whether raw is the JSON literal null.
func isJSONNullRaw(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// boostCreativeName is the boost creative's name: the event and the post, so Campaign Manager
// shows which post the ad sponsors.
func boostCreativeName(eventName, postURN string) string {
	return fmt.Sprintf("%s | boost %s", eventName, trailingID(postURN))
}

// ensureBoostCreative attaches the one DRAFT creative sponsoring postURN to campaignID. On a
// campaign found by name (reused), it first looks for a creative of that campaign already
// referencing postURN (the creatives finder's contentReferences filter) and returns it rather
// than adding a second ad for the same post: unlike a dark post, a boost's creative has a natural
// key. A new campaign holds no creative, so it needs no lookup. reused reports a found creative.
// A found creative keeps whatever status it has.
//
// The lookup's failure is returned plain: nothing was posted. The POST's failures are createCreative's.
func (c *Client) ensureBoostCreative(ctx context.Context, accountID, campaignID string, campaignReused bool, postURN, adName string) (urn string, reused bool, lookupErr, createErr error) {
	if campaignReused {
		found, err := c.findCreativeURNs(ctx, accountID, campaignID, postURN)
		if err != nil {
			return "", false, err, nil
		}
		if len(found) > 0 {
			return found[0], true, nil, nil
		}
	}
	// No leadgenCallToAction: a boost is never a lead generation create (ValidateLeadGen).
	urn, err := c.createCreative(ctx, accountID, campaignID, postURN, adName, nil)
	return urn, false, nil, err
}

// finishBoost is CreateCampaign's creative step for a boost (#673): the one creative sponsoring
// postURN, created, or found on a reused campaign. Its failures follow the single-image loop's:
// an ambiguous POST is UNCONFIRMED and a definite one failed, each with the partial result
// carrying the group and campaign. There is no dark post, so nothing else can be orphaned.
func (c *Client) finishBoost(ctx context.Context, accountID, groupName, groupID, campaignName, campaignID string, campaignReused bool, eventName, postURN string, steps []string, partial func(*CampaignResult) *CampaignResult, finish func(int) *CampaignResult) (*CampaignResult, error) {
	creativeURN, found, lookupErr, err := c.ensureBoostCreative(ctx, accountID, campaignID, campaignReused, postURN, boostCreativeName(eventName, postURN))
	if lookupErr != nil {
		return partial(c.buildResult(accountID, groupName, groupID, campaignName, campaignID, 0, steps)),
			fmt.Errorf("looking for a creative of reused campaign %q that already sponsors post %s failed; no creative was created, and a retry looks again: %w — group %q and campaign %q already exist", campaignID, postURN, lookupErr, groupID, campaignID)
	}
	if err != nil {
		if createOutcomeAmbiguous(err) {
			steps = append(steps, fmt.Sprintf("Boost creative for post %s outcome UNCONFIRMED (timeout, server error, or an in-flight cancellation); it may have been created — verify before recreating", postURN))
			return partial(c.buildResult(accountID, groupName, groupID, campaignName, campaignID, 0, steps)),
				fmt.Errorf("boost creative for post %s UNCONFIRMED: %w — group %q and campaign %q already exist; the creative MAY have been created; inspect the returned partial result", postURN, err, groupID, campaignID)
		}
		return partial(c.buildResult(accountID, groupName, groupID, campaignName, campaignID, 0, steps)),
			fmt.Errorf("boost creative for post %s failed: %w — group %q and campaign %q already exist; inspect the returned partial result", postURN, err, groupID, campaignID)
	}
	if found {
		steps = append(steps, fmt.Sprintf("Creative already sponsoring post %s found on campaign %s: %s (not created again)", postURN, campaignID, creativeURN))
	} else {
		steps = append(steps, fmt.Sprintf("Creative (DRAFT) sponsoring post %s: %s", postURN, creativeURN))
	}
	result := finish(1)
	result.Steps = steps
	result.BoostedPostURN = postURN
	return result, nil
}
