# Campaign service local review knowledge base

Empirical review patterns for `lfx-v2-campaign-service`, extracted from verified
past PR review comments on this repository. This directory is the **single
repo-owned home** for this repo's review pattern evidence and false-positive
decisions; the reviewer skill holds the review *method* and loads this path, so
there is deliberately no second copy of the evidence under the skill's own
directory.

Read by the `campaign-service-learnings-reviewer` brain (the learnings role of the
local pre-PR reviewer trio) and by humans deciding whether a pattern still earns
its place. It is plain documentation: nothing here is wired into `.github/**`, and no
PR-side tooling in this repo consumes it today.

## Scope

These entries describe **what reviewers of this repo actually caught and
developers actually fixed**. They are not a style guide, not a restatement of
`CLAUDE.md`, and not general Go advice. The repo's written rules are the
`campaign-service-code-reviewer` brain's surface; this one is purely empirical.
Where the two overlap — knowledge-bundle upkeep, contract currency, test
synchronisation — the entry here exists because it recurred in review practice
before a PR was ever opened, and it is phrased as a mechanical detect condition
rather than as prose about the rule.

## Evidence base

All entries derive from the LFXV2-2896 read-only evidence scout (epic
LFXV2-2889, 2026-07-29), which mined **all 50 pull requests** of this repository
— created 2026-06-29, so the window is effectively its whole history — and
collected **1,603 Copilot inline review comments**, of which 1,040 of 1,552
reply threads cite a fixing commit SHA.

- **Reviewer identity**, verified from the GitHub API rather than inferred:
  inline comments are authored by login `Copilot`, `user.id 175728472`,
  `node_id BOT_kgDOCnlnWA`, `type Bot`, app
  `https://github.com/apps/copilot-pull-request-reviewer`; the same actor's
  review submissions appear as `copilot-pull-request-reviewer[bot]` with the
  identical id.
- `cursor[bot]` (`user.id 206951365`) is also active on PRs #27–#50 and is
  **excluded** from this evidence base. No CodeRabbit and no Greptile activity
  exists on this repo.
- **All present-day claims were read at `origin/main` =
  `588cce6cd8a7fbee0f06d0672bff593e4512be18`** ("feat(platform): add microsoft
  ads campaign creation (MS-2) (LFXV2-2804) (#49)", 2026-07-28), confirmed
  through the GitHub API. `main` has since advanced; entries state where a
  referenced path lives today rather than assuming that SHA is current. When an
  entry's "Status on main" and the code disagree, the code wins and the entry
  needs updating.

## Promotion gate

An entry is in this knowledge base only if **all** of these hold:

1. A **verified Copilot review comment** raised it, with the thread URL recorded.
2. A **developer fixing commit exists and its hunk was read.** No readable
   fixing hunk, no entry. Commits from squash-merged or force-pushed branches
   were read through the GitHub commits API where they are unreachable from
   `origin/main`.
3. The fixing commit landed on a **merged** pull request. Findings raised on open
   or closed-unmerged PRs count toward recurrence only and are never cited as a
   fix.
4. The condition is **still relevant to the current code** at the SHA above.
5. The condition is **mechanically detectable from a diff** plus a bounded look
   at the target commit — not a judgement about design, and not a fact about a
   third-party API.
6. No deterministic check in this repo already catches it (see
   *Why review-only*).

Generic advice, unmerged-only fixes, and vendor-API assertions are not patterns.

## Why review-only

Every entry here is invisible to this repo's automated pipeline, which is what
makes a reviewer necessary rather than redundant. As of the SHA above:

- `make lint` runs bare `golangci-lint` with **no `.golangci.*` config**, so only
  the default set (errcheck, govet, ineffassign, staticcheck, unused) — no
  `gosec`, no `bodyclose`, no `noctx`.
- `GO_GOLANGCI_LINT` is **disabled** in `.mega-linter.yml`, and `gen/`, chart
  templates and `.specify/` are filtered out of most linters.
- gitleaks scans committed content only, and `.gitleaks.toml` allowlists **every
  `*_test.go` file from every rule**.
- `.github/workflows/validate-okf.yml` is path-filtered to `docs/knowledge/**`
  and the okf tooling, so a code-only PR that skips the bundle never runs the
  validator.
- CI runs `make apigen` **before** `check-fmt`/`lint`/`build`/`test` with no
  `git diff --exit-code` afterwards, so committed `gen/` drift is masked rather
  than merely unchecked.

`make test` does run `go test -race` (`Makefile:95`), which is why the test-hygiene
entries are written around *unsynchronised handoff* rather than around whether a
race detector exists.

## Categories

| File | Patterns |
|---|---|
| [mutation-ordering-and-partial-results.md](mutation-ordering-and-partial-results.md) | validate before the first paid create; return and persist a partial result |
| [context-and-cancellation.md](context-and-cancellation.md) | `ctx.Err()` as the caller-cancel gate; clean pre-send vs uncertain in-flight |
| [credentials-and-untrusted-text.md](credentials-and-untrusted-text.md) | untrusted or credential text in errors; caller-URL redaction |
| [platform-http-client-hygiene.md](platform-http-client-hygiene.md) | bounded reads of `cap+1`; drain before close |
| [test-hygiene.md](test-hygiene.md) | pinned injected clocks; synchronised handler-goroutine handoff |
| [api-contract-and-docs-currency.md](api-contract-and-docs-currency.md) | design stricter than runtime; docs must not advertise what the code rejects; opaque platform config |
| [knowledge-bundle-upkeep.md](knowledge-bundle-upkeep.md) | concept + index bullet + dated log entry with a package change |
| [known-false-positives.md](known-false-positives.md) | the floor, applied last |

## Entry format

Each entry carries:

- a stable **pattern id** as its heading — this is the pattern name a finding
  cites;
- **Severity** guidance;
- **Detect** — the mechanical condition, phrased so it can be applied literally
  and quoted verbatim into a finding;
- **Why it matters** — the cost of a miss in this repo, which is what sets
  severity;
- **Evidence** — Copilot thread URLs, the developer fixing commits (short SHA and
  what the hunk changed), the merged PRs, and the recurrence count;
- **Status on main** — present-day confirmation at the SHA above;
- **Not a finding when** — the boundaries, including any known-false-positive
  interaction.

## Deliberately excluded

The scout produced 25 candidates. This base carries the 14 with the strongest
recurring and present-day evidence. Held back for separate human or agentic
review, and **not** to be reconstructed here by inference:

- **Lower-recurrence patterns (P15–P25)** — payload-logging middleware on
  credential endpoints, applied-migration immutability, hand-written source under
  `gen/`, kodata OpenAPI copies, nil-clobbering options, credential normalisation
  at construction, gitleaks allowlist scoping, OKF description/index desync and
  log-append hygiene, request-body assertions in happy-path fakes, and
  partial-success tests for non-fatal loops. Several are real and well evidenced;
  they were not bulk-promoted.
- **Live defects on `origin/main`** surfaced by the audit. They are repo work,
  not review patterns.
- **Structural CI gaps** — the masked `gen/` drift, the disabled Go linter, the
  test-file gitleaks blind spot, the path-filtered OKF validator. Recorded above
  as the reason these patterns are review-only; repairing them is separate work.
- **Third-party ad-platform API assertions.** See
  [known-false-positives.md](known-false-positives.md).
- **Settled-by-decision items** that a reviewer must not re-litigate: the
  detached-dispatch-goroutine lifecycle gap, and the Meta `leads` divergence from
  `@lfx-one/shared`.
- **The PR-side reviewer amendment.** The repo's own PR review surface has no
  coverage of mutation ordering, orphaned paid resources or the partial-result
  contract — the largest Copilot finding class here. That gap is real and is a
  separate, human-gated proposal. Nothing in this directory changes it, and
  nothing here is wired into `.github/**`.

## Corrections on record

Numbers and labels in this directory are derived from sources, so a correction is
itself evidence. Superseded figures stay here with the probe that produced them —
a claim is not improved by quietly replacing the number, and the next person to
re-derive it needs to know which reading was measured.

**A failed probe is not a refuted claim.** Validate the probe against the source
before you weaken an entry: read the file, then decide.

- **`test-hygiene.md` — P8 comment count (`14` → `12` core / `16` full scope).**
  The original `14` is not reproducible under any filter definition we can state.
  Re-derived from the **3,850-comment corpus** — every inline review comment on
  PRs #1–#50 from *all* authors (developers, `cursor[bot]` and Copilot), of which
  the 1,603 Copilot-authored comments named in *Evidence base* are the subset this
  probe filters down to — restricted to Copilot-authored inline
  comments on `*_test.go` paths in merged PRs #19, #20, #21 and #29: the core
  shape (`unsynchronized`, `handler goroutine`, `without ... happens-before`,
  `data race`, `t.Fatal`, `FailNow`) gives **12**; adding this entry's own
  *also flag* variants (`time.Sleep`, fixed-ms waits, unbounded receive,
  `hangs the entire`, race-prone, flaky) gives **16**. The vendor count (4) and
  merged-PR set (4: #19, #20, #21, #29) verified unchanged in both readings, so
  only the comment total was wrong. The same widened probe also matches #23 (1)
  and #47 (6); both are correctly outside the entry's platform-client vendor
  scope, and **#47 is still open** — no unmerged PR contributes evidence here.
- **The gitleaks test-file blind spot — failed probe, claim upheld.** A first
  check ran `grep -c '_test\.go' .gitleaks.toml` and returned `0`, which made the
  *test files are excluded from secret scanning* claim look false. The probe was
  wrong, not the claim: as a basic regular expression that pattern matches the
  literal text `_test.go`, while the file carries the backslash-escaped form
  `'''.*_test\.go$''` at `.gitleaks.toml:16`. Reading the file settled it. The
  claim stands, and this disclosure stays as the worked example of the rule above.
- **Microsoft *upstream-only* path labels — removed as false.** Several entries
  once labelled `internal/platform/microsoft/**` paths as present upstream but not
  on this branch's base. Rebasing onto current `origin/main` made those labels
  false, because the paths exist locally now. `microsoft` was moved into the plain
  unadopted-sites list in `test-hygiene.md`, and the `ctx.Err` entry in
  `context-and-cancellation.md` gained a direct quote from
  `docs/knowledge/code/internal-platform-microsoft.md` in place of the label.
  Path labels tied to a base commit expire when the base moves; prefer a quote
  from a file that exists.
- **P10 overlapped P14 on the opaque-config surface — false positive closed.**
  `design-contract-looser-than-runtime` fires on a tightened runtime rejection
  with no matching `design/` constraint. But a rule inside
  `CreateCampaigns.config` *cannot* have one — the field is typed `Any` — and
  the entry's "not a finding when" excused only *dynamic* rejections, so a
  static platform rule such as a budget minimum matched on a technicality and
  double-reported against `api-catalog-platform-config-drift`, which actually
  owns that surface. P10 now excludes it explicitly and points at P14. Two
  patterns whose detect conditions can both fire on one change need the boundary
  written into at least one of them.
- **P7 fix count (`Seven` → `Eight`) — off-by-one against its own list.**
  `knowledge-bundle-upkeep.md` said "Seven verified fixes" while enumerating
  eight PR/SHA pairs (#17, #19, #20, #21, #22, #29, #30, #39). The enumeration
  was right and the total was wrong. Same failure family as the P8 count below:
  a prose total drifting from the list beside it. **When an entry states a count
  and then enumerates the items, the enumeration is the source of truth** —
  re-count it rather than trusting the sentence.
- **The `3,850-comment corpus` was never defined.** The figure appeared in the
  P8 correction with no definition anywhere and looked inconsistent with the
  1,603 Copilot inline comments in *Evidence base*, which made the probe
  unreproducible. Both numbers are correct and measure different things; the
  definition is now stated inline. A count is not evidence until the population
  it was drawn from is named.
- **The false-positive floor is applied as of the base commit.** Reading the floor
  from the reviewed change's own result would let that change suppress findings
  about itself — silently, and without ever reaching the human gate this README
  puts on floor changes.

  **Superseded 2026-07-31 — the mechanism changed, the property did not.** The two
  paragraphs below record how this was achieved under the earlier
  snapshot-and-patch design, and are kept because the reasoning is what justifies
  the rule. **They no longer describe how the reviewer works.** The reviewer now
  reads `known-false-positives.md` **directly from `base_sha`**, the pinned
  pre-change base, with `git show <base_sha>:docs/reviews/knowledge-base/known-false-positives.md`.
  Nothing is inferred from the diff and no baseline is reconstructed. The
  consequences are the same and now fall out of the ref itself: a waiver **added**
  in the reviewed range cannot suppress anything, and one **removed** in the range
  still applies. A base tree with no floor path — and a root target with no base at
  all — is a deterministically **empty** floor and a *complete* review, never
  `INCOMPLETE`. Incompleteness is reserved for a floor that is present at the base
  but cannot be read honestly: wrong object type, unreadable content, or an
  absence you cannot distinguish from a read error. The reviewer never falls
  forward to the target floor.

  *Historical — the snapshot-era mechanism:* the learnings reviewer read this
  directory from the *post-patch* snapshot, so the skill had to ignore floor
  additions and widenings introduced by the patch under review.

  *Historical — the bootstrap refinement (2026-07-30).* The first version of that
  rule left the "file is new in this patch" case implicit, which risked the
  opposite failure: a reviewer treating the absent baseline as unreadable and
  returning `INCOMPLETE`. It is not unreadable — a floor file new in the patch has
  a deterministically empty pre-patch baseline. That reasoning survives intact; only
  its implementation, which reconstructed the baseline from the floor file's hunks,
  has been replaced by the direct base read above.
- **Bare-basename anchors — expanded, not rewritten.** Two prose shorthands were
  expanded to full repo-relative paths (`internal/apivalidation/doc.go`,
  `charts/lfx-v2-campaign-service/parity_test.go`). Basenames appearing *inside a
  quotation* were left exactly as the reviewer wrote them, with the current
  anchor added alongside — see the `internal-service.md` note in
  [knowledge-bundle-upkeep.md](knowledge-bundle-upkeep.md). Never edit a path
  inside quoted text to make it resolve.

## Maintenance

Add an entry only with its full provenance chain intact: thread URL, readable
fixing hunk on a merged PR, present-day confirmation, and a detect condition. Add
a floor entry only with the maintainer's rebuttal and the current proof intact.
When an entry stops matching the code, say so in the entry rather than deleting
its history. Removing a pattern or a floor entry changes what the local reviewer
blocks on, so it is a human-gated decision.
