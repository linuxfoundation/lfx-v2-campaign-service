# 2026-08-09 — Email metrics: an open map is a reason to fail, not a reason to return zeros

**Update** — `GetEmailMetrics` in `internal/platform/hubspot/statistics.go` reads HubSpot's
marketing-email statistics list and returns the shared `model.CampaignMetrics`, plus a new
`model.EmailMetrics` sub-object carrying six email-specific counters — four with no ad-platform
analogue, plus `Opens` and `Clicks`, which deliberately duplicate the shared `Impressions` and
`Clicks` so a consumer need not know which ad-shaped field email was mapped onto. This is part 1 of
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

- `ErrUnrecognizedCounters` — a `counters` map with no key from HubSpot's vocabulary.
- `ErrStatisticsFilterNotHonored` — a non-empty `emails` list that is not exactly the id we
  asked for.

A third joined them once the span's real semantics were read (see below):

- `ErrNoSentEmailInWindow` — an empty `emails` list, meaning the span did not contain this
  email's send date.

And two more once the guards were re-read against what they actually admit:

- `ErrRenamedCounter` — a MAPPED key absent while an unrecognized key is present.
- `ErrNegativeCounter` — any counter below zero.

**Both of the last two are holes the first three left open in the same shape as each
other.** `ErrUnrecognizedCounters` asks whether the vocabulary is recognizable AT ALL, and
`{"sent":1000,"emailsOpened":400}` answers yes — one known key is enough, and the `open`
lookup then returns an authoritative 0 for an email with 400 opens. The guard was written
against the TOTAL rename because that was the fixture; the partial one is the neighbouring
value again, which is now three times this file has produced that class.

The obvious remedy — reject any map containing an unknown key — is worse than the bug. The
likeliest way this vocabulary changes is HubSpot ADDING a counter, and that remedy turns a
purely additive upstream release into a hard outage. The opposite remedy, requiring all six
mapped keys, guesses that HubSpot emits zero-valued counters rather than omitting them, and
the spec that would settle it is auth-gated — guessing wrong there rejects ordinary quiet
emails. What is true regardless of both unknowns is that a renamed key does not vanish: it
reappears under a new name in the same response. So the guard requires the CONJUNCTION —
a mapped key absent AND an unrecognized key present — and the two must-not-error subtests
of `TestGetEmailMetrics_PartiallyRenamedCounterVocabularyIsAnError` (an omitted counter, an
added counter) are as much the point as the one that must.

The negative-counter hole was simpler and is a straight consistency gap: LinkedIn, Meta and
Reddit all reject negative counts, HubSpot did not, and a negative `open` becomes a negative
`Impressions` and a negative `Ctr` in the public response. It is checked across the whole
map rather than the six mapped keys, because a negative anywhere says the payload is wrong.

**And `emails` had the nil-versus-empty bug the counters map was explicitly designed around.**
`Emails []int64` decodes a MISSING field and an explicit `[]` to the same nil, so the new
empty-list branch reported a vanished field as "wrong window" — inviting a caller to retry
other windows against a response shape that can never carry what it needs. The field is the
only evidence the filter was honoured, so its absence belongs with
`ErrStatisticsFilterNotHonored`. A pointer keeps the two distinguishable. Worth naming
because the identical distinction had already been reasoned about one struct field away: the
counters guard deliberately CONFLATES nil and empty, on the argument that both are the same
schema break. That argument is right for `counters` and wrong for `emails`, and the tell is
that `emails` has a third meaningful state the map does not.

**Both guards were first written one qualifier too narrow, and the same mistake produced
both.** Each began as a check on the shape it had a fixture for, rather than on the property
it was defending — and the gap that leaves is not a corner case, it is the neighbouring
value.

The filter guard asked "is our id PRESENT?", which passes `[1, 4242, 9999]`. But the request
supplies exactly one `emailIds` value and `aggregate` is the aggregation over the emails the
response covers, so company in the list means the aggregate carries two strangers' sends —
the very misattribution the guard was written to stop, waved through because the guard tested
membership rather than the filter. Its accompanying test asserted the wrong thing for the
right-sounding reason ("the guard rejects ABSENCE, not the presence of company"); the guard
and the test had to be rewritten together, which is what a wrong invariant costs. Either the
filter was honoured and the list is exactly what we asked for, or it was not and none of the
response is trustworthy. There is no middle reading.

The vocabulary guard was conditioned on `len(counters) > 0`, so it never fired on the case it
most needed to: a renamed or dropped `counters` FIELD decodes to a nil map, and every lookup
then returns 0 for an email HubSpot has just told us it covers. The empty map and the missing
key are the same schema break; only the second is invisible to a non-empty test. Zeros briefly
survived exactly one path — an empty `emails` list, which was read as the API's way of
reporting no activity. That reading was wrong, and the next section is why.

Worth naming as a class: **a fail-closed guard qualified by the shape of its fixture.** Both
qualifiers looked like defensive narrowing and were in fact holes, and neither would have been
caught by a test written from the same fixture that motivated the guard.

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
legitimately-quiet window error. The two widened guards were revert-checked in the same way:
restoring the presence check accepts `[1, 4242, 9999]`, and restoring `len(counters) > 0`
returns all-zero metrics for all three absent-counter shapes (`{}`, a missing `aggregate`, and
a missing `counters`).

## Round 2: the probe set was wider than the mapped set, and still not wide enough

"Six counters are mapped; fourteen are recognized" above is now **seventeen**, and the three
that were missing — `contactslost`, `hardbounced`, `softbounced` — are in HubSpot's own v3
response examples. That is not a cosmetic gap. An unrecognized key is one half of the
`ErrRenamedCounter` conjunction, so an entirely ordinary response carrying a bounce breakdown
alongside a zero-valued mapped counter HubSpot chose to omit was rejected as a rename.

The lesson is the one the section above already states, arriving from the other direction: the
asymmetry between the PROBE set and the MAPPED set only protects against over-rejection if the
probe set is genuinely complete. Getting it from memory of "the vocabulary HubSpot's email APIs
have used since v1" produced fourteen of seventeen, and the three missed were exactly the ones
that appear on emails with real bounce activity. Widening the probe set can never do harm — it
does not change what the client READS — so there was no reason to be conservative about it.

Three more corrections landed in the same round, all of the same family:

- **`ValidateMetricsWindow` deferred to `model.IsValidMetricsWindow`.** The two sets are equal
  today, so nothing failed; but the shared vocabulary is where a window gets ADDED, and the
  validator exists precisely so a permanent 400 is reported before credentials are resolved.
  Inheriting the model's set means the first unmapped addition validates, resolves, and fails
  late. It now enumerates HubSpot's own set, as the LinkedIn adapter does.
- **Two error paths interpolated untrusted response content** — the unrecognized counter key in
  `ErrRenamedCounter`, and the key and value in `ErrNegativeCounter`. Both reach a log line
  through `safeErrSummary`, which truncates but does not redact. The half of each diagnosis
  that comes from a static list is still named; the rest is reported by shape (a count, or "an
  unrecognized counter").
- **The JSON decode error forwarded its cause.** Not quoting the body is not sufficient:
  `json.SyntaxError` and `json.UnmarshalTypeError` reproduce fragments of the input, including
  an overflowing numeric literal verbatim. Same fix as the redaction rounds on LFXV2-2775 —
  discard the cause, build the message from constants plus a length.

And one test defect: the non-error cases of the rename table asserted only that the error was
not `ErrRenamedCounter`, so any OTHER failure passed while the subtest names claimed those
shapes read successfully. They now require `err == nil`.

## Round N: the sentinel claimed more than the response could support

Copilot, on the empty-`emails` branch: an empty list only establishes that no SENT email
matched the id and the span. It does not establish that an existing email was sent outside the
window — a staged HubSpot draft that has never been sent, and a stale or nonexistent id, reach
this branch identically.

Real, and the cost is specific: the old message ("the requested window does not contain this
email's send date") reads as an instruction to retry with a different window, which is wrong
for two of the three states and wrong in the expensive direction — a user widens the window
repeatedly looking for an email no window will ever find.

The narrowing lived in the NAME as well as the message, so both changed:
`ErrEmailNotSentInWindow` → `ErrNoSentEmailInWindow`, and the text now says "no email with this
id was sent during the requested window (it may have been sent outside it, never sent, or not
exist)". The other option offered — resolve the email's send state before classifying — needs a
second HubSpot call on the failure path of a read, which is a lot of machinery to distinguish
three states the caller acts on identically.

Revert-verified: restoring the old message makes the empty-list test report `want it to admit
the other two states an empty list can mean`.

The general form, and it is the second time this file has hit it from a different direction:
**an error's name is a claim, and it must be the weakest claim the evidence supports.** The
whole-vocabulary guard was widened for the same reason — because `len(counters) > 0` asserted
something the nil map did not license.

## Round N: "every element matches" is not "exactly one element"

`isExactlyID` looped over the response's `emails` list rejecting any element that was not the
requested id, then returned `len(ids) > 0`. That accepts `[4242, 4242]` — every element matches,
the list is non-empty — and a duplicated id is not the response to a request carrying exactly one
`emailIds` value. Nothing in the body says whether `aggregate` sums one email's counters or two,
so the doubled reading is available and would be reported as this campaign's. The guard now asks
for the singleton (`len(ids) == 1 && ids[0] == want`), which is both stricter and shorter than
the loop it replaces. `TestGetEmailMetrics_RejectsADuplicatedEmailID` pins it.

The general shape: **a per-element predicate answers a question about elements, and the guard
needed one about the SET.** "No stranger appears" and "exactly what I asked for came back" differ
on exactly the responses where the filter misbehaved without inventing an id — which is the case
a filter guard exists for.

Two smaller corrections in the same push:

- The malformed-JSON diagnostic is built from constants plus a length so it can be logged
  (`BriefService.GetCampaignMetrics` logs `safeErrSummary(err)`, which truncates but does not
  redact). Nothing enforced that. `TestGetEmailMetrics_MalformedJSONIsRedacted` now does, using a
  numeric overflow marker — `json.UnmarshalTypeError` copies a numeric literal into its own
  message but reduces a string to the word "string", so a quoted marker could not fail the
  assertion even against a verbatim wrap. Same invariant, same technique, as
  `internal/platform/linkedin/metrics_test.go`.
- `fixedClock`'s comment claimed its 2026-03-15 instant catches an `AddDate(0, -1, 0)` bug. It
  does not — subtracting a month from the 15th is always valid, which is precisely why
  `TestGetEmailMetrics_MonthWindowsOnAMonthEndDate` pins its own clock to March 31. The comment
  now says the fixture is deliberately unremarkable, and the knowledge doc no longer describes
  all six email counters as having no ad-platform analogue: `Opens` and `Clicks` deliberately
  overlap the shared fields.

## Round N+1: a justification for the right code, describing an unreachable case

Two findings, and the first is the more interesting one because the code it defends is
correct.

`knownCounterVocabulary` is deliberately wider than the six counters this client maps, and
the comment justified that with "a window in which an email was created but never sent can
come back carrying only `notsent`/`pending`". Copilot pointed out that case cannot reach the
guard: a never-sent email has an EMPTY `emails` list, and `GetEmailMetrics` returns
`ErrNoSentEmailInWindow` some two hundred lines above any counter is read. Checked, and it
is right.

The widening is still necessary — the real case is an email that WAS sent whose recipients
are all still `pending`, or suppressed per-recipient as `notsent`, with the six mapped
counters zero and therefore omitted. So nothing about the code changed; the comment, the
test's doc, and the knowledge doc each named the wrong case, in the same words, because the
second and third were written from the first.

**A wrong justification for correct code is not a cosmetic defect.** It is the thing a future
reader checks the code against, and the next person to notice that "created but never sent"
is unreachable has two conclusions available: the comment is wrong, or the widening is dead
and can be narrowed. The second one is a real regression, and the comment was the only
evidence pointing at it.

The second finding: the negative-counter table used `spamreport` as its "unrecognized" key —
but `spamreport` is IN the probe set, so every row took the branch that NAMES the key, and
the branch that deliberately refuses to name one had never run. That branch exists for log
safety: an unrecognized key is arbitrary upstream content rendered into a server log.
`TestGetEmailMetrics_NegativeUnrecognizedCounterIsNotNamed` covers it now, with all six
mapped keys present and non-negative so the earlier guards cannot fire first and skip it.
Revert-verified: naming the key makes it fail with the marker in the diagnostic.

**A fixture drawn from the same list the code checks against tests the branch you did not
mean.** Nothing about the test's name or its passing said so.
