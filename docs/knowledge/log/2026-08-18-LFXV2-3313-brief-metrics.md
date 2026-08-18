# 2026-08-18 — LFXV2-3313 brief-wide metrics read

**Creation** — `GET /projects/{projectId}/briefs/{briefId}/metrics` reads every campaign on a
brief in one request, so the Monitor and Optimize tabs stop fanning out one request per
campaign per platform and assembling the result themselves.

**Why an aggregate could not just reuse the campaign-scoped handler.** `GetCampaignMetrics`
distinguishes six failure modes as distinct HTTP responses — window unsupported, unprovisioned,
no data in window, provenance unknown, account mismatch, unusable connection. An aggregate
cannot express those as HTTP responses, because one campaign's 409 would fail the other five.
Each becomes a per-row status instead, collapsed only where the operator's next action is
identical: `unsupported`, `not_ready`, `connection_problem`, `failed`, and `ok`.

`not_ready` and `failed` stay separate deliberately. A staged email draft and an ad-platform
outage produce the same absence of numbers and want opposite responses — one is the expected
steady state before a human presses send, the other is an incident.

**A non-`ok` row carries no `metrics` at all rather than zeroes.** The design attribute is
optional precisely so the schema permits that. A zero is a measurement, and substituting one
for a campaign that could not be read is indistinguishable from a campaign that genuinely
served nothing — the failure-as-measurement shape that produced a false "pause losing
campaigns" recommendation on the ED dashboard. `ok_count` is reported alongside the rows so a
consumer can see a cross-campaign total covers 2 of 6 before presenting it.

**Each `g.Go` returns nil even on error.** Returning it would cancel the errgroup's context and
abandon campaigns whose reads had not yet started, so the aggregate would report failures it
never attempted. Mutation-verified: propagating the error fails four tests.

**No cross-channel cost total.** `cost_micros` is micro-units of each platform's own native
currency and this service does no FX conversion, so summing LinkedIn USD with X's billing unit
would produce a number with no currency and no meaning.

**The window is per-row.** With no request-level window the default is platform-aware — X Ads
caps queryable ranges at 7 days — so rows can legitimately cover different periods. Each row
reports the window IT was read over; the top-level value is the requested one and does not
claim to cover every row. The row's window comes from the resolved value rather than the
adapter's echo, for the reason `GetCampaignMetrics` already documents: adapters are not
required to echo it back, and trusting them emits `""` and violates the response enum.

**Repository** — `ListCampaignsForBrief` is new on `CampaignReader`. It excludes soft-deleted
rows, matching `GetCampaign`, and returns a non-nil empty slice for a brief with no campaigns:
that is what every brief looks like before it is dispatched, and the caller must be able to
answer "nothing to measure yet" without it being a failure.

**Tests** — Nine, including two live-Postgres cases. The live tests caught three things a fake
could not: `brief_id` is `UUID` (not the descriptive string `dbtest.UniqueID` returns), it
carries a foreign key to `campaign_briefs(id)` so a brief must actually exist, and
`campaign_briefs` has no `payload` column.

**Note on a test that is NOT revert-binding.** The `ORDER BY platform, variant` is asserted but
removing it does not fail the live test: the planner serves this query from
`uq_campaigns_brief_platform_variant_live`, keyed on `(brief_id, platform, variant)`, so an
index scan already returns rows in the asserted order at these row counts — confirmed with
`EXPLAIN`. The clause stays because that is an accident of the current plan: a bitmap heap scan
or parallel seq scan on a larger table returns no order, and the consumer's table would start
reshuffling between reads with nothing failing to say so. The limitation is recorded in the
test comment rather than claimed as coverage it does not have.

**Chart** — No change needed, verified rather than assumed. The HTTPRoute regex's
`(briefs|jobs|hubspot)(/.*)?` arm matches the new path, and the RuleSet already carries
`/projects/:projectId/briefs/**`. The parity test passes.

**Scope** — Aggregation only. No action items, no pacing, no keyword flags: those are rules,
they differ per platform today, and they belong in their own change once this shape is proven.
Attribution model does not arise here — `CampaignMetrics` carries impressions, clicks, cost and
CTR, with no revenue or conversions, because the ad-platform APIs do not return them.

**Fix (review round 1)** — Three findings from the local reviewer trio, all real.

Two sentinels fell through `classifyBriefMetricsErr` to the retryable `failed` default:
`domain.ErrNotFound` (no connection row — a 404 on the campaign-scoped endpoint) and
`domain.ErrCredentialDecryptionFailed` (a 500). Both reach this path WITHOUT
`ErrConnectionNotUsable`, so the connection arms did not catch them, and `failed` told an
operator to retry a condition only a human edit clears. Verified by compiling a probe against
the real classifier rather than reading it. The decrypt arm now logs at ERROR — a rotated key
breaking every project must page someone, and the discriminator is the count of those lines —
and logs no error text, since `Encryptor` is an interface whose error may quote key material.

The fake's `ListCampaignsForBrief` discarded `projectID` while the real query filters on it,
so deleting `AND project_id=$2` from the SQL failed nothing. Fixed in the fake and pinned at
both layers: a service test seeding another project's campaign under the same brief id, and a
live test doing the same in Postgres. Mutation-verified — replacing the predicate with a
tautology now fails the live test with "got 2 campaigns, want 1".

`TestCampaignRepo_ReadsExcludeSoftDeleted` inspects query SOURCE to prove every campaigns read
excludes deleted rows, and the new query was not in its map — so the guard the bundle credits
with catching exactly this omission did not cover it. Added, and
`internal-infrastructure-postgres.md` now names six queries rather than five.

**Fix (full-branch sweep)** — Two cross-commit drift findings, both invisible to a per-commit
review because each was created by one commit and falsified by the next.

`briefMetricsRowStatusEnum`'s doc mapped `connection_problem` to "A 409". The review round then
added arms for `ErrNotFound` (404) and `ErrCredentialDecryptionFailed` (500) without touching
the design comment, so the API contract asserted a single status code for a bucket now covering
three. It no longer names a code, and states the rule the collapse actually rests on: the
remedy is identical.

`ErrSystemConnectionNotUsable` shared an arm with `ErrConnectionNotUsable` and inherited its
"reconnect it" wording — unfollowable in the one case that sentinel exists for, where the
project has NO connection of its own and fell back to the shared LF row. Split into its own
arm, raised to ERROR for the same count-is-the-discriminator reason as the decrypt case, and
the test's `wantReason` tightened from "not usable" (which both arms contain, so it passed
either way) to a phrase only the system arm carries. Mutation-verified: re-merging the arms now
fails.
