# Mutation ordering and partial results

The largest empirical review class on this repository: **77 Copilot comments
across 13 pull requests, with confirmed fixes in five platform packages.** Both
patterns below are about the same underlying fact — a create flow in this service
spends real money at an ad platform, and the service cannot delete what it
created. There is no compensating-transaction path anywhere in the tree, and
automatic reconciliation does not exist (deferred to LFXV2-2665 in a dozen code
comments).

---

## validate-all-deterministic-input-before-first-mutating-create

**Severity:** `critical` when the orphaned resource is a campaign, ad set, ad or
budget; `high` for a non-paid upstream object.

**Detect:** Find the first mutating upstream call in a create flow — a
`doRequest(ctx, http.MethodPost, …)`, a `:mutate` endpoint, or any helper an
`isMutatingMethod` check would classify as mutating. Then scan forward for any
`return nil, fmt.Errorf(...)` (or equivalent early return) whose condition
depends **only** on the caller's input, the client's own config, or the injected
clock: a length or rune-count limit, a regex or enum membership test, an ISO date
parse, a start/end ordering rule, a budget bound, or a required-field check. Any
such check placed after the first mutating call is a finding: the deterministic
error fires only once a paid resource already exists.

**Why it matters:** the failure is a *paid* PAUSED campaign, ad set or budget
that the service cannot identify, cannot clean up, and that a retry duplicates.
Every one of these inputs was knowable before the first byte left the client.

**Evidence:**

- Copilot threads on PR #20 (Meta client), each naming the orphan outcome
  explicitly:
  - [`r3562452866`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/20#discussion_r3562452866)
    — a shape-only date regex admits `2026-13-40`; "The campaign is then created,
    but Meta rejects the later ad-set timestamps, leaving an orphaned campaign."
    Fixed in `b270547`.
  - [`r3562597179`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/20#discussion_r3562597179)
    — country code validated for shape, not ISO 3166 membership; "Validate
    against actual supported country codes before the first mutating request."
    Fixed in `86046a6`.
  - [`r3562658867`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/20#discussion_r3562658867)
    — a past start date passes, the campaign POST succeeds, Meta rejects the
    ad-set `start_time`. Fixed in `8b9f602`.
- Developer fixing commits across the class, all on merged PRs:
  `00f6de1b0`, `48ed5065f` ("validate composed creative name length before any
  POST") and `b270547` on **#20**; `49981a7` ("anchor post-path regex and
  validate ISO geo codes") and `c6dc0ea` ("validate composed ad-group name
  before the campaign POST") on **#21**; `7d899d0` on **#22**; `275a42a` on
  **#39**.
- The fixes carry regression tests whose `httptest` handlers `t.Errorf` on **any**
  POST — the test asserts that nothing was created, which is the shape a new fix
  should copy. See `TestCreateCampaign_RejectsOverlongNameBeforeAnyPOST`
  (`internal/platform/reddit/client_test.go`).

**Status on main:** the convention is held in the merged clients and is stated in
the knowledge bundle — `docs/knowledge/code/internal-platform-meta.md:44`:
"Inputs are validated up front, before any mutating call". Sibling concepts for
twitter and googleads say the same. In `internal/dispatch`, the adapters push the
same gate one layer out: every pre-create failure returns `notCreated(...)`
before the client's `CreateCampaign` is reached.

**Not a finding when:** the check genuinely needs upstream state (an id the API
returned, a lookup result, a vendor-side validation outcome) — that cannot be
hoisted. A non-fatal per-variant failure after the campaign exists is the
documented design, not this pattern; it belongs to the partial-result entry
below. Do not turn this into an assertion about a vendor's own limits or enum
spellings — see `known-false-positives.md`.

---

## return-and-persist-a-partial-result-instead-of-nil-err

**Severity:** `critical` for the orchestrator variant (a retry reports success on
an unreconciled orphan); `high` for the client variant.

**Detect:** Two halves; either alone is a finding.

*Client:* after the first successful mutating create in a flow, any `return nil,
…` is a finding. This repo's convention is a partial-result closure —
`partialResult()`, `campaignNamePartial()`, `budgetPartial()` — returned
**alongside** the error, carrying the reconcile key for the resource actually at
risk (the created id, or the deterministic name when no id exists yet).

*Orchestrator/adapter:* any persistence, claim-release or idempotency decision
gated on `PlatformCampaignID != ""` alone. An ambiguous partial legitimately has
an empty id, so the guard must also admit `len(Result) > 0`, and a reuse decision
must key on a terminal status rather than on the id's presence.

**Why it matters:** returning `(nil, err)` after a create discards the only
handle to a paid resource. The orchestrator half is worse: it releases the claim,
so the next retry creates a *second* paid campaign and reports success.

**Evidence:**

- [`r3563472282`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/20#discussion_r3563472282)
  (PR #20) states the contract: "At this point the campaign has already been
  created and `campaignID` is known, but an ad-set failure returns `(nil, err)`.
  The caller therefore cannot identify or clean up the orphaned PAUSED campaign,
  and retrying `CreateCampaign` creates another one." Fixed in `aeb6fff`.
- [`r3564444904`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/20#discussion_r3564444904)
  (PR #20) — the same defect one level down: an ad failure discards the created
  creative's id, "leaves an unidentifiable orphan creative in the account". Fixed
  in `719d049`.
- Developer fixing commits, all on merged PRs: `95f3bedb5` ("return partial
  result with created IDs on downstream failure") on **#20**; `d55f054`
  ("surface partial state when ad-group creation fails after campaign") on
  **#21**; `b24cc05` ("carry CampaignBudgetName in partials; clean pre-send ctx
  failure") on **#33**; and the three orchestrator fixes on **#36** —
  `aa1f01e` ("don't fast-path a pending orphan as a completed success"),
  `7c62de9` ("treat a retained orphan on retry as a failure, not skip") and
  `5cc908b` ("reconcile id-less partial orphans, don't hide as skip").
- Six files, six merged PRs.

**Status on main:** the contract is written into the code —
`internal/platform/meta/client.go:1761` and
`internal/platform/reddit/client.go:1014` both open with `PARTIAL-RESULT
CONTRACT:`, and `internal/platform/googleads/campaign.go:424,480` implement the
two-stage `campaignNamePartial` / `budgetPartial` closures. The knowledge bundle
states the orchestrator half at
`docs/knowledge/code/internal-dispatch.md:61`: "The decision keys on
`result == nil` ALONE — NOT on whether the campaign id is populated".

**Not a finding when:** the failure is provably pre-create — a
`preCreateError` / `NoUpstreamCreate()` path, or a `preSendError` — in which case
`(nil, err)` is correct and the claim *should* be released.

**Two different predicates, and neither replaced the other.** Assert the
*reuse/skip* half against the named `isReusableCampaign`
(`internal/service/orchestrator.go:88`, used at `:577` and `:621`) — that is the
question "may I skip creating this again?".

The *orphan-persistence* half is a different question — "is there a reconcilable
artifact worth recording or reporting?" — and it is asked inline, on purpose, at
`internal/service/orchestrator.go:642` (retained orphan found on retry) and `:716`
(persist-on-error). Both read
`PlatformCampaignID != "" || len(Result) > 0`. These are **current, not
historical**: `isReusableCampaign` would be wrong here because it also requires a
non-pending status and excludes `partialOrphanStatuses` — exactly the rows the
orphan paths exist to catch. Do not flag an inline check on those two paths, and do
not stop auditing them because a named predicate exists elsewhere.
