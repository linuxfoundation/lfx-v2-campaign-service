# 2026-08-18 — LFXV2-3067 campaign settings readback

**Creation** — `GET /projects/{projectId}/briefs/{briefId}/campaigns/{campaignId}/settings`
reads a campaign's CURRENT configuration from Google Ads and reports, per setting, where it
diverges from what the campaign row recorded. It closes a gap the code had documented but
could not answer: `ReadMetrics` returns impressions, clicks, cost and CTR, none of which
describe a campaign's *configuration*, so no reading of it could tell an operator that the
budget upstream is not the budget the row records.

**The row records the REQUEST; the platform keeps its own state.** `budget_amount`,
`budget_type` and `config_snapshot` are the caller-supplied config for a dispatch — what was
asked for. Adoption binds a campaign this service never created and pushes nothing upstream,
so the two can legitimately disagree. The readback makes that disagreement visible without
resolving it.

**Read-only in two senses, and both are load-bearing.** It issues no mutating call upstream
(nothing here can spend money), and it never writes back onto the campaign row. Write-back
would change those columns' meaning from *request* to *observation*, break shape-consistency
with every sibling adapter's rows, and let one transient bad read destroy the only record of
what was requested. An edit capability, when it lands, will *make* a change and record it as a
new request — a different thing, which needs this distinction intact.

**On-demand only: no campaign status, no polling.** A status that goes stale is worse than
none, and this service runs no polling infrastructure. There is deliberately no campaign-level
"in sync" boolean either: a single flag could only exist by collapsing `unknown` into
agreement or disagreement.

**`unknown` is a first-class verdict, never folded into `match`.** `CompareSettingsField` is
the only place the rule lives: `match` and `diverged` both require BOTH sides to have been
read, and absence on either side yields `unknown`. A field the platform did not return is
ABSENT from the response — never defaulted to zero or empty — because a `0` standing in for an
unread budget is indistinguishable from a campaign that genuinely has none, and the two mean
opposite things to an operator. `diverged_count` and `unknown_count` are reported separately:
"2 differ" reads very differently next to "and 5 could not be read".

**Status is reported but NOT compared.** The row's `Status` is this service's own lifecycle
vocabulary and Google's is `ENABLED`/`PAUSED`/`REMOVED` — different axes, as
`model.PlatformCampaignRef` already documents. Comparing them would report a permanent,
meaningless divergence on every campaign ever created. The upstream value is still carried,
with no recorded counterpart, so an operator can SEE that a campaign is paused upstream.

**Field names are VERSION-SCOPED to v23, and two of them are not the request-side spellings.**
Verified against Google's v23 release notes and `campaign.proto` before the query was written:

- `campaign.start_date` / `campaign.end_date` **do not exist in v23** — they were REPLACED by
  `campaign.start_date_time` / `campaign.end_date_time` (format `yyyy-MM-dd HH:mm:ss`, in the
  ad account's timezone). The old names are rejected as unrecognized, so a query built from
  the request-side vocabulary fails outright rather than returning a wrong value. The
  pre-v23 `2037-12-30` no-end-date sentinel is gone with them: no end date is now an ABSENT
  field, so any code testing for that sentinel silently stops matching.
- `campaign_budget.period` is `DAILY` or `CUSTOM_PERIOD` — there is **no `LIFETIME` value**,
  even though this service's own `model.BudgetType` spells that idea "lifetime".
  `CUSTOM_PERIOD` is what corresponds to it, and `googleAdsBudgetTypeFromPeriod` states the
  mapping once. `UNKNOWN`/`UNSPECIFIED` map to nothing and fail closed to an `unknown`
  verdict: mapping a value Google explicitly declined to state would manufacture a verdict.
- `campaign_budget.amount_micros` and `total_amount_micros` are **mutually exclusive** —
  the first for `DAILY`, the second for `CUSTOM_PERIOD`. Reading only the first reports a
  lifetime-budget campaign as having no budget, a false absence that suppresses a real
  divergence.
- `campaign_budget` is an ATTRIBUTED resource of `campaign`, which is what lets its fields be
  selected in a `FROM campaign` query. Attribution is not segmentation, so it does not
  multiply rows and the at-most-one-row guard still holds. The query selects no `metrics.*`
  or `segments.*` field for exactly that reason.

**Bumping `googleAdsAPIVersion` requires re-checking this SELECT list** against that version's
field reference rather than assuming it carries over — `target_cpa.*` left the selectable set
in v25, which is the same class of change as the v23 date rename.

**REMOVED campaigns are reported, not filtered.** `GetCampaign` excludes them server-side to
stop a tombstone being adopted; this read deliberately does not. "The campaign you are
tracking was removed upstream" is the most actionable divergence there is, and excluding it
would report the campaign as absent and hide the finding.

**Google Ads only.** Adoption — which is what lets a row's recorded request and the live
campaign disagree in the first place — exists only there. Other platforms return 400
(`ErrSettingsReadbackUnsupported`), the same clean "not supported" shape metrics and adoption
use for their own capabilities.

**Route/ruleset parity needed no chart change**: the HTTPRoute regex already forwards
`briefs(/.*)?` and the Heimdall RuleSet already authorizes `/projects/:projectId/briefs/**`,
so the new path is covered on both sides. `charts/.../parity_test.go` confirms it.
