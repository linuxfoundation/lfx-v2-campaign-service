# 2026-10-04 — Collapsing duplicate proximity targets before the budget mutate

**Fix** — `validateProximityTargets` (`internal/platform/googleads/geo.go`) emitted
an exact duplicate as a duplicate campaign criterion. Google rejects a duplicate
proximity criterion, and it rejects it at the `campaignCriteria:mutate` — which runs
AFTER the budget and the campaign are committed. That made a locally decidable input
error cost a real paid campaign, which is precisely the preflight guarantee this
validator exists to keep.

It now collapses a repeat, keyed on the rendered `proximityInfo` value (microdegree
coordinates, radius, normalised unit). `proximityInfo` is comparable, so it is the
map key directly; a NaN key cannot arise because NaN is refused earlier in the same
loop iteration.

**Why this list collapses rather than refusing.** The repo already runs both rules.
`resolveGeoList` collapses by resolved id; `validateAdSchedules` and the device
bid modifiers REFUSE a repeat whose bid modifiers disagree. The difference is in the
payload, not the policy: those carry a bid modifier, so two entries naming one slot
can disagree and one of the two values would be silently dropped. A proximity target
carries no such field, so two entries rendering to the same tuple say the same thing
and nothing is lost by keeping one. Refusing here would have been over-refusal —
failing a create Google would have accepted.

**What is deliberately NOT collapsed.** Units are not converted: 10 MILES and 16.09
KILOMETERS is a conversion rather than a spelling, and whether Google treats the two
as one criterion is not locally decidable. Same line `validateAdSchedules` draws at
interval overlap. The over-refusal guard test pins this, along with targets differing
only in latitude, longitude or radius.

**Cap ordering.** `maxProximityTargets` is still checked against the SUBMITTED count,
before any collapsing, which is where `resolveGeoList` checks its own. The cap bounds
the request a caller may send, not the criteria that survive it: a list longer than
the cap means the caller has lost track of what it is asking for, and shrinking it
into the cap would hide that.

**Docs.** `docs/api-catalog.md` is the consumer-facing validation contract for
`CreateCampaigns.config` (typed `Any` in `design/`), so both halves of this landed
there: the proximity dedupe rule, and a correction to the `geoTargets` paragraph,
which said a numeric id is "checked only for SHAPE" without saying how strict that
shape is. `resolveGeoEntry` requires the canonical base-10 spelling of a positive
int64 — `0`, `02840` and a 21-digit overflow all fail locally, before any request.
Only EXISTENCE is left to Google, and that distinction is the one the paragraph
needed to keep.
