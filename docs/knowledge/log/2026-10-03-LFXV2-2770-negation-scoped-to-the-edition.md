# 2026-10-03 — LFXV2-2770 check 4: my own fix widened the audit's scope

**Fix** — Review response on PR #240. The negated-registration-naming fix landed one commit
earlier asked its question in the wrong PLACE, and Copilot caught it on the next pass.

**What went wrong.** `namesRegistrants` was consulted at the TOP of the loop, before the year,
region and event-token gates. So any negated registration name downgraded the whole audit,
including names check 4 is not scoped to at all: `PyTorch 2025 - Unregistered Prospects`
turned an `AGNTCon + MCPCon North America 2026` audit into NEEDS VERIFY. Verified before
fixing — all four out-of-scope shapes returned NEEDS VERIFY.

**Why the ordering is the whole answer.** Check 4 asks one question about ONE edition. The
negation question only *means* anything for a name already established as this edition's —
for anything else it is noise, and noise that lowers a verdict trains operators to ignore
the verdict. Moving the decision below all four gates makes the scope do the work; no new
predicate was needed.

**The shape worth remembering.** The previous commit fixed a false CRITICAL by adding an
undecidable path, and in doing so created a false NEEDS VERIFY. Both are the same error —
a verdict asserted about a list the check has no business judging — and the second was
introduced by the fix for the first. A new verdict path has to be placed with the same care
as the predicate that reaches it: *where* a question is asked is part of the answer.

**Pinned.** `TestCheckCurrentRegistrants_NegatedNamesAreScopedToThisEdition` covers every
gate — an unrelated event, an unrelated event in the same year, a past edition of this
series, a sibling region — plus this edition's negated name (NEEDS VERIFY) and this
edition's real registration list (FAIL, so the reordering cannot weaken the finding the
check exists for). The mutation that restores the top-of-loop position fails all four
out-of-scope cases and leaves the two in-scope ones passing.

**Also in this round.** The Goa `Description` for `run-audience-qa` still advertised three
checks while the endpoint returned four, so regenerated API docs under-described the
contract. Updated in `design/` and propagated with `make apigen` (never hand-edited), which
also refreshed `cmd/campaign-service/kodata/`. And the `event_name` comment beside it still
said "Absent, check 4 reports NEEDS VERIFY" — the behaviour `c2cf7d5e` deliberately changed
to OMITTING the field. A stale comment describing the contradiction a previous review
already rejected is worse than no comment; it now records why omitting is the right answer.

**A third finding, answered with scope rather than code.** Copilot then noted that check 4
treats including current registrants as universally critical while `Post-Event` is a real
stage (`internal/service/emailstage/stage.go`) whose intended audience IS this edition's
attendees. Verified — the stage exists, and the finding is right that the check would fail a
legitimate send there.

It is not a bug *today* only because check 4 is **dormant**: `event_name` is optional and no
client sends it, so the check is omitted from every audit in production. The gate belongs in
the same change that wires `event_name` through from the UI, where the stage is known;
adding a stage attribute to the contract now would be fitting a shape to no caller. Recorded
in the godoc next to the code and filed as #244, so the constraint cannot be lost between
this PR and that one.
