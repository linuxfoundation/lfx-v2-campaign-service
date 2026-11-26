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
	"time"

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
// its creative back (checkPostedBoostCreative), or on a reused campaign's creatives
// (checkReusedBoostCampaign). Its sentences are service-authored; the
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
// What it returns follows the dispatcher's rule: no result only when this call changed nothing
// upstream, so the claim is released and a retry repeats the step; a partial result whenever it
// did, so the claim is kept. A campaign found by name sits in a group found by name (a new group
// holds no campaign), so on a reused campaign everything before the creative POST creates nothing.
//
//   - Reused campaign (whether or not the post was read): every creative it already holds is
//     checked first (checkReusedBoostCampaign), since a boost of another post for the same event
//     shares the campaign, activation flips every creative, and an earlier boost may have left a
//     creative it could not verify or delete. One that cannot be checked (a failed listing or
//     read, an untrusted answer) returns no result; one checked and wrong returns the partial
//     marked BoostCreativeUnverified (pending, reconciliation required), naming it for removal.
//     Nothing is posted in either case, and nothing found is ever deleted: it may be serving.
//   - The lookup of a creative already sponsoring postURN: its failure returns no result, as above.
//   - The creative POST: an ambiguous POST is UNCONFIRMED and a definite one failed, each with the
//     partial result carrying the group and campaign.
//   - Without the post read (postChecked false: the connection lacks r_organization_social), a
//     found creative not covered by the scan, and the creative this call posted, are checked too.
//     A posted creative that fails, or cannot be checked, is deleted (checkPostedBoostCreative) and
//     the partial marked BoostCreativeUnverified is returned, since this call changed LinkedIn (as
//     the dispatcher keeps the claim for an expiry on any mutating request). The one exception is
//     a creative that could not be checked, deleted again, in a reused campaign: nothing this call
//     did remains, so it returns no result and a retry repeats the check. A creative checked and
//     wrong always returns the partial: the post is refused, and a retry would be refused again.
//
// A reused campaign's check costs one GET per creative it holds; LinkedIn caps a campaign at 100.
func (c *Client) finishBoost(ctx context.Context, accountID, groupName, groupID, campaignName, campaignID string, campaignReused bool, eventName, postURN, orgURN string, postChecked bool, steps []string, partial func(*CampaignResult) *CampaignResult, finish func(int) *CampaignResult) (*CampaignResult, error) {
	// unverified is the outcome for a creative checked and wrong, or posted by this call and not
	// verified: the partial kept as pending, so the claim is retained and no retry posts again.
	unverified := func(creativeURN string, err error) (*CampaignResult, error) {
		steps = append(steps, fmt.Sprintf("Boost creative %s did not pass the author check; campaign %s is kept for reconciliation", creativeURN, campaignID))
		r := partial(c.buildResult(accountID, groupName, groupID, campaignName, campaignID, 0, steps))
		r.BoostCreativeUnverified = true
		return r, err
	}
	scanned := map[string]bool{}
	if campaignReused {
		checked, bad, err := c.checkReusedBoostCampaign(ctx, accountID, orgURN, campaignID)
		switch {
		case bad != "":
			return unverified(bad, err)
		case err != nil:
			return nil, fmt.Errorf("checking the creatives campaign %q (found by name in group %q) already holds could not finish; this call created nothing and posted no creative, and a retry repeats the check: %w", campaignID, groupID, err)
		}
		for _, urn := range checked {
			scanned[urn] = true
		}
		steps = append(steps, fmt.Sprintf("Checked the %d creative(s) campaign %s already holds: each sponsors a post authored by %s", len(checked), campaignID, orgURN))
	}
	creativeURN, found, lookupErr, err := c.ensureBoostCreative(ctx, accountID, campaignID, campaignReused, postURN, boostCreativeName(eventName, postURN))
	if lookupErr != nil {
		return nil, fmt.Errorf("looking for a creative of campaign %q (found by name in group %q) that already sponsors post %s failed; this call created nothing and posted no creative, and a retry finds the campaign again and repeats the lookup: %w", campaignID, groupID, postURN, lookupErr)
	}
	if err != nil {
		if createOutcomeAmbiguous(err) {
			steps = append(steps, fmt.Sprintf("Boost creative for post %s outcome UNCONFIRMED (timeout, server error, an in-flight cancellation, or an untrusted answer); it may have been created — verify before recreating", postURN))
			r := partial(c.buildResult(accountID, groupName, groupID, campaignName, campaignID, 0, steps))
			// Without the post read, a creative that may exist has had no author check: kept as
			// pending (reconciliation required), never created_degraded, which the orchestrator
			// would reuse as complete without the check ever running.
			r.BoostCreativeUnverified = !postChecked
			return r, fmt.Errorf("boost creative for post %s UNCONFIRMED: %w — group %q and campaign %q already exist; the creative MAY have been created; inspect the returned partial result", postURN, err, groupID, campaignID)
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
	// With the post read, the creative needs no read-back: a found one was matched by the post's
	// own lookup (filtered by it, every page trusted) and a posted one references it, so either way
	// it sponsors the post verifyBoostPost confirmed the organization authored.
	if !postChecked {
		switch {
		case !found:
			if undone, err := c.checkPostedBoostCreative(ctx, accountID, orgURN, campaignID, postURN, creativeURN, campaignReused); err != nil {
				if undone && campaignReused {
					// Could not be checked, and deleted again, in a campaign this call did not create:
					// nothing this call did remains, so a retry repeats the check.
					return nil, fmt.Errorf("%w; this call created nothing that remains, and a retry repeats the check", err)
				}
				return unverified(creativeURN, err)
			}
		case !scanned[creativeURN]:
			// Found by the post's lookup but not listed by the scan (another process posted it in
			// between): checked on the scan's terms, so this call still changed nothing.
			verdict, why, readErr := c.judgeBoostCreative(ctx, accountID, orgURN, creativeURN, postURN)
			switch verdict {
			case creativeWrong:
				return unverified(creativeURN, fmt.Errorf("creative %s, found on campaign %s for post %s, failed the author check: %s; nothing was posted, so remove it in Campaign Manager before boosting into this campaign again: %w", creativeURN, campaignID, domain.CallerValue(postURN), why, ErrBoostPostIneligible))
			case creativeUnknown:
				return nil, fmt.Errorf("creative %s, found on campaign %s for post %s, could not be checked (%s); this call created nothing and posted no creative, and a retry repeats the check: %w", creativeURN, campaignID, domain.CallerValue(postURN), why, unknownCause(readErr))
			}
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

// boostCreativeDeleteTimeout bounds checkPostedBoostCreative's compensating DELETE, which runs
// detached from the job's context so a cancelled job still removes the creative.
const boostCreativeDeleteTimeout = 30 * time.Second

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

// creativeVerdict is what judgeBoostCreative concluded about one creative.
type creativeVerdict int

const (
	// creativeOK: it sponsors a post authored by the account's organization (and the requested
	// post, when one was named).
	creativeOK creativeVerdict = iota
	// creativeWrong: LinkedIn answered, the answer was trusted, and the creative fails the check.
	creativeWrong
	// creativeUnknown: nothing trustworthy was learned (a failed read, an untrusted or
	// undecodable answer, or an answer about another creative), so it was neither passed nor failed.
	creativeUnknown
)

// judgeBoostCreative reads creativeURN back and judges it: creativeOK when it sponsors a post
// authored by orgURN (and, when postURN is not empty, that post). why explains any other verdict;
// readErr is the read's own failure, set only for a creativeUnknown from a failed GET.
func (c *Client) judgeBoostCreative(ctx context.Context, accountID, orgURN, creativeURN, postURN string) (verdict creativeVerdict, why string, readErr error) {
	raw, readErr := c.doEntityGET(ctx, fmt.Sprintf("adAccounts/%s/creatives/%s", accountID, encodeURNForPath(creativeURN)))
	if readErr != nil {
		return creativeUnknown, "reading it back failed", readErr
	}
	if identityjson.Check(raw) != nil {
		return creativeUnknown, "LinkedIn's answer for it could not be trusted", nil
	}
	var read boostCreativeRead
	if json.Unmarshal(raw, &read) != nil {
		return creativeUnknown, "LinkedIn's answer for it is not a creative", nil
	}
	if read.ID != creativeURN {
		// The finder answers ids as sponsoredCreative URNs (probed 26 Nov), as this GET is asked by.
		return creativeUnknown, "LinkedIn's answer for it describes another creative", nil
	}
	ad := read.Content.StandardUpdateAd
	switch {
	case ad == nil:
		return creativeWrong, "it sponsors no organization post (it is not a boost creative)", nil
	case postURN != "" && ad.Reference != postURN:
		return creativeWrong, fmt.Sprintf("it sponsors post %s, not %s", domain.CallerValue(ad.Reference), domain.CallerValue(postURN)), nil
	case ad.ContentAuthor != orgURN:
		who := "a LinkedIn member"
		if strings.HasPrefix(ad.ContentAuthor, "urn:li:organization:") {
			who = domain.CallerValue(ad.ContentAuthor)
		}
		return creativeWrong, fmt.Sprintf("the post is authored by %s, not by %s, the organization ad account %s serves; only that organization's own posts can be boosted here", who, orgURN, accountID), nil
	}
	return creativeOK, "", nil
}

// unknownCause is what an error for a creativeUnknown wraps: the read's own error when the GET
// failed (a 401 keeps ErrCredentialsExpired), else errBoostCreativeUnchecked. Never
// ErrBoostPostIneligible: nothing showed the post is ineligible.
func unknownCause(readErr error) error {
	if readErr != nil {
		return readErr
	}
	return errBoostCreativeUnchecked
}

// errBoostCreativeUnchecked marks a boost creative LinkedIn answered about in a way that could not
// be trusted, so its author was neither confirmed nor refuted.
var errBoostCreativeUnchecked = errors.New("linkedin: the boost creative's author could not be checked")

// checkPostedBoostCreative checks the creative this call just posted and returns nil only when it
// sponsors postURN and orgURN authored that post. It stands in for verifyBoostPost's author check
// when the connection cannot read posts: without it, a caller could boost any post the
// connection's member may sponsor, from another organization's page included.
//
// Any other verdict deletes the creative (a DRAFT, which LinkedIn lets a DELETE remove but not
// archive) and returns an error; the error says whether the DELETE succeeded. undone reports a
// creative that could not be checked and was deleted, so nothing of it remains (finishBoost
// decides what that means for the claim). The DELETE runs on a context detached from the job's,
// with its own timeout, so a cancelled job still cleans up. The error wraps ErrBoostPostIneligible
// for a wrong creative, and the read's own error (a 401 keeps ErrCredentialsExpired) or
// errBoostCreativeUnchecked when it could not be checked.
func (c *Client) checkPostedBoostCreative(ctx context.Context, accountID, orgURN, campaignID, postURN, creativeURN string, campaignReused bool) (undone bool, err error) {
	verdict, why, readErr := c.judgeBoostCreative(ctx, accountID, orgURN, creativeURN, postURN)
	if verdict == creativeOK {
		return false, nil
	}
	cause := ErrBoostPostIneligible
	if verdict == creativeUnknown {
		cause = unknownCause(readErr)
	}
	cleanup := fmt.Sprintf("the DRAFT creative %s was deleted", creativeURN)
	path := fmt.Sprintf("adAccounts/%s/creatives/%s", accountID, encodeURNForPath(creativeURN))
	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), boostCreativeDeleteTimeout)
	defer cancel()
	deleted := true
	if _, derr := c.doRequest(delCtx, http.MethodDelete, path, nil, nil, nil, true); derr != nil {
		deleted = false
		cleanup = fmt.Sprintf("deleting the DRAFT creative %s failed (%v), so delete it in Campaign Manager", creativeURN, derr)
	}
	left := fmt.Sprintf("campaign %s is left PAUSED", campaignID)
	if campaignReused {
		left = fmt.Sprintf("campaign %s (found by name) is left as it was", campaignID)
	}
	return deleted && verdict == creativeUnknown, fmt.Errorf("boost creative for post %s did not pass the author check: %s; %s, and %s: %w", domain.CallerValue(postURN), why, cleanup, left, cause)
}

// checkReusedBoostCampaign checks the author of every creative campaignID already holds, for any
// post, reading the campaign's creatives with a listing identityjson must trust (an untrusted page
// could hide one). It returns the creatives checked. bad names the first creative checked and
// wrong, with an ErrBoostPostIneligible error; an error with bad empty means the check could not
// finish (a failed or untrusted listing, or a creative that could not be checked). It never
// deletes: a creative it finds may be serving.
func (c *Client) checkReusedBoostCampaign(ctx context.Context, accountID, orgURN, campaignID string) (checked []string, bad string, err error) {
	urns, err := c.listBoostCreativeURNs(ctx, accountID, campaignID)
	if err != nil {
		return nil, "", err
	}
	for _, urn := range urns {
		verdict, why, readErr := c.judgeBoostCreative(ctx, accountID, orgURN, urn, "")
		switch verdict {
		case creativeWrong:
			return nil, urn, fmt.Errorf("creative %s on campaign %s (found by name) failed the author check: %s; nothing was posted, so remove it in Campaign Manager before boosting into this campaign again: %w", urn, campaignID, why, ErrBoostPostIneligible)
		case creativeUnknown:
			return nil, "", fmt.Errorf("creative %s on campaign %s could not be checked (%s): %w", urn, campaignID, why, unknownCause(readErr))
		}
	}
	return urns, "", nil
}
