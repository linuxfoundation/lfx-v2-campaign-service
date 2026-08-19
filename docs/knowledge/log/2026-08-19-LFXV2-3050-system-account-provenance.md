# 2026-08-19 — LFXV2-3050 record which campaigns ran on the LF system account

**Update** — Gap 2 of LFXV2-3050 (gap 1 shipped in PR #150). When a project has no
connection of its own, dispatch falls back to the LF-owned system account. `resolve` already
stamped `fromSystem` on the credential it returned (`internal/dispatch/creds.go`), used it to
attribute errors to the right owner, and then DISCARDED it. The campaign row records the
project, not the credential that served it, so after the fact nothing answered "which
campaigns ran on the LF account" — which is what attributing system-account spend back to
projects needs, and what computing blast radius on a revoked credential needs.

Migration `000027` adds `campaigns.ran_on_system_account BOOLEAN`, nullable, **no default**.

**Pre-existing rows stay NULL, deliberately.** The column carries three states: `NULL`
unknown (row predates the column), `false` known to have run on the project's own account,
`true` known to have run on the LF account. Backfilling `false` — or adding `DEFAULT FALSE` —
would be cheaper to query and would fabricate a fact: it asserts of every historical campaign
that the project paid, when some took the fallback and nothing recorded which. Because the
column exists to attribute SPEND, a false `false` moves LF-funded campaigns out of the LF
column and understates what the foundation paid, unrecoverably. The absent default carries
the same rule forward: with one, a future write that forgets the flag would claim "the
project's own account" by omission. This is the `absence-cannot-carry-new-meaning` case —
absence here already means "unknown", so it must not be recruited to mean "the project paid".

**The value is a historical fact and is never recomputed.** `upsertCampaignQuery` omits the
column from its `DO UPDATE` arm, exactly as it omits `created_by`. Re-deriving it from
whether the project has its own connection TODAY would let a project that connects its own
account next month rewrite who paid for a campaign the LF funded last month. The omission is
bare and **not** a `COALESCE`: unlike `updated_by`, a row holding `false` must not be
upgradable to `true` by a later write carrying one.

**The boundary.** `fromSystem` lives in `internal/dispatch`, the persist happens in
`internal/service`, and those packages are SIBLINGS — `go list -deps` confirms neither
imports the other (the `internal/dispatch` mentions in `internal/service` are all comments).
No new dependency was added and none was needed: the value rides the `*model.Campaign` that
`PlatformDispatcher.Dispatch` already returns, through the `internal/domain/model` both
packages already import. `internal/service` stays free of `internal/platform/*`. The
orchestrator required NO change — its single `d.Dispatch` call site already carries the
struct to `UpsertCampaign`, and its contribution is purely negative (leave the field alone),
which is what `TestOrchestrator_PersistsDispatcherProvenance` pins.

Each dispatcher applies `resolved.stampProvenance` through a `defer` on a NAMED RETURN rather
than at each `return campaignFromX(...)`. The seven dispatchers have two or three
campaign-returning exits each, several returning a campaign ALONGSIDE an error (UNCONFIRMED /
degraded) — the rows reconciliation most needs. Per-site stamping would be seventeen edits an
eighth path silently omits, and that omission would look identical to a campaign that really
ran on the project's own account. `resolveRedditClientWithCreds` /
`resolveHubSpotClientWithCreds` were split out because those two adapters resolve behind a
helper returning only the client; read-only callers keep the narrower signature.

Not exposed in `design/`, so `gen/` and the OpenAPI documents are untouched and `make apigen`
was not run. It is operator-facing provenance for spend attribution and credential blast
radius — the same class as `created_by`, which is likewise persisted and likewise absent from
the API. Adding it to a caller-facing payload would answer a question no API caller asked and
would commit us to a wire contract for a column whose consumers are reporting queries.

**Verification** — five mutations, each COMPILING and each reverted:

- Discarding `fromSystem` in `stampProvenance` (`_ = r.fromSystem`) fails all three
  `TestStampProvenance_*` tests, including the aliasing one.
- `ran_on_system_account=EXCLUDED.ran_on_system_account` in the `DO UPDATE` arm fails
  `TestLiveUpsertDoesNotRecomputeProvenanceOnUpdate` on BOTH arms (rewritten to false;
  erased to NULL).
- The "safe-looking" `COALESCE(EXCLUDED..., campaigns....)` still fails that test's
  false case — which is why the omission is bare.
- `NOT NULL DEFAULT FALSE` on the migration fails the column-shape test on both
  nullability and default, and fails two upserts with SQLSTATE 23502.
- Dropping the `$17` binding (`(*bool)(nil)`) fails the persistence test on the stored
  value for both known states.

The live tests RAN (not skipped) against PostgreSQL 16.10 via `TEST_DATABASE_URL`; the
in-memory suite alone would not have caught the `DEFAULT FALSE` or `COALESCE` mutations.

**What the local review trio added, and it was the important part.** Two reviewers
independently found the same hole by mutation: the seven
`defer func() { res.stampProvenance(camp) }()` calls — the ONLY thing moving provenance from
the credential onto the campaign — had no test. Deleting one left the entire suite green, and
for five of the seven it still COMPILED, because those `Dispatch` bodies use `res` elsewhere.
(Reddit and hubspot use `res` for nothing else, so the compiler catches those two — stricter,
but not a property the other five have, and not one to rely on.) The three original test files each covered a piece in
isolation (`stampProvenance` called directly; the orchestrator handed a campaign that already
carried the flag) and nothing ran a real `Dispatch` and looked at the field. The change's own
argument — that a defer beats seventeen per-return edits because a future exit cannot be
missed — was the one claim nothing held in place, and deleting a defer was every bit as
silent as forgetting a return site would have been.

`TestAllDispatchers_StampProvenanceOnEveryCampaignReturn` closes it with one case per
dispatcher over both credential scopes, and
`TestReddit_DispatchStampsProvenanceEndToEnd` adds the error-carrying exits (the UNCONFIRMED
create, which returns a campaign ALONGSIDE an error). All seven mutations now COMPILE and
FAIL, each naming its own dispatcher.

**The mutation had to be chosen carefully, and the first choice was wrong.** Deleting the
defer line outright does NOT compile for reddit or hubspot — `res` is used by nothing else in
those two `Dispatch` bodies, so the compiler rejects it. A build break is not evidence that a
test covers anything, so recording those two as "killed by mutation" would have overstated
the proof. The honest analogue of a real regression keeps `res` used and the defer present
while dropping only the effect:

    defer func() { _ = res }()   // compiles everywhere; provenance never stamped

Under that mutation all seven COMPILE and all seven are killed by real TEST failures, each
naming its dispatcher. That is the form the evidence rests on.

Each row also DECLARES which exit it drives (`wantErr`) and asserts it. Eleven reach a clean
create; the two linkedin rows reach the UNCONFIRMED arm, because the fake answers the
dark-post step with a plain id where the client requires a `urn:li:share/ugcPost` URN. That is
kept rather than "fixed": the error-carrying exit is the one per-return stamping would most
plausibly miss. Asserting it stops the table from silently degrading into all-partials, which
would still stamp and so would slip past the provenance assertion alone.

The first version of that table was itself the bug it was written to prevent: pointing all
seven at one 5xx server made nine of fourteen cases hit `t.Skip` (most adapters return
`(nil, err)` rather than a partial when a create fails outright), so it reported PASS while
asserting almost nothing. The skip is now a FAILURE naming the dispatcher, and every case
drives a real successful create against that adapter's own fake API. Getting there surfaced
four real contract details a skipping test would have hidden: LinkedIn's config key is
`linkedInConfig` (capital I) so the wrong key silently decoded to an empty config, its
by-name search requires a `metadata` block, Meta needs `currencyOffset` to skip FX
derivation, and **HubSpot never reaches the system fallback at all** — `systemConn` refuses
it for non-paid-ads providers, because HubSpot is a CRM portal and falling back would write
one project's contacts into the LF's own portal. The table encodes that rule rather than
asserting a behaviour the service deliberately does not have.

Three smaller review findings landed with it: `hubspot.go`'s partial-return site assigned a
LOCAL `camp :=` rather than the named return (correct today, fragile to any later edit);
`stampProvenance`'s overwrite-vs-preserve asymmetry is now documented as intended ("I know"
always wins, "I do not know" never overwrites); and the `adoptCampaignQuery` comment was
narrowed, because the two adoption paths genuinely differ — `AdoptCampaign` records NULL
while google-ads' in-dispatch adoption stamps, since one resolved a credential and the other
was handed an id by a caller.

Three existing fixtures in `campaign_repo_test.go` asserted the old 20-column shape and were
updated — `TestScanCampaign_MapsEachColumnToItsField` (now also proves the new column maps to
its field), `TestScanCampaign_NullActorsDecodeToNil` (now pins that NULL does not decode to
`false`), and `TestScanCampaign_MalformedActorJSONIsAnError`. They failed loudly rather than
silently, which is the drift guard `campaignColumnOrder` exists to provide working as
intended.
