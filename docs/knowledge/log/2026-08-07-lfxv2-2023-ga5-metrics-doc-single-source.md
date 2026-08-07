# 2026-08-07 — One endpoint, one description: collapsing the duplicated metrics row

**Update** — Closed dealako's `[minor]` finding on PR #70 (`docs/api-catalog.md`).

The per-campaign metrics endpoint was documented **three times** — a prose paragraph and a
table row under *Monitoring*, both added by this branch, plus the pre-existing row under
*Optimization*. The two descriptions had drifted into contradiction, which is the actual
cost: the Monitoring copy said "Google Ads only; other platforms 400 until wired" and gave
`last_30_days` as the flat default, while the Optimization copy already documented
LinkedIn, Reddit and X support and X's `last_7_days` default (its stats endpoint caps a
queryable range at 7 days). A reader landing on the first one would have been told, with
equal authority, something that stopped being true when #73/#74/#75 merged.

The duplication was not a copy-paste slip — it came from writing GA-5's docs before those
platform PRs landed and never reconciling afterwards. That is the failure mode a second
copy always has: it is correct when written and there is nothing to make it wrong loudly.

**Resolution: exactly one authoritative description**, the Optimization row, with the
GA-5-specific detail folded in rather than dropped:

- The **409** now names both causes it covers — an unprovisioned campaign (empty
  `PlatformCampaignID`) and a campaign created under a different ad account than the
  project's connection currently resolves to. Both mean "this campaign id is not readable
  as asked"; the account-scoped case matters because reading under the wrong account
  silently returns zeros or another account's numbers rather than failing.
- The platform-support table gained its missing **Google Ads** row: all seven windows,
  mapped to GAQL date literals behind an allow-list so the platform-agnostic value never
  reaches the query as caller-supplied text.

What remains in *Monitoring* is a pointer, not a second contract: the endpoint is
campaign-scoped rather than under `{provider}/metrics` because it needs the persisted
campaign row (platform + `PlatformCampaignID`), not a provider+project pair. That
placement rationale genuinely belongs where the `{provider}/metrics` paths are listed —
it answers "why isn't it here?" — and it states no contract that can drift.
