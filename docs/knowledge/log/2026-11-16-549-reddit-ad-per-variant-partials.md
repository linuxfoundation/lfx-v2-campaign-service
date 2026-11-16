# 2026-11-16 — Reddit one ad per variant keeps its posts on a refused campaign (#549)

**Fix** — On the one-ad-per-variant create (#549 gap 6, behind `REDDIT_AD_PER_VARIANT_ENABLED`),
the variants' promoted posts are authored before the campaign. A definite refusal of the campaign
create now returns a partial result naming those posts (`variantAds`) instead of `(nil, err)`, so
the dispatcher keeps the claim and a retry does not author them again. A caller cancellation during
ad creation now names every variant left without a confirmed ad in `AdWarning`, not only the
cancelled one, and a variant the cancelled create never reached omits `outcome` instead of
persisting `""`. The dispatcher reads the flag once and passes it to its check and to the client.
`docs/api-catalog.md` now says a `budgetType: "daily"` create (#549 gap 9) is a per-day cap and
that the budget PATCH keeps the pacing the campaign carries.
