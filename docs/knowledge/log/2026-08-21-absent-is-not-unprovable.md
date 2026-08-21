# 2026-08-21 — an absent row is a SETTLED answer, not an unprovable one

**Fix** — a fifth instance of this PR's recurring shape, plus three Reddit operator-guidance
defects that turned out to be one surface. Extends
[2026-08-20-which-fault-to-report-is-a-provenance-question](2026-08-20-which-fault-to-report-is-a-provenance-question.md);
that fragment's rule is intact and unchanged.

## `known=false` was overloaded across three unlike states

`systemCreated` answered `(created, known)` and returned `known=false` for three situations:

- the system-row lookup FAILED (a repo error);
- the system row is present but records no account of its own;
- the system row is **absent** (`ErrNotFound`).

The first two genuinely cannot settle the question, and the deliberate design — an unsettleable
question must not fabricate an answer — is right for them. The third is not like them at all.
`ErrNotFound` is a **completed read of storage**: no LF row is installed for this provider.

Collapsing them meant `resolveExisting`'s mismatch arm returned the project's resolution for a
campaign whose recorded creation account the project does not own and whose system row does not
exist. The caller got `ErrCampaignAccountMismatch` → a 409 saying "reconnect the original
account", for an account that was never theirs, caused by a missing LF credential only an
operator can install. Nobody is paged; the campaign keeps spending.

`systemCreated` now returns `(created, known, absent)`, and `systemRowProvablyAbsent` exposes
the third value. The mismatch arm returns the system error — which already carries
`ErrSystemConnectionMissing` — only on **proven** absence. A repo failure or a nameless row
still keeps the project-owned default, because a wrong guess in that direction pages an
operator for a project's own repair.

**The generalisation.** "Cannot determine" and "determined absent" are different answers. A
boolean that means "not established" silently acquires "established false" the moment any
caller needs to tell them apart — and the caller that needs it is usually the one deciding who
gets paged.

## `allow_comments:false` never meant what the code said it meant

Step 1.5's ordering rested on a stated causal claim: that `allow_comments:false` marks the
authored post "promoted-only", never distributed to a feed, so an unattached one is a
"harmless, unbilled orphan". Checked against Reddit's published docs, that is two claims and
they fail differently:

- **`allow_comments:false` makes a post promoted-only — CONTRADICTED.** Reddit documents the
  field as "whether comments are allowed on the post", on both the legacy and structured post
  endpoints, and a separate PATCH exists purely to flip it on an *existing* post. If it
  governed dark status, that PATCH would toggle a live ad between hidden and public.
- **The endpoint inherently creates promoted-only posts — NOT DOCUMENTED.** Reddit is silent
  on feed distribution and profile visibility for API-created posts; no visibility,
  publication or distribution field exists on the create body or the GET that reads one back.

Absence of documentation is not confirmation. The ordering argument does not need the claim —
it rests on the billing asymmetry (an unattached post carries no spend; a campaign does), which
is what makes post-then-campaign strictly safer than the reverse. So the comment now states
only what is known, and because visibility is **unknown rather than known-benign**, every arm
where a post may exist tells the operator to **delete** a post not attached to an ad. That
instruction is correct whether or not the post is distributed, which is precisely why it
belongs there while the question is open.

Also noted in passing: `POST /profiles/{id}/posts` is marked **legacy** by Reddit, superseded
by the structured-post job flow. Not migrated here; recorded so the next editor knows.

## Three "separate" findings were one runbook

`Steps` is read as a runbook, and the authoring-failure path had three defects that only look
independent:

1. The Step 4 `else` said *"No ad variants or post URL provided — add ads manually"*. It was
   written when its only cause was "the caller never asked for an ad"; Step 1.5 added an
   opposite way in and the branch was not updated. On the **ambiguous** arm it told the
   operator to go create the ads while `AdWarning` said the post may exist and to verify first
   — a runbook contradicting its own warning, on the double-spend path. The wording was also
   false whenever `Variants` were supplied.
2. The definite-failure arms said only "author the post and add the ad manually", dropping the
   "(campaign and ad group created, PAUSED)" context the ad-create path carries. Followed
   literally, that builds a second campaign while the first sits PAUSED and orphaned.
3. Nothing told the operator to remove a stray post (above).

Fixed together, because a runbook is judged as one document: the guard is `postWarning != ""`
at the **variant-listing** level (not just the zero-variant `else` — the listing arm was the
one actually firing), every arm names the campaign and ad group as created and PAUSED, and the
ambiguous arms say DELETE.

## An in-flight cancellation released the claim

`createPromotedPost`'s failure handler checked `ctx.Err()` **before** classifying ambiguity,
returning `(nil, err)`. The request layer deliberately wraps ctx errors as `transportError`
because a ctx error from `Do` can fire *after* the POST body reached Reddit. And
`CreateCampaign`'s contract makes `(nil, err)` mean "nothing was or may have been created" —
`RedditDispatcher.Dispatch` keys claim release on `result == nil` **alone**. So a cancellation
interrupting the in-flight POST released the claim on a post that may exist, and a retry
authored a duplicate.

The fix is a **conjunction**, not a reordering: `ctx.Err() != nil && createOutcomeAmbiguous(err)`
returns a name-carrying partial result (claim retained); a non-ambiguous cancellation still
aborts with `(nil, err)` (nothing sent, claim released); and every *non-cancellation* failure —
including a plain 5xx — stays **non-fatal** and degrades to a campaign with no ad, which is what
Step 1.5's ordering exists to provide. An earlier attempt aborted on any ambiguous error and
threw that degradation away for the most ordinary transient failure there is; the 503 test case
caught it.

## Mutation notes

- The nil-row guard added at `updateConn` **survived** its first revert: `fakeRepo.Get` never
  returned `(nil, nil)`, the shape `domain.ConnectionReader` permits. A fixture that can only
  express absence one way makes a guard against the other way read as covered.
- `validateImageURL`'s hostless arm appeared to survive — the mutation had been applied to
  `validateRegistrationURL`, which carries the **identical predicate** eight lines up. A
  duplicated predicate makes a mutation land on the wrong copy and read as behaviour-preserving.
  Always confirm which copy the revert edited.
- The conditional current-row read is invisible in every result (same connection either way);
  only a Get **counter** distinguishes it. A stale comment asserted the read was unconditional,
  sitting exactly where the missing test went.
