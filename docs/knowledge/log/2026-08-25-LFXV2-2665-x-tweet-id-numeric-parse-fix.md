# 2026-08-25 — X tweet-authoring numeric id parse fix

**Fix** — `accounts/:id/tweet`'s response carries a legacy v1.1-shaped tweet
object with a JSON **numeric** `id` (unlike every other Ads API entity's
string `id`). The generic `extractID` helper, whose struct declares
`ID string`, silently failed to unmarshal that numeric field and returned
`""` — even though `resp.Data` held a fully valid, non-empty tweet — so a
tweet X had genuinely authored was misclassified as the malformed
2xx-no-id case: `AuthoredTweetID` stayed empty and the campaign persisted as
`created_degraded` with an UNCONFIRMED warning, despite the tweet actually
existing on X.

Found via a live nullcast-tweet-authoring dispatch against the real `8r7gb`
X ads account (`LFX_FORCE_SYSTEM_ADS_ACCOUNT=true`), which kept returning
`created_degraded` for a run that had, in fact, published and correctly
promoted a tweet.

New `extractTweetID` (`internal/platform/twitter/client.go`) parses the same
`resp.Data` for `id_str` instead — X's own string-typed escape hatch for the
same value, present specifically because the numeric id can exceed
float64's exact-integer range. `CreateCampaign`'s Step 4 now calls
`extractTweetID(resp)` in place of `extractID(resp)` for the tweet-authoring
response only; every other `extractID` call site (campaigns, line items,
promoted tweets) is untouched, since those ids are genuinely string-typed.

Also updated the two test fixtures that had encoded the old, unrealistic
all-string-id shape (`{"data":{"id":"tw1"}}`) — which happened to satisfy
the old buggy code — to the real numeric-id + `id_str` shape:
`internal/dispatch/twitter_test.go` and
`internal/platform/twitter/authortweet_test.go`.

See also
[internal/platform/twitter](../code/internal-platform-twitter.md#authoring-a-promoted-tweet-brief---servable-ad).
