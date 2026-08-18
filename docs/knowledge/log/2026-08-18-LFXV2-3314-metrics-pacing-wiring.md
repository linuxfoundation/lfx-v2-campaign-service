# 2026-08-18 — LFXV2-3314 pacing and action items on the brief-metrics read

**Update** — `GET /projects/{projectId}/briefs/{briefId}/metrics` now returns a `pacing` object
per readable row and a brief-wide `action_items` list, both derived from `internal/service/rules`.

**Why service-side.** Four UI implementations derived these independently and disagreed three
ways on the underspending floor; one disagreed with itself, labelling at 50 while alerting at 40,
so a campaign could render as healthy while raising an alert. Deriving once behind one set of
thresholds is the point of moving it.

**Pacing is computed against the row's OWN window.** Each row records the window it was actually
read over, which is not always the requested one — with no window named the default is
platform-aware and X Ads falls back to a narrower range. `WindowDays` maps that window to a day
count, and expected spend is prorated over `min(windowDays, elapsedFlight)`. Using the elapsed
flight instead would compare a 7-day spend against 20 days of plan and report an on-track
campaign as spending a third of what it should.

**`pacing.pct` is a pointer and stays absent when incomputable**, with `label` reading `unknown`.
A zero there is indistinguishable from a campaign that spent nothing, which is the same
substitution the row statuses already exist to prevent — this endpoint refuses to zero-fill
`metrics` for exactly that reason and pacing must not reintroduce it one field over.

**Unreadable rows raise no action items.** They have no impressions and no spend to look at, so
evaluating them would fire `zero_delivery` for every failed read and report an outage as a dead
campaign. An empty `action_items` therefore means "nothing flagged among the READABLE rows" —
`ok_count` is still what tells a consumer how much of the brief that covers.

**CTR is converted at the boundary.** `CampaignMetrics.Ctr` is a ratio (0.02 = 2%) on every
adapter and in the design's own example; the rules take a percentage. Passing the ratio through
would make the low-CTR threshold a hundred times too strict and flag every healthy campaign.

**The clock is injected** on `BriefService` (`SetClock`), matching the platform clients' existing
`WithClock`. Pacing is date arithmetic, and a test that reads the wall clock passes or fails by
when it is run. One `now` is read per request rather than per row, so a request spanning midnight
cannot pace one campaign against a different day than its sibling.
