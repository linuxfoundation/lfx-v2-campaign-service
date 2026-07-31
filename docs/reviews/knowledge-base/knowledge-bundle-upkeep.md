# Knowledge-bundle upkeep

One pattern, with the strongest CI-blindness argument in this knowledge base.

---

## okf-bundle-not-updated-with-package-change

**Severity:** `should-fix`.

**Detect:** A patch that adds a Go package, or shifts an existing package's role,
or changes architecture or a Helm manifest, without **both** of:

1. a concept file under `docs/knowledge/**` covering it (new, with OKF
   frontmatter `type`, `title`, `description` — or an edit to the concept whose
   subject changed);
2. a dated entry appended to `docs/knowledge/log.md`.

**And, only when the patch adds a concept, renames one, or changes a concept's
indexed description**, the containing `index.md` bullet added or updated.

Both required parts in the same patch. A patch that has one of them is still a
match — the incomplete update is the recurring shape, not a rarity.

**The index arm is conditional, and demanding it unconditionally is a false
finding.** `CLAUDE.md:22-23` requires the bullet only when a concept is *added,
renamed, or its description changes*, so an in-place edit to a concept's body that
leaves its path, title and indexed description untouched owes no index change.
Raising one is churn, and it is the reviewer contradicting the rule surface it is
supposed to be enforcing.

**Why it matters:** this is invisible to CI by construction.
`.github/workflows/validate-okf.yml` is `paths:`-filtered to `docs/knowledge/**`
and the okf tooling, so a patch that adds a package and skips the bundle **never
runs the validator at all**. And the validator is shallow even when it does run:
it checks little beyond a non-empty `type` field and the `##` heading lines of
`log.md`. The bundle is what `CLAUDE.md` points every agent at as its map of the
repo, so a stale bundle silently degrades every future agent's context.

**Evidence:**

- [`r3563289512`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/19#discussion_r3563289512)
  (PR #19) cites the rule by line: "This feature adds a new Go package but does not
  update the OKF knowledge bundle. `CLAUDE.md:17-26` requires each PR to update the
  relevant concept, containing index, and …". Fixed in `e5714e4`.
- [`r3563291850`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/20#discussion_r3563291850)
  (PR #20): "This new Go package is absent from the OKF code index and change log,
  so the repository knowledge bundle no longer maps all packages. Add an
  `internal/platform/meta` conc…". Fixed in `34a312e`.
- [`r3562453491`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/11#discussion_r3562453491)
  (PR #11): "This adds a new API surface and orchestration flow, but the required
  OKF knowledge bundle was not updated: `docs/knowledge/code/design.md` and
  `internal-service.md` still…". Fixed in `e8da570`. The quote's bare
  `internal-service.md` is the reviewer's own shorthand, preserved as written;
  both concepts exist today at `docs/knowledge/code/design.md` and
  `docs/knowledge/code/internal-service.md`.
- [`r3562410724`](https://github.com/linuxfoundation/lfx-v2-campaign-service/pull/17#discussion_r3562410724)
  (PR #17) shows it applies to a bug fix too, not only a new package: "The OKF
  knowledge bundle is not updated for this runtime bug fix". Fixed in `3035f9f`.
- Eight verified fixes across merged PRs **#17** (`3035f9f`), **#19**
  (`e5714e4`), **#20** (`34a312e`), **#21** (`09f2ac1`), **#22** (`fd1cad2`),
  **#29** (`42ab758`), **#30** (`e8042d9`) and **#39** (`fdb4768`). Five of them
  landed within 5–41 minutes of the comment — developers agree with this one
  immediately.

**Status on main:** the rule is written at `CLAUDE.md:17-26`, with the same
four-step contract restated for humans in `README.md`. Note the accompanying
prohibition: `go run ./cmd/okfgen` must **not** be re-run to satisfy it, because it
regenerates the whole bundle from source and clobbers hand-edited concepts —
`CLAUDE.md:28-30`.

**The bundle is not a complete inventory.** At the evidence SHA these packages
have no concept and no index bullet: `internal/apivalidation`, `internal/domain`,
`internal/domain/model`, `internal/infrastructure/crypto`, `internal/okf`,
`internal/okfgen`, `internal/okfvalidate`, `cmd/okfgen`, `cmd/okfvalidate`. So
"there is no concept for this package" never proves the package is new or wrong,
and a pre-existing gap is not a finding against a patch that merely touches a
neighbour.

**Not a finding when:** the patch changes no package role, no architecture and no
manifest — a bug fix wholly inside one function, a test-only change, a dependency
bump. Do not demand a `log.md` entry marker other than the one the bundle actually
uses; `**Creation**` and `**Update**` are both in active use and the choice between
them is not a finding (see `known-false-positives.md`).
