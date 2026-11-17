# 2026-11-17 — #564 X variants: the synchronous length check uses the composed tweet text

**Fix** — from the Copilot review of PR #614:

- A variant (or a single `tweetText`) whose copy carries a link of its own that is not the
  destination passed the config-only rule (`NormalizeTweetVariants`: 250 characters plus a link is
  274) but got the destination appended at create (298), so the create was a 202 and then a failed
  job. The dispatcher's synchronous preflight now composes each tweet exactly as the job does
  (`twitter.CheckComposedTweetTexts`, through the same `composeSingleTweet` /
  `composeVariantTweet` that `CreateCampaign` publishes from), each variant with its own
  `utm_content=variant-N` destination, and refuses it as a 400 before any job or X call.
- The preflight gets the brief from the context (`model.WithCreatePreflightBrief`, set by
  `CreateCampaigns`); without one it checks the config alone, as before. A destination that cannot
  be built, and the credential screen, stay the job's.
- The website card has no such gap: its headline is not composed with a URL and is already checked
  in full at preflight.

Not verified live: no X call was made.
