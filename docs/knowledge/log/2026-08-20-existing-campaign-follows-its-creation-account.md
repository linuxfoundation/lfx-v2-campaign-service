# 2026-08-20 — an existing campaign follows its CREATION ACCOUNT, not the flag

**Fix** — supersedes the resolution rule recorded in
[2026-08-20-force-system-scoped-to-creation](2026-08-20-force-system-scoped-to-creation.md).
That fragment stays as written; the reasoning that produced it is intact and worth reading, and
the two other defects it records (the reversibility guard, the system-missing sentinel) are
unaffected. Only its `resolveExisting` rule is replaced.

## Scoping the flag to creation fixed one direction and broke the other

The earlier fix introduced `resolveExisting` so that operations on an existing campaign were
"never forced", and pointed it at `resolveWithFallback` — project scope, then system fallback. It
fixed the reported bug: a PRE-cutover campaign became pausable again with the flag on.

It also created the mirror-image bug, and the mirror is worse. Creation IS forced, so a campaign
created after the cutover lives in the SYSTEM account and records that in its `CampaignResult`.
Project-then-fallback resolution returns the PROJECT's account for it whenever the project has a
connection of its own — the ordinary case, and the case the flag exists for. The provenance guard
compares the recorded system account against the project account just resolved, and refuses:

```
toggle meta campaign status: campaign 23851234567890123 was created under meta ad account
act_999 but the project's current connection resolves to account act_111: the campaign belongs
to a different platform account than the project's current connection
```

So the cutover path could not stop spend on the campaigns it had just created. Same severity as
the bug it replaced, pointed the other way, and with the same absence of a fix-forward — worse,
because turning the flag back OFF does not help. The misrouting is not flag-conditional.

## The comment was already right; the body was the lie

The docblock over `resolveExisting` said the creation account is "read from the campaign's own
persisted result rather than recomputed from current config". The body read nothing from the
campaign — it never received one. The prose described the correct invariant while the code
implemented an approximation of it, and the approximation is what shipped.

Worth naming as a review signal: a function whose comment describes a fact it has no parameter
to obtain is not merely under-documented, it is structurally unable to do what it claims.

## The invariant is the recorded account, not "before or after the cutover"

Every adapter already reads the creating account as a historical fact —
`metaCreationAccountID` and its four siblings, each parsing its own platform's persisted blob
shape. That recorded id is the invariant. A date comparison or a flag reading would be another
approximation of the same fact, and would fail the same way the moment the flag is retired.

`resolveExisting` now takes `creationAccountID` as a parameter: resolve the project scope, and if
that is not the creating account, try the system row before giving up. It consults the flag
nowhere. That is what keeps a cutover-created campaign stoppable after the flag is turned OFF —
the row still records the system account, so resolution still follows it. A rule conditional on
flag state would strand exactly those campaigns at cleanup time.

An empty recorded id keeps ordinary project-then-system resolution: absence must not become a new
failure signal for rows written before the explicit field existed, the same "unknown, proceed"
every provenance guard already grants them.

## Threading it through the signature, and why the compiler was the point

The parameter is not a convenience. The bug existed because `resolveExisting(ctx, projectID,
provider)` *could not* consult the campaign while every one of its callers already held one — the
omission was invisible at the call site and cost nothing to write. Adding the parameter turned it
into a compile error, and the compiler then enumerated all nine call sites across five platforms.
That enumeration is the exit-path audit; doing it by grep is how a sibling arm gets missed.

Adapters whose credential helper serves both creation and existing-campaign callers keep the
`credsResolver` shape via `existingResolver(<platform>CreationAccountID(campaign))`, so the
creation path's signature does not grow an argument it has no value for.

## Two mutations that survived, and only one was a test gap

Neutering the `metaCreationAccountID` wiring killed the new Meta pause tests immediately. Doing
the same to the other FOUR platforms left the entire dispatch suite green. The Meta tests proved
the mechanism, not the wiring, and four of five adapters were unpinned — the same "guards one arm,
leaves its sibling" shape that produced the bug being fixed.

A reader-level test was not enough either: it proves each `*CreationAccountID` returns the right
value, but a reader is inert if the toggle path ignores it, and that test stayed green under the
same mutations. The pin has to be on the OBSERVABLE SEAM — which connection scope the repo is
asked for. `TestEveryPlatformResolvesTheSystemAccountForASystemCreatedCampaign` asserts on
`scopedConnReader.gets` and kills the mutation on all six adapters.

The second survivor was NOT a test gap. Inverting the empty-creation-id short-circuit in
`matchesAccount` survives the whole suite, and it should: with it removed, an unrecorded account
fails to match either candidate and resolution returns the project result anyway — the same
answer. It is an **equivalent mutant**, and the arm is a short-circuit that skips a lookup which
cannot change the outcome. That is now stated in the code comment, so the next person to run a
mutation there does not go hunting for a test that cannot exist. Distinguishing "no test covers
this" from "no test CAN cover this" is the difference between a finding and a wild goose chase.
