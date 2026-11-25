// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
//
// That read is best-effort on a 403 only. A 403 means this connection cannot read posts (its token
// lacks r_organization_social), not that the post is ineligible, and the scope is not needed to
// sponsor one: probed 2026-11-26 on LinkedIn-Version 202611, a token without it got 403 on the
// Posts API GET and yet 201 on the creative POST {content: {reference: <share>}} against a paused
// campaign with no format, which LinkedIn then set to STANDARD_UPDATE itself. So on a 403 the
// pre-checks are skipped and the creative POST decides; its refusal (UGC_FORBIDDEN when the
// connection's member has no sponsoring role on the page, or one of the UGC_* codes above) comes
// back through finishBoost's creative-failure path, as a partial result with the PAUSED campaign.

// FormatBoost is a SPONSORED_UPDATES campaign whose single creative sponsors CampaignInput.BoostPost.
const FormatBoost = "boost"

// ErrBoostPostIneligible marks a boost post LinkedIn would refuse to sponsor from this ad account,
// found by reading the post before anything is created, or, when the post cannot be read, by reading
// its creative back (checkBoostCreativeAuthor). Its sentences are service-authored; the
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
// creative, as for every image campaign). checked reports whether the post was read and checked.
//
// It sends one GET and nothing else. A 404 is a refusal of the post (it does not exist). A 403
// means this connection cannot read posts (it lacks r_organization_social), not that the post is
// ineligible: it returns ("", false, nil), logging that the pre-checks were skipped, so the create
// goes on with no campaign format and LinkedIn's creative POST decides (see the package note
// above). Any other failure is returned wrapped, so a 401 still reaches the dispatcher as the
// connection's expiry; nothing was created.
func (c *Client) verifyBoostPost(ctx context.Context, accountID, orgURN, postURN string) (format string, checked bool, err error) {
	shown := domain.CallerValue(postURN)
	raw, err := c.doEntityGET(ctx, "posts/"+encodeURNForPath(postURN))
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) {
			switch apiErr.StatusCode {
			case http.StatusNotFound:
				return "", false, boostIneligible("post %s was not found (LinkedIn answered 404); check the URN, and that the post has not been deleted", shown)
			case http.StatusForbidden:
				slog.WarnContext(ctx, "linkedin boost: post pre-checks skipped because the connection cannot read posts (r_organization_social); LinkedIn's creative POST decides whether the post can be sponsored",
					"post_urn", postURN, "ad_account_id", accountID)
				return "", false, nil
			}
		}
		return "", false, fmt.Errorf("reading boost post %s failed before anything was created; a retry reads it again: %w", shown, err)
	}
	format, err = checkBoostPost(accountID, orgURN, postURN, raw)
	return format, err == nil, err
}

// checkBoostPost runs verifyBoostPost's checks on the Posts API answer raw.
func checkBoostPost(accountID, orgURN, postURN string, raw []byte) (string, error) {
	shown := domain.CallerValue(postURN)
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
		// A member's person URN is a linked pseudonym the caller never supplied: name it only
		// when the author is an organization, so it cannot reach the job's failure message.
		who := "a LinkedIn member"
		if strings.HasPrefix(post.Author, "urn:li:organization:") {
			who = domain.CallerValue(post.Author)
		}
		return "", boostIneligible("post %s is authored by %s, not by %s, the organization ad account %s serves; only that organization's own posts can be boosted here", shown, who, orgURN, accountID)
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

// boostCreativeCodes are the Creatives API error codes a boost's creative POST can answer about
// the post itself, each with the service's sentence for it. A code outside this list is never
// quoted: the envelope is upstream text.
var boostCreativeCodes = map[string]string{
	"UGC_FORBIDDEN":                   "the connection's LinkedIn member needs a sponsoring role (ADMINISTRATOR, DIRECT_SPONSORED_CONTENT_POSTER or CONTENT_ADMIN) on the organization page that authored it",
	"UGC_RESHARE_CANNOT_BE_SPONSORED": "the post is a reshare, which LinkedIn does not sponsor; boost the original post instead",
	"UGC_INVALID_VISIBILITY_SETTING":  "LinkedIn sponsors only a PUBLIC post",
	"UGC_INVALID_FEED_DISTRIBUTION":   "a boost sponsors an organic MAIN_FEED post",
}

// boostCreativeRefusal explains a definite creative POST failure whose LinkedIn error envelope
// carries one of boostCreativeCodes (as its code, or an input error's), or returns "" when it
// carries none. The envelope is read only when identityjson accepts it.
func boostCreativeRefusal(err error, postURN, accountID string) string {
	var ae *apiError
	if !errors.As(err, &ae) || len(ae.raw) == 0 || identityjson.Check(ae.raw) != nil {
		return ""
	}
	var env errorEnvelope
	if json.Unmarshal(ae.raw, &env) != nil {
		return ""
	}
	codes := []string{}
	if env.Code != nil {
		codes = append(codes, *env.Code)
	}
	if env.ErrorDetails != nil {
		for _, ie := range append(append([]inputErrorWire(nil), env.ErrorDetails.InputErrors...), env.ErrorDetails.ConditionalInputErrors...) {
			codes = append(codes, ie.Code)
		}
	}
	for _, code := range codes {
		if why, ok := boostCreativeCodes[code]; ok {
			return fmt.Sprintf("LinkedIn refused to sponsor post %s from ad account %s (%s): %s", domain.CallerValue(postURN), accountID, code, why)
		}
	}
	return ""
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
// The lookup's failure is returned plain: nothing was posted. The lookup runs only on a reused
// campaign, so the group and campaign both existed before this call. The POST's failures are
// createCreative's.
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
// postURN, created, or found on a reused campaign. It runs under the event's group lock, so two
// same-name creates in this process cannot both find no creative and post one each.
//
// A failed lookup returns (nil, err), like the group-widening and legacy-name failures before it:
// it runs only on a campaign found by name, in a group found by name, so this call created
// neither, and posted nothing. A partial here would be recorded with the campaign id, as
// created_degraded (which the orchestrator reuses as complete, so a repeated request would report
// success without the lookup) or as pending (which refuses every retry as "reconciliation
// required"); either way the lookup the error promises would never run. With (nil, err) the claim
// is released and a retry finds the group and campaign by name again and repeats the lookup.
//
// The creative POST's failures follow the single-image loop's: an ambiguous POST is UNCONFIRMED
// and a definite one failed, each with the partial result carrying the group and campaign. There
// is no dark post, so nothing else can be orphaned.
//
// When the post could not be read first (postChecked false: the connection lacks
// r_organization_social), the creative is read back and checked against orgURN by
// checkBoostCreativeAuthor before anything is reported, so that path fails closed too.
func (c *Client) finishBoost(ctx context.Context, accountID, groupName, groupID, campaignName, campaignID string, campaignReused bool, eventName, postURN, orgURN string, postChecked bool, steps []string, partial func(*CampaignResult) *CampaignResult, finish func(int) *CampaignResult) (*CampaignResult, error) {
	creativeURN, found, lookupErr, err := c.ensureBoostCreative(ctx, accountID, campaignID, campaignReused, postURN, boostCreativeName(eventName, postURN))
	if lookupErr != nil {
		return nil, fmt.Errorf("looking for a creative of campaign %q (found by name in group %q) that already sponsors post %s failed; this call created nothing and posted no creative, and a retry finds the campaign again and repeats the lookup: %w", campaignID, groupID, postURN, lookupErr)
	}
	if err != nil {
		if createOutcomeAmbiguous(err) {
			steps = append(steps, fmt.Sprintf("Boost creative for post %s outcome UNCONFIRMED (timeout, server error, or an in-flight cancellation); it may have been created — verify before recreating", postURN))
			return partial(c.buildResult(accountID, groupName, groupID, campaignName, campaignID, 0, steps)),
				fmt.Errorf("boost creative for post %s UNCONFIRMED: %w — group %q and campaign %q already exist; the creative MAY have been created; inspect the returned partial result", postURN, err, groupID, campaignID)
		}
		if why := boostCreativeRefusal(err, postURN, accountID); why != "" {
			steps = append(steps, why)
			// Only a campaign this call created is known to be PAUSED: one found by name may be ACTIVE.
			paused := ""
			if !campaignReused {
				paused = " (the campaign is PAUSED)"
			}
			return partial(c.buildResult(accountID, groupName, groupID, campaignName, campaignID, 0, steps)),
				fmt.Errorf("boost creative for post %s failed: %s: %w — group %q and campaign %q already exist%s; inspect the returned partial result", postURN, why, err, groupID, campaignID, paused)
		}
		return partial(c.buildResult(accountID, groupName, groupID, campaignName, campaignID, 0, steps)),
			fmt.Errorf("boost creative for post %s failed: %w — group %q and campaign %q already exist; inspect the returned partial result", postURN, err, groupID, campaignID)
	}
	if !postChecked {
		if err := c.checkBoostCreativeAuthor(ctx, accountID, orgURN, campaignID, postURN, creativeURN, !found, campaignReused); err != nil {
			return nil, err
		}
		steps = append(steps, fmt.Sprintf("Boost creative %s read back: the post is authored by %s", creativeURN, orgURN))
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

// boostCreativeRead is the part of a Creatives API GET a boost's author check reads: LinkedIn
// answers a creative made with content.reference as content.standardUpdateAd (probed on
// 26 Nov 2026), naming the post and its author.
type boostCreativeRead struct {
	ID      string `json:"id"`
	Content struct {
		StandardUpdateAd *struct {
			ContentAuthor string `json:"contentAuthor"`
			Reference     string `json:"reference"`
		} `json:"standardUpdateAd"`
	} `json:"content"`
}

// checkBoostCreativeAuthor reads creativeURN back and returns nil only when it sponsors postURN
// and orgURN authored that post. It stands in for verifyBoostPost's author check when the
// connection cannot read posts: without it, a caller could boost any post the connection's
// member may sponsor, from another organization's page included.
//
// Any other outcome is an error with no partial result, so nothing is recorded and the campaign
// cannot be activated through this service: a retry finds the group and campaign by name and
// checks again. A creative this call created (created) is deleted first; it is a DRAFT, which
// LinkedIn lets a DELETE remove (a DRAFT cannot be archived). A failed read is treated like a
// wrong author, since nothing then shows the creative is safe to keep. A creative found on a
// reused campaign is never deleted here: it may be serving, so the error names it instead.
func (c *Client) checkBoostCreativeAuthor(ctx context.Context, accountID, orgURN, campaignID, postURN, creativeURN string, created, campaignReused bool) error {
	shown := domain.CallerValue(postURN)
	why := ""
	raw, err := c.doEntityGET(ctx, fmt.Sprintf("adAccounts/%s/creatives/%s", accountID, encodeURNForPath(creativeURN)))
	switch {
	case err != nil:
		why = fmt.Sprintf("reading it back to check the post's author failed (%v)", err)
	case identityjson.Check(raw) != nil:
		why = "LinkedIn's answer for it could not be trusted"
	default:
		var read boostCreativeRead
		decodeErr := json.Unmarshal(raw, &read)
		ad := read.Content.StandardUpdateAd
		switch {
		case decodeErr != nil || ad == nil:
			why = "LinkedIn's answer for it names no sponsored post"
		case ad.Reference != postURN:
			why = fmt.Sprintf("it sponsors post %s, not %s", domain.CallerValue(ad.Reference), shown)
		case ad.ContentAuthor != orgURN:
			who := "a LinkedIn member"
			if strings.HasPrefix(ad.ContentAuthor, "urn:li:organization:") {
				who = domain.CallerValue(ad.ContentAuthor)
			}
			why = fmt.Sprintf("the post is authored by %s, not by %s, the organization ad account %s serves; only that organization's own posts can be boosted here", who, orgURN, accountID)
		default:
			return nil
		}
	}
	if !created {
		return fmt.Errorf("%w: creative %s, found on campaign %s for post %s, failed the author check: %s; remove it in Campaign Manager before boosting this post again", ErrBoostPostIneligible, creativeURN, campaignID, shown, why)
	}
	cleanup := fmt.Sprintf("the DRAFT creative %s was deleted", creativeURN)
	path := fmt.Sprintf("adAccounts/%s/creatives/%s", accountID, encodeURNForPath(creativeURN))
	if _, derr := c.doRequest(ctx, http.MethodDelete, path, nil, nil, nil, true); derr != nil {
		cleanup = fmt.Sprintf("deleting the DRAFT creative %s failed (%v), so delete it in Campaign Manager", creativeURN, derr)
	}
	left := fmt.Sprintf("campaign %s is left PAUSED with no ad", campaignID)
	if campaignReused {
		left = fmt.Sprintf("campaign %s (found by name) is left as it was", campaignID)
	}
	return fmt.Errorf("%w: boost creative for post %s failed the author check: %s; %s, and %s", ErrBoostPostIneligible, shown, why, cleanup, left)
}
