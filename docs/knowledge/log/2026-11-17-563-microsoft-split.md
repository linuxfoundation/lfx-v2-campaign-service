# 2026-11-17 — #563 Microsoft gaps split into smaller PRs

**Note** — PR #613 (branch `feat/563-microsoft-gaps-v2`) grew too large for the review bot, which
approves only when GitHub returns a patch for every changed file. The work is now split. The first
PR (branch `feat/563-microsoft-gaps-part1`) keeps six create inputs: search network
(`searchPartners`), ad schedule (`adSchedules`), device, age and gender bid adjustments, shared
negative keyword lists (`sharedNegativeKeywordListIds`), structured snippets
(`structuredSnippets`) and the logo extension (`logoAssetId` / `logoBusinessName`). It carries the
fixes from #613's review rounds that apply to them, including the shared error-entry rules in
`keyword_levers.go`, `audiences.go` and `geo.go`.

Moved to a later PR, still on `feat/563-microsoft-gaps-v2`:

- The remarketing-list picker (`GET /projects/{projectId}/microsoft-ads/remarketing-lists`,
  `MICROSOFT_REMARKETING_LISTS_ENABLED`) with its gateway route, rule and parity tests, and the
  streamed picker decoder `decodeCappedList`.
- Conversion goals and conversion bidding (`conversionGoalIds`, `bidding.strategy`
  `maxConversions` / `targetCpa`, `MICROSOFT_CONVERSION_GOALS_ENABLED`) and the conversion-goal
  picker (`GET /projects/{projectId}/microsoft-ads/conversion-goals`).
- Multiple ad groups (`adGroups`, `MICROSOFT_AD_GROUPS_ENABLED`), with the optional
  `service.BriefCreatePreflighter` hook that checks each group's display URL, the per-group
  toggle, bid and keyword levers, the bid write's ad group reconciliation and its temporary 409.
- The `config` attribute description in `design/brief.go`. Each edit of that one-line description
  regenerates it into several OpenAPI and CLI files, so it is updated once, with the last part.
  Until then the API catalog documents the six inputs. The create-campaigns method description
  already lists their synchronous 400s.

The knowledge log entries from #613 for the moved features stay as written.
