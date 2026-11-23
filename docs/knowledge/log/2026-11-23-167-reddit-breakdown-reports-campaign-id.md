# 2026-11-23 — #167 Reddit audience and keyword reports also need the CAMPAIGN_ID breakdown

**Fix** — The audience (`get-reddit-ads-audience`) and keyword (`get-reddit-ads-keywords`) reads
sent `breakdowns: [<DIM>]` with `CAMPAIGN_ID` only in `fields`. A live read in the dev cluster
against the LF system Reddit account (campaign 2577406841688564185, `last_30_days`) showed that
rows broken down by `GENDER` alone carry no `campaign_id`, so every row would fail the provenance
check once `REDDIT_METRICS_ENABLED` is on. `readBreakdownReport` now sends
`breakdowns: [CAMPAIGN_ID, <DIM>]`. Reddit accepted the pair for `GENDER`, `COUNTRY`, `COMMUNITY`
and `KEYWORD`, and every row carried the id.

Same read: `GENDER` and `COUNTRY` rows summed exactly to the campaign total (505,751
impressions, 1,365 clicks). `COMMUNITY` rows summed to 775,204 impressions, because one
impression can count toward several communities. `KEYWORD` rows covered only keyword-matched
delivery (257,183 impressions).
