# 2026-09-22 — wizard: every cross-turn HubSpot id now checks provenance

**Fix** — a cross-tenant mutation in `SetWizardSendList`, and the reason three rounds were needed
to find it.

## The defect

`ResolveHubSpotClient` was widened to report `fromSystem` — whether the credentials came from the
LF-wide row rather than one the project owns — to close a portal-wide search that could return
another tenant's email. Three other call sites took `_` and a comment:

> `fromSystem` is discarded deliberately: this path WRITES the requesting project's own content.

**That comment was false at all three**, and each was caught by someone else:

- `wizardReferenceEmails` — `GetEmail`/`GetEmailHTMLWidgets` are documented read-only.
- `CloneWizardEmail` — `CloneEmail` READS `sourceID`'s content before writing the draft.
- `SetWizardSendList` — caught by both review bots independently, then by the reviewer correcting
  his own approval. `sess.EmailID` is recorded by the CLONE turn, so it is a cross-turn id too —
  and this one MUTATES: it changes recipients on another tenant's draft.

The resolver is per-call precisely because a connection can be revoked or rotated between turns.
Any id that crosses a turn boundary was recorded against whatever portal resolved back then.

## The rule

**Read or write is the wrong question. The question is whether the id came from THIS turn.**

All four consumers now check provenance: the plan search and the voice-reference read skip, the
clone and the send-list answer 409. No call site discards the flag.

## What made it take three rounds

Writing a justification for a skipped guard felt like diligence and was the opposite: it turned an
unchecked assumption into something that reads as verified, so neither the author nor the first
reviewer re-derived it. A comment asserting why a guard is unnecessary is a claim, and one
`grep -n "func.*GetEmail"` would have refuted it.

A related trap in the same round: the first test for the clone guard PASSED WITH THE GUARD
REMOVED. The handler has three `ConflictError` arms and the test asserted the error TYPE, while
"no generated content" answered first. When several arms return the same type, assert which one —
and mutation-verify, which is what exposed it.
