# 2026-09-21 — wizard: a fix that truncated, and two cross-turn portal leaks

**Fix** — three defects from the review round after
[the hardening entry](2026-09-21-email-wizard-review-hardening.md), two of them introduced by
that entry's own fixes. The pattern is the part worth recording.

## The property I named was not the property that mattered

The previous round closed a leak: a self-closing `<script/>` let its payload out as escaped text.
The fix bumped `skipDepth` for all five `dropContent` tags on a self-close, reasoning that *none
of these is a void element*.

That is true and irrelevant. The property that governs the tokenizer is **raw text**, and only
three of the five have it:

```
script  [SelfClosingTag  Text(TAIL<p>x</p>)]                 <- tail consumed whole
object  [SelfClosingTag  Text(TAIL) StartTag(<p>) Text(x)]   <- resumes normally
```

For `object` and `embed` no `</object>` ever arrives, `skipDepth` never returns to 0, and every
later token is suppressed — `<p>before</p><object/><p>after</p>` silently lost `<p>after</p>`.
**A truncation is worse than the leak it replaced, because the leak was visible.** The bump is
now scoped to the raw-text subset, verified against the tokenizer rather than inferred.

## A guard on one door, and a false comment on two others

`findWizardSourceEmail` searched the HubSpot portal with no ownership filter — portal-wide, while
`projectID` only chose the connection, so on the shared LF row a hit can be another tenant's sent
email. `EmailReferenceSource.Get` already refused for exactly this reason; the wizard reached the
same cross-tenant read through a different door.

Closing it widened `HubSpotClientResolver` to report `fromSystem`. The two other call sites were
given `_` and a comment saying *"this path WRITES the requesting project's own content."* That
comment was **false**, and the review caught it:

- `wizardReferenceEmails` calls `GetEmail` and `GetEmailHTMLWidgets`, both documented read-only.
- `CloneWizardEmail` calls `CloneEmail`, which READS `sourceID`'s content before writing a draft.

Both use a `src.ID` captured in an **earlier turn**. The resolver is deliberately per-call because
a connection can be revoked or rotated between turns — so a later turn can resolve to the shared
portal and read a numeric id recorded against a different one. The reference path now degrades to
generating without a voice reference; the clone path answers **409**, because it would otherwise
materialise the leak as a durable draft attributed to the requesting project.

The lesson is not "check the other call sites" but that **a comment asserting why a guard is
unnecessary is itself a claim, and this one was refuted by the function it sat above**.

## A test that proved storage, not usage

`url` at plan-start was accepted and never read — the one optional field of four not persisted.
The first tests asserted it round-tripped through the plan blob, which `resolveWizardDetails`
never consults when the brief already carries an event name (as the shared harness always seeds).
They would have passed against the original bug. A recording fetcher over an empty-details brief
is what actually pins the read path, and mutation-neutering the fallback now fails it with the
brief's own URL in the diagnostic.
