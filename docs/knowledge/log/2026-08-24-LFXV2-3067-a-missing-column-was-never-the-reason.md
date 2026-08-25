# 2026-08-24 — "no column expresses it" was never the reason `status` is upstream-only

**Fix** — a review finding on the settings readback (PR #154), present at two sites: the
consumer-facing `docs/api-catalog.md` row and, more seriously, the `campaign-settings-field`
`field` description in `design/brief.go`, which Goa copies verbatim into the generated
clients and into all four committed OpenAPI documents. Both asserted a rationale that is
simply false.

## The claim, and why it is wrong

The settings readback reports four Google Ads settings **upstream-only** — `status`,
`budget_delivery_method`, `budget_explicitly_shared` and `bidding_strategy_type` — each with
no `recorded` counterpart and therefore a permanent `unknown` verdict. The docs explained all
four with one sentence: *"because no column on the campaign row expresses them."*

That sentence is true of three of them and false of `status`. `campaigns.status` has existed
since migration `000002_create_brief_campaign_tables`, and `model.Campaign.Status` has carried
it since. The column is right there.

The real reason is a **different axis**, not a missing column. `campaigns.status` holds this
service's own lifecycle vocabulary — mostly provisioning state (`pending`, `created`,
`created_degraded`, the retained-partial orphan markers, the soft-delete `deleted`) and only
sometimes a run state written by the status toggle. Google's `ENABLED`/`PAUSED`/`REMOVED` is
purely delivery state. A `created` campaign is neither more nor less `ENABLED` than a
`created_degraded` one. Comparing the two columns would report a permanent, meaningless
divergence on nearly every campaign while saying nothing about whether it is actually serving.
`model.PlatformCampaignRef` had documented this correctly all along; the readback prose had
drifted away from it.

## Why the wrong reason is worse than vague prose

A false rationale does not just misinform — it proposes a fix. "No column expresses it" tells
the next reader that adding a column would make `status` comparable. It would not; it would
produce a column on the wrong axis and a comparison that must then be suppressed anyway. A
rationale that implies a remedy has to be right about the remedy.

Four fields sharing a *verdict* were folded into sharing a *reason*. They do not, and the
grouping is what made the error easy to write and hard to see.

## The second half: the vocabulary is not a column list

The same description also claimed field names match "the campaign row's column names."
Checked against the DDL, six of the ten do (`budget_amount`, `budget_type`, `campaign_name`,
`start_date`, `end_date`, `status`) and four do not. `advertising_channel_type` is the sharp
one: it is a COMPARED field whose recorded side comes from `config_snapshot`, not from any
column — so a consumer trusting "these are column names" would conclude a compared field has
no recorded side. The names are this service's own stable, per-platform vocabulary that
happens to overlap the schema; they are now described that way.

## Rule

**A rationale is a claim about the code, and a doc that names the wrong cause is a bug even
when the behaviour it describes is correct.** Every site here described the right behaviour —
`status` really is never compared — and still had to be rewritten, because each named a cause
that does not hold. When one sentence explains several things at once, check it against each
of them separately: this one was written once, was true of three fields, and was copied into
a public API contract where it was false of the fourth.
