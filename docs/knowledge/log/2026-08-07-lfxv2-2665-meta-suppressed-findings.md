# 2026-08-07 — Meta idempotent create: the by-name lookup is now opt-in, created ids are gated

**Update** — Closed the suppressed Copilot findings on PR #79
(`internal/platform/meta/client.go`, its tests, and
`docs/knowledge/code/internal-platform-meta.md`).

The by-name reuse ran on every create. Reading `internal/service/orchestrator.go`
alongside `docs/api-catalog.md` showed that this is exactly backwards: **the lookup
was unreachable in the one case it helps and reachable only in the two where it
hurts.** It cannot help a retry after an ambiguous create, because the orchestrator
retains the partial row and answers "reconciliation required" rather than
re-dispatching — no retry ever arrives at the client. The cases it does reach are
both harmful. `buildCampaignName` is event/region/objective/project, so it is
deterministic but NOT brief-unique: a second brief sharing those four segments would
be attached to the first brief's upstream campaign and their spend would become
indistinguishable. And DELETE frees the (brief, platform) slot LOCALLY without
touching the ad platform, so the documented delete → re-dispatch flow — the supported
way to fix a campaign created with the wrong budget — would find the old campaign by
name (budget is not a name segment), reuse it and its ad set, and report success
while silently re-running the wrong budget.

The lookup is therefore gated on a new opt-in `CampaignInput.ReconcileByName`, false
at every call site. **The caller, not the client, is what knows a dispatch is being
resumed**; when LFXV2-2665's reconcile path lands it sets the flag. The capability is
in place and deliberately unreached rather than on by default.

Both created ids now pass `numericIDRE`. The two lookups already gate the ids they
return, and every consumer of a STORED id gates it again, which left the freshly
created campaign and ad set ids as the ONLY ones reaching `CampaignResult` — and from
there durable storage and `"/{id}/..."` paths on later calls — unvalidated. A 2xx
carrying `"123?fields=x"` or a padded `"123 "` would be persisted now and rejected
much later, at a call site with no idea a campaign was created. Both are classified as
a malformed SUCCESS, not a failure: Meta answered 2xx so the resource almost certainly
exists, it just is not addressable by id — so a name-carrying partial is returned and
the dispatcher (which gates on `result == nil`) keeps the claim rather than freeing the
pair for a duplicating retry.

The test suite's fake ids (`camp_1`, `adset_1`) were what hid this: **a fixture that
does not model the real contract cannot fail on a missing contract check.** Meta ids
are numeric, so every campaign/ad-set fake is now a realistic numeric id — except the
two fixtures that are deliberately non-numeric because they exercise the lookup's own
gate. Three new tests bind the changes, each revert-verified in ISOLATION: sabotaging
all three fixes at once made the un-gated lookup mask the id tests' diagnostics, which
is the whole reason to sabotage one at a time.

`docs/knowledge/code/internal-platform-meta.md` now records the gate and its reasoning,
the created-id validation, and a corrected ambiguity list — "ambiguous" is not just
transport/5xx but also a 2xx with no `data` field, a `paging.next` with no cursor, the
page cap reached with pagination pending, a match with an empty or non-numeric id, and
cancellation mid-enumeration. Only a dial error or a definite conflict (a match that is
not `PAUSED`, or whose objective differs) is a clean failure.

## Second pass — the throttle classification was collapsed into the retry decision

`do` computed one `throttled` flag and then, for creates, CLEARED it:
`if !retryThrottle { throttled = false }`. That reads as "creates are never throttled",
which is false — creates are shed exactly like everything else; what is different is only
that a create must not be REPEATED. The two facts were being carried in one variable, and
the terminal read-error path needs the one that got erased.

The concrete failure: Meta reports rate limiting as an HTTP 400 with a Graph rate-limit code
far more often than as a 429, and `createOutcomeAmbiguous` recognises the 400 shape by that
CODE — a bare 400 is an ordinary semantic rejection. On the truncated-read path the returned
`*APIError` carried only status/method/path, so a shed create whose body arrived complete but
whose read failed on a mismatched `Content-Length` (the common truncation shape — the JSON is
all there, the connection just closed early) classified as 400/code 0: a CLEAN failure. The
dispatcher then releases the claim, and the next attempt re-POSTs a create Meta may already
have committed. Duplicate PAID campaign, from a response we had the bytes to classify
correctly.

Two separate names now: `isThrottle` is the classification, `throttled := isThrottle &&
retryThrottle` is the retry decision, and the terminal `*APIError` carries whatever Graph
envelope did parse (`Type`, `Code`, `FBTraceID`). Preserving diagnostics on a read failure is
right on its own terms — support needs them precisely when a rate limit is hit — but here it
is load-bearing rather than cosmetic. `TestDoCreateTruncatedGraphThrottleStaysAmbiguous`
pins both shapes, with the 429 case as a control: it stays ambiguous on the STATUS alone, so
a regression that drops only the Graph code fails the 400 subtest and not the 429 one, which
is what localises it. Revert-verified in exactly that pattern.

**The dormant-capability claim leaked back into one sentence.** The reconcile-by-name lookup
is opt-in, false at every call site, and `internal-platform-meta.md` says so at length in two
places. The throttle paragraph nonetheless still ended "the next run's `findCampaignByName`/
`findAdSetByName` adopts what Meta committed" — describing an automatic reconciliation that
does not happen, in the one paragraph a reader hits FIRST. An UNCONFIRMED throttled create is
surfaced for verification in Ads Manager; nothing re-dispatches a retained partial. The
sentence now says that and points at the section that explains why the lookup is gated,
rather than contradicting it seventy lines later.
