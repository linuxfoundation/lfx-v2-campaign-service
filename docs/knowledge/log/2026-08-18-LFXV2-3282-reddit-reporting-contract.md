# 2026-08-18 — LFXV2-3282 Reddit reporting contract

**Verification** — Reddit's Ads API v3 reporting contract is publicly documented after all.
The LFXV2-2995 finding that it is not — that v3 reporting sits behind a gated developer
portal and a private Postman collection, leaving this client's implementation a best-effort
guess — is **superseded**. Reddit publishes an OpenAPI document at
`https://ads-api.reddit.com/api/v3/openapi.json`, linked as "Download Specs" from
`https://ads-api.reddit.com/docs/v3/`, and its own introduction states the Ads API "is open
to all developers and does not require allowlisting or approval from Reddit to access".

**The prior finding was not careless.** Web search still surfaces no shape: the docs render
client-side, `ads-api.reddit.com` and `web.archive.org` were both unreachable from the search
tooling, and the third-party integrations that prove the capability exists (Supermetrics,
Domo, Unified.to, Fivetran, dltHub) publish only their own normalized column names, never the
raw Reddit request or response. The spec was reached by opening the docs site in a real
browser and following the download link. **The generalisable lesson: "no public documentation"
is a claim about what was reachable, not about what exists, and a client-rendered docs site
looks identical to an absent one from a fetcher.**

**Four of the five guesses were wrong.** Only the path and method survived.

| Element | Guess | Spec |
| --- | --- | --- |
| Path + method | `POST /ad_accounts/{id}/reports` | correct |
| Campaign scoping | `campaign_ids` array | `filter` string DSL: `campaign:id==<id>` |
| Field names | lowercase `impressions` etc. | UPPERCASE enums `IMPRESSIONS`, `CLICKS`, `SPEND`, `CAMPAIGN_ID` |
| Response `data` | bare array of rows | object carrying a `metrics` array + `pagination` |
| `spend` | decimal string, scaled by 1e6 | `int64`, already **microcurrency** |
| `starts_at`/`ends_at` | bare `YYYY-MM-DD` | `YYYY-MM-DDTHH:00:00Z`, hourly granularity only |

The request schema sets `additionalProperties: false`, so `campaign_ids` would have been
rejected outright rather than ignored. The `spend` correction has the largest blast radius:
the old code multiplied by 1e6, so had it decoded at all it would have reported every figure a
million times too large. Notably this was the one guess the client's own conventions already
argued against — every monetary value the create path sends is an integer micro-dollar count
(`goal_value` carries `toMicrodollars(BudgetUSD)`) — which is a reminder that a proven local
convention is evidence worth weighing before a sibling platform's habit.

"Microcurrency" means the ad account's **own billing currency**, which this client does not
read, so `CostMicros` is micros of an unspecified currency, exactly as it is for X. It must
not be summed across platforms; `internal/service` already records that caveat.

**`CAMPAIGN_ID` is requested as a FIELD, not just a breakdown.** Requesting it only as a
breakdown groups by campaign without returning the id, which would leave the row-provenance
check nothing to verify against — the check would pass vacuously on a row whose id was never
populated. `breakdowns` is omitted entirely so Reddit aggregates over the window.

**The gate stays ON, and the reason changed.** `REDDIT_METRICS_ENABLED` still defaults off,
but no longer because the contract is unknown — because no request has ever been made against
a live Reddit ad account. A schema cannot express whether a campaign with no activity yields an
empty `metrics` array or a row of explicit zeros, nor whether `ends_at` is inclusive of its
final hour. Both readings are now handled without a wrong answer (an explicit zero row is
accepted as real data; an absent field is refused), and both are recorded at their site rather
than assumed away. A live-account run is now the ONLY thing between this and GA.

**Refusing rather than reporting a plausible number.** Every metric field is decoded as a
POINTER and a missing or null field is an error. This is not defensive habit: the spec types
`impressions`, `clicks`, and `spend` as `["integer","null"]`, so Reddit may send an explicit
null, and a value field decodes null to `0` — indistinguishable from a campaign that served
nothing. This was verified by probe against the PREVIOUS code, not assumed: of five plausible
wrong-shape bodies, two returned a silent all-zero `CampaignMetrics` with a nil error. That is
the failure-as-measurement class this repo refuses everywhere else, and it is the reason the
change is worth more than the field corrections.

The strictness deliberately differs from googleads, which treats an empty metric as a real
zero. That is correct there because Google Ads is *documented* to omit zero-valued metrics;
Reddit's spec documents no such behaviour, so there is nothing to justify reading an absent
metric as a measurement. If a live account later shows Reddit does omit zero metrics, the
guards should be relaxed deliberately with that evidence in hand — not by observing zeros.

**Errors name the cause without leaking.** A refusal lists which requested fields were absent,
so whoever first flips the flag learns the cause in one read instead of debugging a decoder.
No upstream value, campaign id, or account id is echoed; errors report the bare `reports` path
because the account id is interpolated into the real one. Pinned by test.

**The campaign-id charset guard now protects two sites.** It already sanitised the URL path;
the id is now also interpolated into the `filter` DSL, where a comma splits one filter term
into two and would silently widen the report's scope to another campaign. A
`camp_123,campaign:id==camp_999` injection case is pinned.

**Mutation results — 17 run, 16 killed, 1 survived and is reported rather than hidden.**

Killed: metric fields pointer→value (the silent-zero bug — 5 tests), spend scaled by 1e6,
timestamp layout → bare date, layout → `+00:00` offset, `ends_at` at midnight, lowercase field
enums, `campaign_ids` instead of `filter`, `metrics` key renamed, null `metrics` → zero,
missing-field guard disabled, provenance check disabled, clicks-without-impressions guard
disabled, negative-spend guard, negative-impressions guard, CTR halved, campaign-id charset
guard disabled.

**Survived: the `campaign_id == nil` arm of the missing-field check.** Making `CampaignID` a
value field and dropping that arm leaves every test passing, because a nil id decodes to `""`
and the provenance comparison rejects `""` anyway. The mutation is not weakened and no test was
added to force it: the arm is genuinely redundant for CORRECTNESS and is kept only because it
reports the accurate cause ("the row carried no campaign_id") rather than a misleading one
("the row is for a different campaign"). That limitation is now stated in the code comment, so
the guard is not credited with coverage it does not have.

Note the first attempt at that mutation was rejected as evidence: replacing the pointers
without fixing the dereferences only breaks the build, which proves nothing. Each mutation was
made to COMPILE before its result was counted.

**Test fixtures state their provenance.** The file header records that the bodies are modelled
on the published schema, not captured from a live account, so no assertion in it can be read
as proof that the schema matches production. Where a test encodes an assumption rather than a
documented fact — the no-activity shape especially — the test comment says so.

**Access that would unblock the remainder**: Reddit ad-account credentials with the `adsread`
scope for a live read. Partner support (`adsapi-partner-support@reddit.com`) is no longer
needed to obtain the contract, only an account to exercise it against.
