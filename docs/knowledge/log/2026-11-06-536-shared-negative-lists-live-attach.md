# 2026-11-06 — #536 the multi-list shared negative keyword list attach is verified live

**Docs**: records a live create of the `sharedNegativeKeywordLists` attach and replaces the
NOT LIVE-VERIFIED wording it settles. No request shape or behaviour changes.

- **validateOnly (nothing persisted).** A `googleAds:mutate` with a temp-id budget, a PAUSED Search
  campaign and `campaignSharedSet` links was accepted with two `NEGATIVE_KEYWORDS` sets, with one
  `NEGATIVE_PLACEMENTS` set, with one `BRANDS` set, and with a mix of the first two types. A
  `campaignSharedSets:mutate` against the persisted probe campaign also accepted the
  `NEGATIVE_PLACEMENTS` and `BRANDS` sets. Neither was attached for real.
- **Live create (#536).** Two new `NEGATIVE_KEYWORDS` shared sets were created for the test:
  12302728737, with a BROAD and a PHRASE keyword, and 12302728740, with an EXACT keyword. Then
  campaign-service's own `CreateCampaign` created paused Search probe campaign **24433949469**
  (account 8666746580, v23, budget 1.00/day, non-shared). Its `SharedNegativeKeywordLists` were
  `[A, " B", A]`. The client trimmed and de-duplicated them and sent ONE
  `campaignSharedSets:mutate` with two operations. Google answered 2xx with
  `24433949469~12302728737` and `24433949469~12302728740` in operation order, and
  `sharedNegativeKeywordListIds` was `["12302728737","12302728740"]`.
- **Read-back.** On `shared_set`, both sets are `NEGATIVE_KEYWORDS`, `ENABLED`, with member counts 2
  and 1 and a reference count of 1 each. On `shared_criterion`, the three keywords hold the text and
  match type that were sent. On `campaign_shared_set`, both links to campaign 24433949469 are
  `ENABLED`.
- **Docs.** `shared_negative_lists.go`, the `sharedNegativeKeywordLists` row in
  `docs/api-catalog.md` and the googleads code page now cite the live create. They also state that
  Google does not refuse a set of another type, and that this client does not check the type.
