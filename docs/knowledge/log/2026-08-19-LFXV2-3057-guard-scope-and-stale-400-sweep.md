# 2026-08-19 — LFXV2-3057 auth-error guard scope, and the stale 400 sweep

**Fix** — three review findings from the local reviewer trio and the human review, all
verified before acting.

## The 500-guard covered connections only

`assertEveryConnectionEncoderHandles` parsed a single file, `connectionsErrorEncoders`. The
case-presence guarantee — that a method omitting `Error("Unauthorized", ...)` from its design
block encodes a refused token as a **500** rather than a 401 — was therefore enforced for the
connections service alone. Briefs and audiences declare the same errors through
`commonBriefErrors()` (`design/brief.go`), a helper maintained separately from connections'
`authErrors()` (`design/connection.go`), and their ~22 methods had no equivalent guard.

The sibling challenge test does not close this gap, which is the part worth recording, and it
is worth being precise about WHY because a careless mutation gives the wrong answer. It
compares `count(setChallenge)` against `count("case \"Unauthorized\":")` per file. When Goa
regenerates after a method loses its `Error("Unauthorized", ...)` declaration, it emits
neither the `case` nor the `w.Header().Set(...)` inside it, so **both** counts decrement and
the equality still holds — the omission is invisible there.

Verified by mutation, in the shape Goa would actually produce (removing the whole case block,
header write included) rather than by renaming the case label. Renaming only the label leaves
the header write behind, makes the counts diverge, and misleadingly fails the challenge test —
an artefact of the mutation, not a property of the guard. Under the faithful mutation the
challenge test reports `ok` while the generalised guard fails, naming
`EncodeCreateBriefError`. The two tests check genuinely different properties: one that the arm
exists, the other that an existing arm sets the header.

`TestEverySecuredMethodEncodesAuthErrors` now iterates `authErrorEncoders` — all three
generated files — for both `"BadRequest"` and `"Unauthorized"`. Current coverage is exact and
was already correct on the wire (connections 47/47, briefs 17/17, audiences 5/5), so this
pins a property that holds today against a future method added without the declaration; it
repairs no live defect.

The test was renamed from `TestEveryConnectionMethodEncodesBadRequest`, which had become
doubly inaccurate: it asserts two error names, not just `BadRequest`, and now spans three
services rather than connections. A failure log naming `...EncodesBadRequest` for an
`Unauthorized` regression costs debugging time. `docs/knowledge/code/design.md` cites the new
name.

## Stale 400-era prose, swept

This change moved only **token-side refusals** from 400 to 401. The `unavailable` branch —
no verifier wired, or `domain.ErrKeyUnavailable` from a JWKS fetch — has always answered
**503** and is untouched. Prose claiming "both were 400" therefore asserted that the 503 case
used to be a 400, contradicting the 503 section a few paragraphs below it in the same files.

Fixed in `internal/service/auth.go`, `docs/knowledge/code/internal-service.md`, and three
sites in `internal/service/auth_test.go`. The last three are worth noting because they are
the class the previous round missed: the earlier fix commit corrected the concept file's
line 716 and the log, but the same false claim survived in the
test file's godoc and — sharpest — in a `t.Errorf` **failure message**, which told a developer
that token-side refusals "must map to 400" while the contract this PR establishes is 401.
The assertion itself was always correct; it checks the `unavailable` bool, not a status.

These lines are pre-existing text this change *falsified* rather than lines it added, which
is precisely when documentation drift becomes this PR's to fix.

`internal/infrastructure/auth/jwt.go:64` was initially left alone on the reasoning that the
package is untouched by this branch, so its 400-era prose was pre-existing drift. That was
wrong, and the distinction is worth stating: the line says the service layer maps "every
token-side refusal" to 400 in the PRESENT TENSE, describing the mapping this branch
CHANGES. This change falsified it, so it is this PR's to fix, and it now says 401.

Whether the file is edited by the branch is the wrong test — what matters is whether the
claim was true before the change and false after. Three neighbouring 400 mentions in the
same file (`:245`, `:257`, `:613`) are NOT in that class and stay as they are: each narrates
the counterfactual a guard prevents ("a JWKS misconfiguration would have come back as a 400
to a caller holding a good token"), describing the era before `ErrKeyUnavailable` split the
two branches. They are historical framing of a path that no longer reaches the token side at
all, not present-tense claims about today's mapping.

## The log fragment was missing its kind marker

`2026-08-19-LFXV2-3057-challenge-coverage-and-jwks-wording.md` went straight from its H1 to
plain prose. `AGENTS.md` requires "a first H1 dated to match the filename, then a bold kind
marker and an em dash". `okfvalidate` checks the filename, the dated H1 and frontmatter only,
so it does not catch this. Prefixed with `**Fix** —`.
