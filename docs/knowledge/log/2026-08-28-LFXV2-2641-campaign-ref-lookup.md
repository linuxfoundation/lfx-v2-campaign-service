# 2026-08-28 — LFXV2-2641 campaign-ref lookup

**Creation** — Added `GET /projects/{projectId}/google-ads/campaign-ref`, which
maps a Google Ads campaign id to this service's own campaign and brief.

The keyword reads publish Google's numeric campaign id, because that is what
the GAQL rows carry. Every mutation route is keyed by this service's campaign
UUID under its brief. Nothing bridged the two, so a caller holding keyword rows
could not act on them.

It is a READ rather than a bulk mutation endpoint, and that is the design
constraint rather than a preference: `docs/api-catalog.md` rule 5 forbids bulk
mutation endpoints because a single bulk call cuts across per-target permission
boundaries, and the same section records that cross-campaign bulk endpoints are
intentionally omitted. Resolving ids here and letting the caller issue one
per-campaign mutation each keeps every mutation scoped to one
permission-evaluated target.

Three contract decisions, each pinned by a test.

An unowned id is 200 with an empty `matches`, not 404. "This project owns no
campaign with that upstream id" is an answer the caller acts on by refusing,
while 404 says the route or the project is wrong — a caller that cannot tell
them apart either retries something that will never work or reports the wrong
cause.

`matches` is an array, but a valid database can never return more than one:
`uq_campaigns_platform_campaign_live` (migration 000020) is a global UNIQUE
index on `(platform, platform_campaign_id)` for live Google Ads rows, so
scoping to a project can only narrow one row to zero or one. The array shape is
kept so that a lapsed invariant — a dropped index, a narrowed predicate — is
refusable rather than silently resolved by taking the first row. An earlier
revision of this work claimed the schema permitted duplicates; it does not, and
the comments and tests now say what the index actually guarantees.

The scope lives in the SQL, not in a Go-side filter. The Google Ads customer is
shared across foundations, so an unscoped lookup would answer "does foundation
X own campaign N?" — a question that must not be answerable rather than
answered and discarded.

**Note** — A storage failure is reported as a 500 and deliberately NOT through
`classifyInsightsError`. Every arm of that classifier describes a platform
failure, and its default reports an upstream outage; this lookup contacts no
platform, so routing a local table fault there would advertise it as a
retryable Google Ads problem, and would reuse the 503 this method reserves for
COLD START.
That status means "not wired yet, try again"; a live database fault is neither.
