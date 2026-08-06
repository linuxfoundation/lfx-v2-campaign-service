# 2026-08-05 — Meta campaign/ad-set name reconciliation idempotency (LFXV2-2665)

**Update** — Meta's Graph API enforces no campaign-name uniqueness and exposes no create-idempotency key,
unlike Google/Microsoft/LinkedIn. `CreateCampaign` now runs pre-create name lookups via
`findCampaignByName` / `findAdSetByName` using Graph API `filtering` queries before POSTing,
reusing the id if found rather than creating a duplicate. Names are fully deterministic, so
retries reach the same names and reconcile correctly. Lookup failures are classified:
ambiguous (transport/5xx) → UNCONFIRMED partial (may exist, verify first); pre-send/4xx
→ clean error. Four new tests cover match/no-match/malformed-data/reuse paths.

**Update** — PR #79 review fix: a malformed-but-2xx lookup response (missing `data`
field, or a matched row with no usable id) meant Meta DID respond, but was being classified
as a clean failure rather than ambiguous — `createOutcomeAmbiguous` only recognized
`transportError`/`*APIError`, not the plain errors `findCampaignByName`/`findAdSetByName`
returned. Added an `errLookupAmbiguous` sentinel these two now wrap, and taught
`createOutcomeAmbiguous` to recognize it, so a malformed 2xx now returns the UNCONFIRMED
partial like a 5xx instead of a clean error. Also corrected an UNCONFIRMED message that
listed "an unfollowed redirect" as a possible lookup outcome — impossible, since
`createOutcomeAmbiguous`'s 3xx branch is gated to mutating methods and lookups are GETs.
Added two tests covering the lookup 4xx-clean vs malformed-2xx-UNCONFIRMED paths.
