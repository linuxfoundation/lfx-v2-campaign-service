# 2026-10-06 — LFXV2-2665 Creative fetch-phase deadline budget and partial asset ids

**Fix** — The creative image fetch bounded each image at 20s and nothing bounded the
walk. The walk is sequential and the slot caps allow up to 25 images on Demand Gen and
~30 on Performance Max, so the worst case is eight to ten minutes against a
`providerCallTimeout` of two minutes. The comment claimed the caller's context bounded
the whole operation, which was true and was exactly the problem: the deadline would
expire somewhere inside the MUTATE cascade, leaving the orphaned campaign the preflight
guarantee exists to prevent. `fetchSlotImages` now gives the fetch phase HALF the
caller's remaining deadline, so exhaustion lands before the first budget mutate, where
it costs an error and nothing else, and says so in its own words rather than surfacing a
bare deadline error. The share is derived from what the caller actually has left rather
than set as a constant: a fixed ceiling would refuse a creative that comfortably fit the
caller's budget, and refusing a create Google would have accepted is the worse failure.
A caller with no deadline keeps the behaviour it had.

**Fix** — The assets:mutate short-result case on both channels shared an arm with
malformed JSON and returned `nil` asset ids. In that case the body PARSED — the resource
names in it are real, and are the only handle an operator has on account-level assets
that may already exist. The two conditions are now split, and the short-count branch
returns the parsed ids alongside the UNCONFIRMED error through a shared
`parsedAssetIDs`, matching the malformed-resource-name arm immediately below it that
already returned what it had got to. The campaign-level partial result was never at
risk; what was being dropped were the ids needed to find and reuse the uploaded assets.

**Fix** — `validateBiddingPlan` echoed the caller's raw, uncapped `BiddingStrategy` into
the unknown-strategy error. These errors persist unencrypted as Steps entries and this
package caps every other caller-controlled string that reaches one (`ad_copy.go` wraps a
far shorter value for the same reason); it now goes through `capForError`.

**Docs** — Three doc comments described code other than the code they were attached to:
the `searchBiddingStrategies` paragraph sat above `performanceMaxBiddingStrategies` — the
channel whose guard REFUSES the `manualCpc` the text promises — leaving the Search set
undocumented; `performanceMaxAssetCreate.Name` claimed Google requires a name on an image
asset, which the URL-containment fix had made false and which `buildPerformanceMaxAssets`
contradicts twenty lines away; and the `validateYouTubeVideoIDs` explanation was attached
to `isYouTubeIDRune`. Each now sits on what it describes. The OKF concept also said asset
groups are created PENDING while the code creates them PAUSED (as `docs/api-catalog.md`
already said); the concept, its frontmatter `description`, the matching `index.md` bullet
and the misnamed `performanceMaxAssetGroupPending` constant are all corrected.
