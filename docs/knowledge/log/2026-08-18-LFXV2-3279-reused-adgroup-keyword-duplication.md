# 2026-08-18 — LFXV2-3279 a reused ad group had every keyword posted a second time

**Fix** — `createAdGroupAndAd` consulted `existed` at the ad-group step, emitting "Ad group
already exists by name: … (not re-created, bid unchanged)", and then never consulted it again.
The keyword step fired unconditionally. So a caller-level retry — which is the DESIGNED path,
since `NameSuffix` = the brief id composes the same campaign and ad-group names so the lookups
reuse the existing tree — re-posted the whole keyword batch onto an ad group that already had
it.

That duplicates spend, which is what makes it worth a fix rather than a note. Two copies of a
keyword are two criteria bidding on the same term, so the reused campaign pays twice for the
traffic the operator approved once. And the output actively concealed it: the steps said the
ad group was "not re-created" on the same run that silently doubled its keywords.

**There is no reconciliation available, and that constraint picked the fix.** All five
non-test files in `internal/platform/microsoft/` were enumerated: there is no keyword READ
anywhere — no `GetKeywordsByAdGroupId`, no list, no reconcile helper. v13's `AddKeywords` has
no idempotency key either (already stated in `createKeywords`' own doc comment, which is why a
429 is not retried there). So on the reuse path the client cannot know which keywords already
exist, and any "add the missing ones" reconciliation would be invented rather than implemented.

The keyword step is therefore SKIPPED when the ad group already existed, and the steps say so
in full — that it was skipped, that re-posting would duplicate and double the bid, and that a
keyword added to the brief since the first run must be attached by hand. This is the same rule
`findOrCreateAdGroup` already applied one step earlier, where a reused group keeps its existing
`CpcBid` rather than being re-bid by a create-only retry; keywords are that group's other
spend-bearing attribute, so the consistent choice was to leave them alone too.

**The cost is stated rather than hidden**: a genuinely-new keyword goes unattached. That is the
lesser harm by a wide margin — a missing keyword under-serves and is named in the steps, while
a duplicated keyword silently doubles the bid on a term nobody re-approved. Under-serving is
recoverable and visible; over-spending is neither.

**Knock-on, recorded because it is a real behaviour change and not obviously benign.** The
dispatcher's ACTIVATE guard refuses a campaign whose persisted `keywordIds` are empty, and on
the skip path they ARE empty — so activating a reconciled reuse now returns the 409
"keyword targeting is not yet provisioned" even though the keywords exist upstream. That is
kept deliberately: the ids needed to enable those Paused keywords are exactly what this run
could not learn, so an activate would report success while leaving every keyword Paused.
Refusing is the honest answer; LFXV2-2665's reconciliation is what resolves it properly. The
`KeywordIDs` doc comment on `CampaignResult` was enumerating the reasons the field can be
empty and has been corrected — it had two, and there are now three.

**Mutation-tested, and two mutations survived the first pass**, both on `AlreadyExisted`
rather than on the skip itself:

- Hardcoding `AlreadyExisted = false` on the new branch changed no test. The contract is "this
  run created NOTHING", and a wholly pre-existing tree whose keyword step posts nothing
  satisfies it — but nothing pinned that. `…ReusedTreeWithKeywordsIsStillAnUntouchedTree` now
  does.
- Dropping the `adExisted` conjunct also changed no test, and the reason was worse: the test
  meant to cover it omitted the campaign lookup, so the campaign level alone forced the result
  false and the assertion held regardless of what the ad contributed. It was passing for the
  wrong reason. The fixture now pre-provides the campaign AND the ad group, isolating the ad
  as the only entity created.
