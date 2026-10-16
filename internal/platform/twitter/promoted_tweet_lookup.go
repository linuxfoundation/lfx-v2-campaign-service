// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/identityjson"
)

// promotedTweetsPageSize is the count requested on the promoted_tweets list — X's documented
// maximum for that endpoint (Min 1, Max 200; https://docs.x.com/x-ads-api/creatives/reference,
// "Promoted Tweets"). Like listPageSize it is also the denominator of the short-page test.
const promotedTweetsPageSize = 200

// promotedTweetRef is one live promoted-tweet association already on a line item.
type promotedTweetRef struct {
	// ID is the promoted_tweet entity id (what PromotedTweetID records).
	ID string
	// TweetID is the promoted tweet's id, held to tweetIDRe like every tweet id this client
	// promotes or persists.
	TweetID string
}

// promotedTweetElement is the subset of an X promoted-tweet object this client reads.
type promotedTweetElement struct {
	ID         string `json:"id"`
	TweetID    string `json:"tweet_id"`
	LineItemID string `json:"line_item_id"`
	Deleted    bool   `json:"deleted"`
}

// listPromotedTweets returns every live promoted-tweet association on lineItemID, via
// GET accounts/:account_id/promoted_tweets?line_item_ids=<id>&with_deleted=false.
//
// It is the reuse read for Step 4 of CreateCampaign: a promoted tweet has no name, but it
// has a parent, and the parent line item is found by its deterministic name. So when a retry
// REUSES the line item, this read says what an earlier attempt already attached to it — the
// association a lost promoted_tweets response may have committed, and the tweet a lost
// authoring-and-promotion may have published — and the caller reuses that instead of
// re-POSTing the association or publishing a second tweet.
//
// It follows findByName's rules, all failing closed, because an empty answer is acted on by
// writing: a short page is conclusively last, a FULL page with no usable cursor is
// inconclusive, a 2xx with no data field is unanswered, a repeated cursor or the page cap is
// unterminated — each an error rather than "none". So is any element it cannot account for:
// a missing promoted-tweet id, a tweet id of the wrong shape, or a parent line item other
// than lineItemID (the filter was not honoured, so nothing in the page is known to belong to
// this line item). An element X reports deleted is skipped.
//
// Each page's raw body must pass identityjson before it is decoded: a duplicated `data` key is
// resolved last-wins by encoding/json, so a trailing empty array would read as "no
// associations" and license authoring and promoting a second tweet. The same association id
// repeated across pages (or within one) is one association, not two — counting it twice would
// refuse a reuse as ambiguous — but the same id naming two different tweets is refused. Reads are unpaced, like every
// GET on this client, and are retried on 429 by request().
func (c *Client) listPromotedTweets(ctx context.Context, lineItemID string) ([]promotedTweetRef, error) {
	path := "promoted_tweets?line_item_ids=" + url.QueryEscape(lineItemID) + "&with_deleted=false&count=" + strconv.Itoa(promotedTweetsPageSize)
	refs := []promotedTweetRef{}
	// tweetOf is every association id already in refs, with the tweet it promotes.
	tweetOf := map[string]string{}
	seen := map[string]struct{}{}
	cursor := ""
	for page := 0; page < maxListPages; page++ {
		resp, err := c.requestPage(ctx, path, cursor)
		if err != nil {
			return nil, fmt.Errorf("x promoted tweets lookup failed: %w", err)
		}
		if resp != nil {
			if err := identityjson.Check(resp.raw); err != nil {
				return nil, fmt.Errorf("x promoted tweets lookup returned a page that cannot be trusted, so an existing promoted tweet cannot be ruled out: %w", err)
			}
		}
		var items []promotedTweetElement
		if resp != nil && len(resp.Data) > 0 {
			if err := json.Unmarshal(resp.Data, &items); err != nil {
				return nil, fmt.Errorf("x promoted tweets lookup returned an undecodable page: %w", err)
			}
		}
		if resp == nil || items == nil {
			return nil, fmt.Errorf("x promoted tweets lookup returned a page with no data field, so an existing promoted tweet cannot be ruled out")
		}
		for _, it := range items {
			if it.Deleted {
				continue
			}
			if strings.TrimSpace(it.LineItemID) != lineItemID {
				return nil, fmt.Errorf("x promoted tweets lookup returned an association for a different line item, so the line-item filter cannot be trusted")
			}
			id := strings.TrimSpace(it.ID)
			tweetID := strings.TrimSpace(it.TweetID)
			if id == "" || !accountIDRe.MatchString(id) {
				return nil, fmt.Errorf("x promoted tweets lookup returned an association without a usable id")
			}
			if !tweetIDRe.MatchString(tweetID) {
				return nil, fmt.Errorf("x promoted tweets lookup returned an association without a usable tweet id")
			}
			if _, err := strconv.ParseInt(tweetID, 10, 64); err != nil {
				return nil, fmt.Errorf("x promoted tweets lookup returned an association without a usable tweet id")
			}
			if prev, dup := tweetOf[id]; dup {
				if prev != tweetID {
					return nil, fmt.Errorf("x promoted tweets lookup returned one association naming two different tweets, so which one it promotes cannot be told")
				}
				continue
			}
			tweetOf[id] = tweetID
			refs = append(refs, promotedTweetRef{ID: id, TweetID: tweetID})
		}
		switch cursorVerdict(resp) {
		case cursorMore:
		case cursorExhausted:
			return refs, nil
		default:
			if len(items) < promotedTweetsPageSize {
				return refs, nil
			}
			return nil, fmt.Errorf("x promoted tweets lookup returned a full page with no usable next_cursor, so an existing promoted tweet cannot be ruled out")
		}
		if _, dup := seen[resp.NextCursor]; dup {
			return nil, fmt.Errorf("x promoted tweets lookup did not terminate (repeated page cursor)")
		}
		seen[resp.NextCursor] = struct{}{}
		cursor = resp.NextCursor
	}
	return nil, fmt.Errorf("x promoted tweets lookup exceeded %d pages with more results remaining", maxListPages)
}

// promotedTweetsFor returns the refs in refs whose TweetID is tweetID.
func promotedTweetsFor(refs []promotedTweetRef, tweetID string) []promotedTweetRef {
	var out []promotedTweetRef
	for _, r := range refs {
		if r.TweetID == tweetID {
			out = append(out, r)
		}
	}
	return out
}
