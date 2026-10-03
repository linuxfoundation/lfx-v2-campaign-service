# 2026-10-03 — LFXV2-2770 check 4: three holes in one regex, one root cause

**Fix** — Pre-PR reviewer simulation on #240 (local, before un-drafting). Two independent
simulated reviewers found three defects in the negated-registration predicate committed hours
earlier, all three verified by running the real `CheckCurrentRegistrants`.

**The three.** Each is the *unsafe* direction — a false CRITICAL on the list a registration-push
send is FOR:

| name | read as | why |
|---|---|---|
| `Not Yet Registered`, `Haven't Registered`, `Not Currently Registered` | plain → **CRITICAL** | the negation was required to sit IMMEDIATELY before the term; any intervening word reopened it |
| `Co-Registrants`, `xregistrants`, `bioregistration` | plain → **CRITICAL** | `\b?` is an OPTIONAL zero-width assertion, so it can match empty and a bare stem matched mid-word |
| `Ünregistered` | plain → **CRITICAL** | Go's `\b` is ASCII-only: no boundary between `Ü` and `n`, so the `un` prefix never matched |

`REGİSTRANTS` went the quiet way — read as NO mention at all, missing a real registrant list.

**One root cause.** All three come from judging the RAW string with a single pattern that encoded
three different questions at once. The fix judges TOKENS of a normalised form and asks them
separately: fold accents and case; split into PHRASES on separators that end a thought so a
negation reaches only to the end of its own phrase; within a phrase let `-` and `'` JOIN so
`Pre-registration` and `Haven't` survive as one token; treat a fused prefix as itself the
qualification. This is the [[fix-the-class-not-the-spelling]] lesson arriving for the third time
on this one function — a denylist over spellings cannot converge, and the structural question can.

**41 names enumerated**, both directions, including the seven a spelling denylist breaks and the
three classes above. Four mutations, each failing only its own cases: no accent fold (`Ünregistered`),
unanchored term (the three bare stems), no phrase split (`Not Interested - Registration List`),
no fused-prefix rule (11 cases).

**A mutation that proved nothing.** The first accent-fold mutation reported ZERO failures. The
mutated tree did not COMPILE (`declared and not used: folded`), so the test never ran — the exact
trap the reviewer-sim skill names. Rewritten to keep the call and discard its result, it fails the
one case it should. **Check the mutated tree builds before reading a mutation result.**

**Also in this round.**
- `docs/api-catalog.md` had DUPLICATE rows for `compose-master` and `qa/run`: an earlier commit
  inserted a new pair instead of editing the existing pair, and the surviving stale `qa/run` row
  still said "three checks". Worse, the duplicated `compose-master` row was the POORER of the two —
  it omitted the `brief_id` recording contract the original documents. Kept the richer row and the
  check-4 prose, deleted the two duplicates.
- The `undecidable` verdict branch had NO positive test: the only reference to its message was an
  assertion that it is absent, so deleting `undecidable = true` broke nothing. Two cases added, one
  per cause plus the both-causes roll-up that `make([]Finding, 0, 2)` exists for.
- A table case named "an unrelated event's negated list" carried a 2025 year, so it was rejected by
  the YEAR gate — the same gate as the past-edition case — and proved nothing about the gate it
  names. Now carries the current year and fails when `MatchLastSent` is removed.
- Two test docblocks described a "FULL token set" rule the shipped code does not implement; the
  code separates regions with `isOtherEdition`. Corrected to match, including one claim that was
  measurably inverted.
- A hard-coded `filters.go:274` reference replaced with the owning symbol, `MasterListFilter`.
