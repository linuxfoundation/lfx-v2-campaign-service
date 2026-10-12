# 2026-10-07 — LFXV2-2665 HubSpot monitor PR #290 fixes

**Fix** — Four PR review threads on `monitor-hubspot-account`.

- **Possible examples.** `AccountMonitorActionItem` gets a type-level example (an ad-monitor
  finding, no `email_id`); Goa had cloned the HubSpot-only `email_id` into every ad-monitor
  action-item example. HubSpot's `action_items` keep their own example with `email_id`.
  `TestPublishedAdMonitorActionItemExamplesCarryNoEmailID` walks all four specs.
- **Attribution before dedupe.** `hubspotMonitorTargets` decides attribution (recorded portal is
  the token's, id canonical) first and de-duplicates only attributable emails, keyed portal+id.
  A newer foreign-portal, unrecorded-portal or malformed row with the same number no longer
  suppresses the older attributable row, and is always counted unattributable.
- **Request bound wording.** A read is at most 101 logical calls (100 emails + token-info), each
  retried at most 3 times on a 429, so at most 404 HTTP attempts — comments, concept and catalog.
- **`Truncated` doc.** The model comment states the conditional semantics: false does not prove
  nothing was capped (an old draft sent late is the unflagged residual).
