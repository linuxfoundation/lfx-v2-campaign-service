# 2026-10-13 — Negative keywords for Reddit and X

**Update** — `add-negative-keywords` was Microsoft-only; it now serves Reddit and X too. On Reddit
it appends to the `excluded_keywords` of the ad group this service created. It uses the same
whole-targeting PATCH as `remove-keyword-targeting`, sits behind
`REDDIT_KEYWORD_TARGETING_WRITES_ENABLED`, requires the read's `revision` (409 when stale), stops at
Reddit's 2,000 excluded keywords (409), and confirms the write with a re-read. On X each keyword is
one `NE` keyword targeting criterion on the recorded line item: one paced POST, sent once, at most
20 per request, each with its own outcome. The match-type enum gains `Broad`. Microsoft accepts
`Exact`/`Phrase`, X accepts `Broad`/`Phrase`/`Exact`, and Reddit accepts `Broad` only, because its
excluded keywords have no match type. Anything else is a 400. Already-present keywords are
`ALREADY_PRESENT` and are not resent. Provenance fails closed on both platforms, and ownership of
the ad group or line item is proven by a read before any write. The `NegativeKeywordAdder` method
gained a `revision` argument, which Microsoft and X refuse. The Microsoft text rules moved into
`microsoft.NormalizeNegativeKeywordText` so every platform judges the text the same way. See
[keyword-targeting-reddit-x](../architecture/keyword-targeting-reddit-x.md) and the
`/negative-keywords` row of the API catalog. Neither platform has been exercised against a live
account.
