# Known false positives — the floor, applied last

Claims a reviewer has raised on this repository that are **wrong**, each with the
maintainer's rebuttal and the present-day proof. Apply this file **after**
everything else: if a candidate finding matches an entry here, drop it silently.
Do not re-raise it in different wording, and do not argue with the rebuttal.

Every entry below was refuted by a developer on a real thread and re-verified
against `origin/main` `588cce6cd8a7fbee0f06d0672bff593e4512be18`. The repository
had no false-positive record before this file; these are the ones the evidence
supports.

---

## 1. `pgcrypto` is required for `gen_random_uuid()`

**The claim:** the migrations call `gen_random_uuid()` without enabling
`pgcrypto`, so they will fail on a fresh PostgreSQL. Raised on PRs #2, #9 and #10
and correctly refused all three times — **the highest-recurrence false positive in
the corpus.**

**Why it is wrong:** `gen_random_uuid()` moved into PostgreSQL **core in v13**. The
deployment target is PG 15+/16 under CloudNativePG.

**Threads:** [PR #10 `r3539916527`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/10#discussion_r3539916527)
— the developer replied: "it's a false positive: `gen_random_uuid()` moved into
**PostgreSQL core in v13** (it's no longer part of pgcrypto). Our target is **PG
16** (CloudNativePG), so no `CREATE EXTENSION pgcrypto` is needed". Also
[PR #2 `r3495449040`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/2#discussion_r3495449040),
where the reviewer then contradicted itself by calling the corrected note
inaccurate.

**Current proof:** the rebuttal is written into the migration itself —
`internal/infrastructure/postgres/migrations/000001_create_connection_tables.up.sql:9`:
`-- gen_random_uuid() is in PostgreSQL core since v13 (no pgcrypto extension).`

---

## 2. gitleaks `[[allowlists]]` should be singular `[allowlist]`

**The claim:** `[[allowlists]]` is not a recognised gitleaks section, so the
suppressions are ignored. Raised three times on PR #24.

**Why it is wrong:** since gitleaks **v8.25.0** the global key is `[[allowlists]]`
(an array of tables); the singular form now fails config load with
`'AllowList' expected a map, got 'slice'`. MegaLinter runs gitleaks **v8.28.0**.

**Threads:** [PR #24 `r3562071612`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/24#discussion_r3562071612)
and two siblings; the developer replied "No change needed: MegaLinter CI runs
gitleaks **v8.28.0**, and since **v8.25.0** the global key is `[[allowlists]]`
(array of tables)".

**Current proof:** `.gitleaks.toml` still uses the plural form, at lines 13 and 27.

**Note the adjacent rule that is *not* a false positive:** a *bare* `paths = [...]`
allowlist that mutes every gitleaks rule for a tree is a real problem — the repo
prefers fingerprint-scoped suppression. The false positive is the singular/plural
claim, not allowlist scoping.

---

## 3. `**Creation**` is reserved for the initial bundle entry in `log.md`

**The claim:** a new concept's `docs/knowledge/log.md` entry must use
`**Update** — …` because `**Creation**` is reserved for the bundle's first entry.
Raised on PRs #20, #21 and #22.

**Why it is wrong:** the premise is false, and the reviewer contradicted its own
advice on the same PR — having asked for `**Update**`
([`r3563524814`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/20#discussion_r3563524814),
briefly complied with in `19f6438`), it then asked for the two lines to be
collapsed into "one valid entry"
([`r3575026604`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/20#discussion_r3575026604)),
which the developer did as a single `**Creation**` entry in `d7de627`.

**Current proof:** `docs/knowledge/log.md` on `origin/main` contains **13**
`**Creation**` entries, including the ones that were flagged.

Encoding this would push reviewers to fight a settled convention. The choice
between `**Creation**` and `**Update**` is not a finding.

---

## 4. A fully-omitted `PG*` set contradicts FR-009 / the service should exit non-zero

**The claim:** omitting all PostgreSQL settings should fail startup. Raised three
times on PR #23.

**Why it is wrong:** the no-database / metadata-only mode is an **intentional
supported configuration**. FR-009 applies to an *incomplete* `PG*` set, not to a
fully-omitted one, and production charts always inject `PG*`.

**Current proof:** the distinction is in the code and the README.
`internal/infrastructure/config/config.go:129-131`: "An empty database
configuration remains allowed for unit tests and metadata-only local runs (no-DB
mode). Production charts inject PG* so this path is not used in-cluster."
`config.go:142`: "An explicit PGPORT or PGENGINE alone counts as partial
configuration (FR-009)." `README.md:24-27`: "When any PostgreSQL setting is
supplied, the set must be complete or the process exits non-zero. Fully omitting
all database settings is allowed for unit tests / metadata-only local runs".

---

## 5. `startupProbe` on `/readyz` wrongly couples startup to the database

**The claim:** pointing the startup probe at `/readyz` makes pod startup depend on
PostgreSQL. Raised on PR #23.

**Why it is wrong:** it is deliberate. The platform chart contract requires
readiness **and** startup on `/readyz`, with `/livez` staying process-only, and
the container is built to boot in 503 mode rather than crash when the database is
unreachable.

**Current proof:** `charts/lfx-v2-campaign-service/templates/deployment.yaml`
carries `livenessProbe` on `/livez`, `readinessProbe` on `/readyz` and
`startupProbe` on `/readyz` with a `failureThreshold: 90` / `periodSeconds: 1`
budget, and an in-template comment explaining that the process "no longer exits
when the DB is unreachable at boot: NewContainer boots in 503 mode and retries
migration/pool in the background, so /readyz stays 503 (not a crash) until the DB
is up and this probe keeps the pod alive across the window."

---

## 6. Third-party ad-platform API assertions

**Not one claim but a recurring class.** A reviewer asserts a fact about a vendor's
API — an enum spelling, a required field, a response shape, a character limit —
and is refuted with the vendor's own documentation. **Do not raise findings of this
shape at all.** They cannot be checked from a diff, and on this repo they have been
wrong more often than right.

Confirmed instances, each refuted with a vendor doc citation:

- **LinkedIn `WEBSITE_CONVERSION` is misspelled; the enum is plural.** Wrong —
  singular is correct.
  [PR #22 `r3607536973`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/22#discussion_r3607536973):
  "This is a false positive — `WEBSITE_CONVERSION` (singular) is the correct enum",
  with the LinkedIn Campaign Objectives doc quoted.
- **LinkedIn cursor pagination puts `nextPageToken` under `paging.metadata`.**
  Wrong — it is a top-level `metadata` object.
  [PR #22 `r3607591811`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/22#discussion_r3607591811):
  "False positive. LinkedIn's Cursor-Based Pagination doc shows the token under a
  TOP-LEVEL `metadata` object".
- **Microsoft `Campaign.Languages` is required when adding a Search campaign.**
  Wrong — it is required for Audience campaigns, optional for Search.
  [PR #44 `r3648802598`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/44#discussion_r3648802598):
  "I checked this against the v13 Campaign object docs and the current code is
  correct here — no change needed."
- **Google Ads campaign-name length** — the reviewer gave two different limits
  (128 and 255) on this repo, so no numeric claim of this kind is safe to encode.

What *is* legitimate is the mutation-ordering consequence: if a value is
deterministically checkable from input, config or the clock, it must be checked
**before** the first paid create. Raise that, citing
`mutation-ordering-and-partial-results.md` — never the vendor fact itself.

---

## 7. `yq` is not installed in the release workflow

**The claim:** the tagged-release workflow shells out to `yq` without installing
it, so releases can break. Raised on PR #3.

**Why it is wrong:** GitHub-hosted `ubuntu-latest` runners ship `yq` (mikefarah)
preinstalled, and the workflow is deliberately in parity with the reference
`lfx-v2-project-service` release workflow.

**Thread:** [PR #3 `r3514171781`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/3#discussion_r3514171781)
— "We're intentionally keeping this in parity with the reference
lfx-v2-project-service release workflow, which uses the same
`yq '.name' charts/*/Chart.yaml` with no separate install step."

---

## 8. Findings raised against a stale diff position

**The shape:** re-raising something already fixed in an earlier round, because the
comment anchors to a diff position rather than to the current head. PR #20 shows a
godoc-placement issue re-raised after it was fixed.

**The rule for this reviewer:** verify every candidate against the **snapshot at
the target commit**, not against a remembered or diff-anchored line. If the code
at the cited path and line no longer has the defect, there is no finding.

---

## Settled by decision — do not re-litigate

These are **not** false positives. They were acknowledged as real and then
deliberately deferred or accepted, so raising them again adds nothing:

- **The detached dispatch-goroutine lifecycle gap** (PR #11) — acknowledged, held
  as an architectural follow-up. The code documents the residual gap and points at
  LFXV2-2665.
- **The Meta `leads` divergence from `@lfx-one/shared`** (PR #20) — a documented,
  intentional exception, written into `internal/platform/meta/client.go`.
- **Worker ownership / lease / heartbeat** (PR #11) — never implemented; recovery
  keys on a 15-minute stale-job cutoff and the code defends that choice in a
  comment. Proposing the lease design is architecture review, not code review.

---

## Adding to this file

A new floor entry needs the **maintainer's rebuttal thread** and the **present-day
proof** that the rebuttal still holds. A claim that merely went unfixed is not a
false positive — plenty of real findings go unfixed. Removing an entry lets the reviewer
surface that claim again, so it is a human-gated decision — surfacing it, not
blocking on it; what blocks is the developer's call.
