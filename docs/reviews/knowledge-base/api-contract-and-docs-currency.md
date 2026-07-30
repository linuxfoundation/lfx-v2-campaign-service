# API contract and documentation currency

Three patterns that share one root cause: campaign creation is **asynchronous**
and `CreateCampaigns.config` is an **untyped opaque object** in `design/`. Goa
therefore validates none of it, and a mismatch between what the docs promise and
what the code accepts surfaces as a `202 Accepted` followed by a dead job — with
no synchronous signal to the caller. That makes the written contract the only
thing a consumer can build against.

---

## design-contract-looser-than-runtime

**Severity:** `high`.

**Detect:** A new or tightened runtime rejection in `internal/service/**` or
`internal/platform/**` with no matching constraint added to `design/` —
`Required`, `Pattern`, `MinLength`, `MaxLength` or `Enum` — **and** regenerated
`gen/` output, in the same patch. The named symptom is an API that accepts and
persists a value that can never succeed: an `active` connection row that can never
dispatch, or a documented example the endpoint always rejects.

Watch the inverse too: Goa's `Required` checks **presence, not length**, so a
`Required("platforms")` array still admits `[]`. A presence-only constraint behind
a loop that must run at least once is the same defect.

**Why it matters:** CI cannot catch this. `make apigen` runs **before**
`check-fmt`/`lint`/`build`/`test` with no `git diff --exit-code` afterwards, so CI
compiles and tests freshly generated code rather than the code in the repository.
Committed `gen/` drift is masked, not merely unchecked — and `gen/` is excluded
from every linter and from the license-header check.

**Evidence:**

- [`r3553978593`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/11#discussion_r3553978593)
  (PR #11) is the presence-vs-length half: "An empty `platforms` array is accepted
  here. The design declares `Required("platforms")`, but Goa's `Required` only
  checks presence, not length, so a request with `\"platforms\": []` passes
  decoding." Fixed in `3448c95`.
- [`r3562430964`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/11#discussion_r3562430964)
  (PR #11): "The DSL accepts arbitrary strings even though the handler only accepts
  `model.Provider` values. As a result, OpenAPI clients cannot discover valid
  values and Goa generated the advertised create-campaigns example with three
  values that the endpoint always rejects." Fixed in `e8da570`.
- Developer fixing commits on merged PRs: `b51d8cb8c` on **#11**; `d2f560865`
  ("address PR #38 follow-up review feedback") on **#38**; plus the same class on
  **#36** and **#39**.

**Status on main:** `internal/apivalidation` exists specifically to hold tests
that exercise the generated validators from outside the generated tree — its
`internal/apivalidation/doc.go` explains that this "avoids hand-editing anything under gen/ (DO NOT
EDIT)". A datastore `CHECK` constraint is the belt-and-braces companion where a
`design/` enum guards only request time:
`docs/knowledge/code/internal-infrastructure-postgres.md:46-48` notes that "the
DSL `Enum(\"hubspot\")` guards it only at request time, so a direct/worker write
could otherwise persist an unsupported platform."

**Not a finding when:** the rejection is genuinely dynamic — it depends on
upstream state, a lookup, or a per-account value Goa cannot express. `gen/` changes
without `design/` changes are a `repo_code` concern (the generated boundary), not
this pattern.

**Also not a finding when the tightened value lives inside
`CreateCampaigns.config`.** That field is typed `Any`, so **no** `design/`
constraint can express it and demanding one is a false positive — a static
platform-config rule such as a budget minimum is not *dynamic* under the clause
above, so it would otherwise match this Detect on a technicality. The pattern that
owns that surface is [`api-catalog-platform-config-drift`](#api-catalog-platform-config-drift)
below, which asks for the `docs/api-catalog.md` update instead. Raise one or the
other for a given change, never both.

---

## docs-must-not-advertise-what-the-code-rejects

**Severity:** `high`.

**Detect:** A godoc comment, a `docs/api-catalog.md` entry, or a **validation
error message** that enumerates accepted values, defaults or behaviour which a
guard in the same flow rejects or implements differently. The highest-signal form
is an error message listing valid options generated from one source while the next
check rejects one of them — the caller follows the guidance and receives a second
error.

**Why it matters:** a consumer that follows the documented contract launches a
*materially different paid campaign* than the one described, or gets a `202`
followed by a dead job with no synchronous signal.

**Evidence:**

- [`r3562716245`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/20#discussion_r3562716245)
  (PR #20): "This error advertises `leads` as a valid objective via
  `objectiveKeys()`, but the next validation unconditionally rejects `leads`.
  Callers following this guidance receive a second error. Until lead-form support
  is implemented, list only the four objectives accepted by `CreateCampaign` and
  update the …". Fixed in `302c3ce`; `objectiveKeys()` was later refactored to
  derive from the source-of-truth map so it cannot drift again — that refactor is
  the shape to prefer over a second hand-maintained list.
- Developer fixing commits on merged PRs: `24b1e999c` ("today-start ad set time;
  drop leads from field doc") on **#20**; `cdd3e0882` ("correct metaConfig leads +
  currencyOffset semantics") on **#38**; `8ea7cf0e5` ("point the uniqueness rule at
  the migration") on **#48**.

**Status on main:** the pattern's fix direction is established: derive the
advertised list from the same map the guard reads, rather than maintaining two.

**Not a finding when:** the divergence is a documented, intentional exception. The
Meta `leads` divergence from `@lfx-one/shared` is settled and written into
`internal/platform/meta/client.go` — do not re-litigate it. Pre-existing doc drift
the patch does not touch is not this pattern; the trigger is the patch making a doc
or message stale, or adding a new one that is already wrong.

---

## api-catalog-platform-config-drift

**Severity:** `high`.

**Detect:** A patch that changes what a platform config struct accepts,
defaults, or requires — the effective public shape of `CreateCampaigns.config` —
without the matching `docs/api-catalog.md` update. Both directions count: the
catalog marking a field required that the client happily defaults, and the catalog
permitting values the client rejects.

**Why it matters:** `CreateCampaigns.config` is typed `Any` in `design/`, so Goa
validates nothing and the catalog **is** the consumer-facing validation contract.
Because creation is asynchronous, a mismatch produces a `202` and a job that dies
later; the caller gets no synchronous error to debug against.

**Evidence:**

- [`r3633024948`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/38#discussion_r3633024948)
  (PR #38) states the reasoning outright: "Because `CreateCampaigns.config` is
  untyped, this catalog is the consumer-facing validation contract, but it
  currently allows values the Meta client rejects: the budget must be positive and
  round to at least one minor unit, the start date cannot be before today (UTC),
  and the end date must be strictly later." Fixed in `e8aa6b7`.
- [`r3627737588`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/38#discussion_r3627737588)
  (PR #38): the config struct "is now the effective public Meta campaign-config
  contract, but `CreateCampaigns.config` is typed as `Any` and
  `docs/api-catalog.md:272` still describes `metaConfig` only as an unspecified
  object." Fixed in `59e93fd`.
- [`r3631728513`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/38#discussion_r3631728513)
  (PR #38) is the required-vs-defaulted direction: the catalog "marks `geoTargets`
  as required, but the Meta client explicitly accepts omission or an empty list and
  defaults it to `US`". Fixed in `6dbf94b`.
- Developer fixing commits on merged PRs: `51e4e6b` on **#21**; `afd584108`
  ("document client-enforced metaConfig validation constraints") on **#38**; plus
  the same class on **#39**.

**Status on main:** the catalog carries per-platform config sections that were
brought into line by the #38 fixes. Whether a given field is still accurate is a
per-field question — read the client, not only the catalog.

**Not a finding when:** the change is internal and does not alter what a caller
may send. A field renamed inside a Go struct with no JSON tag change is not a
contract change.
