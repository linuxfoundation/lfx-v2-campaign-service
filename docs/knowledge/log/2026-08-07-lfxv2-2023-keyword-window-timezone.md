# 2026-08-07 — LFXV2-2023: the keyword window has a timezone, and 429 is not a definite 4xx

**Update** — Eight more suppressed Copilot findings closed on PR #81. The keyword report now
specifies `segments.date DURING LAST_N_DAYS` instead of interpolated dates; the batch mutate
returns before contacting Google Ads when authorization refused every criterion; 429 is excluded
from the "definite 4xx" prose in both the plan and the previous log fragment; and `design.md`
distinguishes an ABSENT optional property from a `null` one.

**Fix** — `dateRangeForDays` computed the window from `time.Now().UTC()`. GAQL compares
`segments.date` against calendar dates in the **customer account's** timezone
(`customer.time_zone`), which is set per account and is usually not UTC, so for every hour
between the account's local midnight and UTC midnight the two disagree about what day it is and
the window silently shifts by one — daily, for as long as the offset lasts. That defeats exactly
the legacy-window alignment Open Question 4(b) exists to settle. The plan now emits the GAQL
literals, which have no clock in this process at all, resolve the calendar where the data is
stored, and give completed-day semantics for free — the same three literals the UI's
`resolveDateRange` already sends, so the numbers tie out against the screen being replaced. 4(b)
stays open, but its price is now stated: include-today cannot be expressed with `LAST_N_DAYS`,
so it costs resolving and caching `customer.time_zone` and computing dates in that location, plus
a test using a fake clock in a NON-UTC location — a test that reads `time.Now().UTC()` on both
sides passes against the very defect it exists to catch.

**Fix** — 429 was used as the example of a definite request-level rejection in the plan and again
in `2026-08-07-lfxv2-2023-keyword-plan-phase-boundary.md`, both times as "quota refusal". That is
the opposite of what the helper the same paragraph defers to actually does:
`createOutcomeAmbiguous` (`campaign.go:383`) tests `ae.StatusCode == http.StatusTooManyRequests`
with no "was it retried" qualifier and returns true, because throttling may be applied before the
API sees the request or after it has accepted and counted it and the response does not say which.
A durable log teaching `429 → failed` while citing a helper that says `429 → unconfirmed` is
worse than saying nothing. Both now read "definite 4xx other than 429", name an unauthorized
customer as the example, and state why 429 is the one 4xx that behaves like a 5xx.

**Fix** — The batch path called `MutateKeywordCriteria` even when `ops` was empty. `ops` is empty
only when authorization refused EVERY requested criterion — precisely the case the planned
foreign-criterion test pins as "must fail, not mutate" — so the call sends an empty mutation
Google Ads may reject, and puts a request-level error in front of per-action outcomes that are
already decided and correct. It returns the outcomes instead.

**Fix** — Three shape corrections on the Goa types. The batch result dropped the top-level
`success` the current UI reads while claiming per-item compatibility; it is restored as
deprecated and defined as `succeeded == total`, deliberately not `failed == 0`, which would
report a batch containing unconfirmed operations as a clean success — the exact fold the
three-state design prevents. And `quality_score` / `max_cpc_micros` were documented as `null`:
an optional Goa attribute generates `omitempty`
(`gen/http/lfx_v2_campaign_service_briefs/server/types.go:225`), so a nil pointer drops the
property entirely and never serialises as `null`. The transcribed UI contract says
`qualityScore: number | null`, which is a different shape; the plan now names both options and
recommends coalescing at the client boundary. `design.md` carries the same distinction, since it
is meant as reusable guidance and previously said only "nullable".

**Fix** — The plan's **Status** line still said dependencies and phasing awaited a human decision,
which the document itself resolves several sections later (PR #69 merged; the three-PR split is
fixed). It now names the four decisions that genuinely remain and points at them by number.
