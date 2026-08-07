# 2026-08-07 — LFXV2-2665: the ad-set name conflict was documented clean and returned a partial

**Update** — `errLookupConflict` marks the one lookup failure that is NOT unconfirmed: the
lookup completed, enumerated the name, and read a match that cannot be adopted. The campaign
arm returned `(nil, err)` for it. The ad-set arm returned `partialResult()`, while its own
comment and `docs/knowledge/code/internal-platform-meta.md` both called it a clean failure.

**Fix** — Clean means nil, not a partial. The dispatcher releases the retained claim only
when the result is nil and treats EVERY non-nil result as UNCONFIRMED, so there is no third
shape for "definite failure, but here is some context". The partial therefore reported a
stable, re-readable conflict as "verify in Ads Manager" — and a non-PAUSED ad set does not
become PAUSED by being looked at again, so every retry re-read the same conflict and
re-retained. That forever-verify loop is the exact thing `errLookupConflict` was introduced
to break, on the arm where it was introduced.

**Fix** — Nothing is lost by dropping the partial, and the gate is what proves it: the
ad-set lookup runs only under `existingCampaignID != ""`, so the campaign was FOUND BY NAME,
not created by this call, and `adSetID`/`adCount` are still zero. The partial described a
campaign that predates this dispatch entirely. The three error strings said "campaign %s
created, PAUSED" and now say "reused" — the same gate makes "created" false on all three,
including the two ambiguous arms that legitimately keep their partial (the ad set's absence
really is open there, and a released claim would let the next dispatch POST the same
deterministic ad-set name under the same campaign).

**Fix** — `TestCreateCampaignAdSetLookupConflictIsCleanFailure` reuses the campaign arm's
`assertCleanConflict`. The campaign half had two tests and the ad-set half had none, which
is why the two arms could disagree for as long as they did. Reaching the arm requires the
campaign lookup to return an adoptable PAUSED match first, so the test also pins the gate it
depends on. Reverting to `partialResult()` fails it on the partial-result assertion.

**Note** — `docs/knowledge/code/internal-platform-meta.md` needed no change: it already
described the intended split correctly. The doc was right and the code was wrong, which is
the direction that stays invisible until someone diffs the two.
