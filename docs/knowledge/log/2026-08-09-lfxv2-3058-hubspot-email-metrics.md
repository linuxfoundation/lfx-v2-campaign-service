# 2026-08-09 — Email metrics: an open map is a reason to fail, not a reason to return zeros

**Update** — `GetEmailMetrics` in `internal/platform/hubspot/statistics.go` reads HubSpot's
marketing-email statistics list and returns the shared `model.CampaignMetrics`, plus a new
`model.EmailMetrics` sub-object carrying the six counters no ad platform has. This is part 1 of
2: the client and its contract. Part 2 wires it to `HubSpotDispatcher.ReadMetrics` and adds the
optional `email` object to the Goa result, making HubSpot the first non-ad-platform on
`GET .../campaigns/{id}/metrics`.

**The contract came from HubSpot's generated client, not from a guess.** The v3 statistics
documentation is auth-gated, and the last time a metrics adapter shipped against an inferred
shape (Reddit, LFXV2-2995) it had to be gated default-OFF because a guessed response returning
200 looks authoritative to every consumer. So this one was read out of
`HubSpot/hubspot-api-python`, which is generated from the same spec the API serves:
`AggregateEmailStatistics` is `{emails, campaignAggregations, aggregate}`, and
`EmailStatisticsData` is `{deviceBreakdown, qualifierStats, counters, ratios}`.

**And that is where the problem was.** `counters` is typed `map[string]int` with **no
enumerated keys**. A renamed key set therefore decodes perfectly — into zeros. Zeros are not a
neutral answer here: the caller acts on them, and "this campaign sent nothing" is a
decision-grade claim about a campaign that may have sent ten thousand emails. The same shape
applies to the `emailIds` filter: if it is not honoured, the counters belong to a stranger's
email and reporting them attributes their sends to this campaign.

So both are errors rather than values:

- `ErrUnrecognizedCounters` — a non-empty `counters` map with no key from HubSpot's vocabulary.
- `ErrStatisticsFilterNotHonored` — a non-empty `emails` list that omits the id we asked for.

**The probe set is deliberately wider than the mapped set, and that asymmetry is the point.**
Six counters are mapped; fourteen are recognized. An email created but never sent inside the
window legitimately comes back carrying only `notsent`/`pending`, and a guard that recognized
only what it maps would call that a schema change and error on an ordinary empty result. The
guard's job is to separate "the vocabulary is intact and these numbers are zero" from "the
vocabulary changed and we are reading nothing at all" — which requires recognizing the whole
vocabulary while mapping part of it. Over-rejection is a false answer too; it is just the
easier mistake to make, because it looks conservative.

**`campaignAggregations` is not decoded on purpose.** It is keyed by email-CAMPAIGN id, not by
email id. Indexing it with the id we filtered on would miss silently and fall through to a zero
value — the exact failure the guards above exist to prevent, reintroduced by a plausible-looking
map lookup. Since the request filters to one email, `aggregate` already is that email's
aggregate.

**Where the ad-platform analogy stops.** `impressions` mirrors `opens` and `ctr` is
`clicks/opens` (computed here rather than taken from HubSpot's own `ratios`, so one definition
of CTR holds across every channel in a report). `cost_micros` is 0 — and that 0 means "this
platform bills no per-send cost", not "this campaign was free". Email spend sits in the HubSpot
subscription, invisible to this API. A consumer that blends it into a cross-channel
cost-per-acquisition understates the real cost, so it is stated in the model doc and the
package doc. The field's shape gives no hint of it. The Goa description is NOT one of those
places yet — it still carries the generic per-platform cost wording; saying so there belongs
with part 2, where the email window reaches the API surface.

**Window validation runs before credentials are resolved.** An unsupported window is a
permanent 400 whatever the connection looks like; resolving first would report a connection
problem instead and invite a retry of a request that can never succeed. `ValidateMetricsWindow`
is exported for exactly that ordering, and `timeRangeForWindow` calls it too so the two can
never disagree about which windows exist.

**Two date bugs were designed out rather than fixed later.** Month boundaries are computed from
the first of the month — `AddDate(0, -1, 0)` on the 31st normalizes into the following month and
would shift `this_month`/`last_month` for three days of every long month. And `end` is the last
millisecond of the final day, not next-midnight: HubSpot does not document whether
`endTimestamp` is inclusive, and under either reading this range is off by at most a
millisecond, where next-midnight would gain an entire extra day if the bound is inclusive.

That second one was only half designed out. `end` was BUILT as the final millisecond and then
formatted with `time.RFC3339`, which truncates the fraction — so what actually went on the wire
was `23:59:59Z`, giving away 999ms of every window and contradicting the contract the comment
stated. `time.RFC3339Nano` fixes it and leaves `start` (exactly midnight, no fraction) alone.
Worth naming as a class: the invariant was established in the value and then lost in the
SERIALIZATION, where no test of `timeRangeForWindow` would ever see it. What caught it was
asserting on the query string the handler received rather than on the returned `time.Time`.

The main test clock is pinned to 2026-03-15 — mid-month, in a 31-day month whose predecessor has
28 days. That reaches the next-midnight bug but NOT the month-arithmetic one: subtracting a month
from the 15th is always valid, so `AddDate(0, -1, 0)` would have passed every assertion.
`TestGetEmailMetrics_MonthWindowsOnAMonthEndDate` pins the 31st separately, where the naive form
normalizes 2026-02-31 into 2026-03-03 and reports `last_month` as a few days of March.

**Every guard was revert-checked.** Removing the filter-not-honored guard returns `nil` where
the sentinel is expected; removing the counter-vocabulary guard does the same; relaxing id
canonicality sends a request for a malformed id; next-midnight and `AddDate(0,-1,0)` each
produce a visibly wrong range; and narrowing the probe set to only the six mapped keys makes a
legitimately-quiet window error.
