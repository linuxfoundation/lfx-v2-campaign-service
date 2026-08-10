# 2026-08-08 — A validity check that ran on only one of the three paths that needed it

**Update** — `login_customer_id`'s stored-shape check moved out of
`resolveGoogleAdsDiscoveryClient` and into `validatedLoginCustomerID`, which every path that
reads that column now calls. A malformed manager id on the toggle and metrics paths used to
answer `503`; it now answers `409`, and campaign create no longer leaks the raw id into the
dispatch-failure log.

## The defect

`internal/dispatch/googleads.go` has THREE readers of the same connection column:

| reader | serves |
|---|---|
| `resolveGoogleAdsClient` | status toggle, metrics reads |
| `resolveGoogleAdsDiscoveryClient` | the account-discovery endpoint |
| `Dispatch` | campaign create — builds its own client inline, rather than through a resolver |

All three read `providerConfig["login_customer_id"]` and hand it to the same
`googleads.NewClient`. Only the discovery one inspected it first.

The third is the one this fix's own first round missed, and the miss is instructive: the two
RESOLVERS are what a reader of this file sees as "the callers", so wiring the check into both
of them looks complete. `Dispatch` does not go through either. It was found by asking which
call sites read the COLUMN, not which call sites use a resolver — and there was no test on the
create path to fail, which is why a review caught it rather than CI.

So the same stored value, with the same defect, produced two different answers depending on
which endpoint the caller reached:

- **Discovery** — caught at the dispatch boundary, tagged
  `ErrConnectionNotUsable` + `ErrProviderConfigInvalid`, mapped to **400**
  (`internal/service/connection.go`, the `ErrConnectionNotUsable` arm). Correct: a stored id
  with dashes in it needs a human to edit the connection, and discovery is the endpoint that
  edit is made from — the caller is looking at the connection they must fix, so the fault is
  in the request they just sent. The campaign-scoped paths answer **409** for the same defect
  because there the connection is a precondition of the resource being addressed rather than
  the thing being addressed; both say "non-retryable, a human must edit the connection", which
  is the property that matters, and neither is a 503.
- **Toggle and metrics** — passed through uninspected. They failed later, inside the client,
  at `validateLoginCustomerID` — by which point the error is indistinguishable at the
  orchestrator's boundary from a genuine upstream failure. It fell to the service layer's
  default arm: **503, "the provider call failed, retry later"**, for a value that no amount
  of retrying will repair.
- **Create** — passed through uninspected too, but what that COST is different, and saying
  "create answered 503" would be wrong. Create is queued work rather than a request: every
  dispatcher error becomes the same `platform campaign creation failed` job result, so there
  is no status code to get right. What the missing classification cost is claim semantics —
  an unclassified failure is not `notCreated`, so the orchestrator RETAINS the claim for
  reconciliation of a campaign that was never attempted, wedging the (brief, platform) slot
  against the re-dispatch that repairing the connection is supposed to enable.

Create carried a second defect the read-only paths did not. The client's own validator renders
the offending value with `%q`, and a create failure is logged by the orchestrator on both the
released- and retained-claim arms — so the raw manager id, which is account-identifying
configuration, reached application logs. Everything else on this path deliberately keeps error
text to a fixed sentinel vocabulary with no payload attached, precisely so it can be logged;
create was the one hole in that. `TestGoogleAds_Dispatch_MalformedManagerIDIsClassified`
asserts the absence of the value as well as the presence of the sentinels, and revert-checking
it reproduces both regressions at once.

## Why the check has to be where the value is READ

The client's own `validateLoginCustomerID` still exists and still runs. It is not a
duplicate of this check and it cannot replace it: it fires inside the same call that talks to
Google, so by then the information needed to tell a bad stored row from a bad upstream
response is gone. Classifiability is a property of WHERE a check runs, not of whether one
runs at all.

The two regexps (`storedCustomerIDRE` here, `customerIDRE` in the client) must stay in step —
widen one and you must widen the other.

## The shape of the mistake, which is the reusable part

Nothing about the original inline check was wrong. It was correct, well-commented, and had a
test. It was in the wrong PLACE — inside one caller of a shared resource, rather than beside
the read of that resource. That is invisible to every gate: it builds, it lints, its test
passes, and the endpoint it does cover behaves perfectly.

The tell is structural, not behavioural: **several functions reading the same stored field,
and only some of them validating it.** Worth grepping for whenever a helper acquires a second
caller — the second caller inherits the reads but not the guards.

The follow-on round sharpened it. Enumerate readers by the FIELD, not by the abstraction:
`grep -n 'providerConfig\["login_customer_id"\]'` finds `Dispatch`; "which resolvers call
this?" does not, because `Dispatch` is not a resolver. An abstraction that most callers share
makes the one caller that bypasses it harder to see, not easier — and that caller is
disproportionately likely to be the oldest and most important one, since it predates the
abstraction.

## Note

The 503 was correctly described as an open gap in `internal/service/orchestrator.go` when
LFXV2-2023 shipped, rather than papered over; that comment now records the gap as closed.
Writing down "this is wrong and here is why" at the point you decline to fix it is what made
this a ticket instead of a surprise.

## "Preflight" named two groups the paragraph had just separated

The `toggleCampaignStatus` comment opens by splitting everything before the platform call into
two groups — the dispatcher's **cred resolution** and its **connection-state checks** — and then
says the classification of "that last group" is no longer uniform. The very next sentence read
"Google Ads tags every one of its **preflight** failures with `domain.ErrConnectionNotUsable`".

That is false of cred resolution, on Google Ads as everywhere else.
`credsSource.resolve` (`internal/dispatch/creds.go`) has three returns carrying no
`ErrConnectionNotUsable` at all: a missing connection row keeps `domain.ErrNotFound` (404), a
repository failure keeps only the wrapped repo error (retry later), and a GCM authentication
failure carries `domain.ErrCredentialDecryptionFailed` (page ops — a wrong or rotated
application key is not something a project admin can fix by editing a connection). Only the
provably-bad-row returns — no stored credentials, `ErrCredentialsMalformed` — are tagged.

So a reader taking the paragraph at its word would flatten three genuinely different answers
into "edit your connection", which is exactly the distinction `resolve`'s own comments spend
thirty lines protecting.

The tell is that the sentence used a **different word** for the group it had just named.
`preflight` is the informal umbrella for both halves; `connection-state checks` is the term the
paragraph defined two sentences earlier. Swapping to the loose synonym silently re-scoped the
claim over a group the writer had explicitly excluded. **When a paragraph defines a partition,
every later sentence must keep using the partition's names** — a synonym is how a claim about
one half becomes a claim about the whole.

## The documented review technique did not run, and would have lied if it had

The `internal-dispatch.md` note recommending how to find all three readers offered
`grep providerConfig["login_customer_id"]`. Two defects, and the second is the interesting one.

It does not execute: unquoted `[...]` is a shell glob (zsh refuses outright with "no matches
found"), and basic grep reads `[login_customer_id]` as a character class rather than literal
brackets. Fixed by quoting and `-F`.

But the corrected command is still the wrong enumeration, and now returns exactly ONE hit —
inside `validatedLoginCustomerID`, because centralising that expression is precisely what this
PR did. A technique keyed on an expression a refactor was designed to collapse reports one
reader and reads as reassurance: the reviewer runs it, sees a single site, and concludes there
is nothing to check.

So the doc now gives two commands that survive the refactor — the stored KEY
(`login_customer_id`, every reader including comments and tests) and the helper
(`validatedLoginCustomerID`, three call sites) — and says explicitly why the expression-keyed
form is no longer the one to use.

Reusable: **a documented search command is executable documentation and rots the same way code
does.** It has two failure modes, not one — it can fail to run, which is loud, or it can run and
return a confidently wrong answer, which is not. Re-run any grep a doc recommends after the
refactor that doc describes.

## Correction to the round above: those sentinels are not this endpoint's answers

The round above is right that `credsSource.resolve` returns three errors carrying no
`ErrConnectionNotUsable`, and wrong about what happens to them. It annotated them "(404)",
"(retry later)" and "(page ops)" as though those were the toggle endpoint's responses. They are
not. `ToggleCampaignStatus`'s switch (`internal/service/brief.go`) has several typed arms, and
**none of them matches any of these three** — no `ErrNotFound` arm, no arm for a bare repository
error, no `ErrCredentialDecryptionFailed` arm. All three untagged returns fall to `default`:
logged, then 503, indistinguishable from a repository failure. (An earlier draft of this
paragraph said the switch had "exactly two arms", which is false and was itself an instance of
the mistake being described — asserting more about the switch than had been checked. The load-
bearing fact is the absence of the three arms, not the arm count.) The distinct
classifications are honoured by the read-only discovery handlers, which is where they were
introduced.

The mistake is a specific and repeatable one, and it is the mirror image of the defect the round
was correcting. That round widened a claim by swapping in a loose synonym; this one widened a
claim by reading a sentinel's **intended meaning** as its **realised behaviour**. A sentinel is a
proposal — it says what a caller *could* distinguish. Only the caller's switch says what any
caller *does*. Writing "`ErrNotFound` (404)" in a comment above a switch that has no
`ErrNotFound` arm documents an endpoint that does not exist, and it is worse than saying nothing,
because the next person adding a caller will assume the mapping is already there.

Rule: **when a comment attaches a status code to a sentinel, the switch that produces that status
must be in view.** If you cannot point at the arm, write down what actually happens and what the
gap is.

The gap itself is real and now recorded in the comment: "no connection configured for this
project" is permanent, and answering 503 invites a retry that cannot succeed. Fixing it means
adding arms to the toggle handler — a behaviour change, tracked as LFXV2-3065, deliberately not
smuggled into a classification-documentation PR.
