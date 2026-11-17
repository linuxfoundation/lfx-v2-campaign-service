# 2026-11-17 — #564 X part 2b: review fixes (partial-result ids, reuse wording, card warning)

**Fix** — from the Copilot and lfx-one reviews of PR #626:

- **Variant abort kept the single ids empty.** When `promoteTweetVariants` aborted (the context
  ended between variant writes), `CreateCampaign` returned `partialResult()` before copying the
  first authored / promoted tweet into `AuthoredTweetID` / `PromotedTweetID`, and the partial
  result never carried a promotion id at all. `PromotedTweets` named the confirmed tweets while the
  single fields were empty, breaking the "set on partial results too" contract. The single fields
  are now filled before the abort check, `promotedTweetID` is declared above the closure, and
  `partialResult()` copies it. `TestCreateCampaign_VariantAbortNamesThePublishedTweets` asserts
  both equal the first `PromotedTweets` entry (it fails without the fix).
- **Variant reuse wording.** The `config` description said no variant is authored on a reused line
  item when its promoted-tweet count differs from the variants; with ZERO promoted tweets every
  variant is authored. The text now limits the refusal to a read failure or a nonzero mismatched
  count and states the zero case separately.
- **Website card warning suggested a retry.** The dispatcher saves a card failure as
  `created_degraded` with a campaign id, and the orchestrator's reuse branch returns that row
  without dispatching again, so a retry never creates the card or tweet. Both warnings (refused and
  UNCONFIRMED) now say to reconcile in X Ads Manager and that a retry does not create them.

Not verified live: no X call was made.
