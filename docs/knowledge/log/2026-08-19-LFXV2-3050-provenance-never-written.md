# 2026-08-19 — LFXV2-3050 the provenance column was never written

**Fix** — `ran_on_system_account` was omitted from `upsertCampaignQuery`'s `DO UPDATE`
arm, on the reasoning that the column is a historical fact and the INSERT arm stamps it
once. The first half is right and the second is not: on a normal dispatch the INSERT arm
never runs, so the column was written on no path at all and stayed `NULL` on every
campaign the service created.

`Orchestrator.dispatchPlatform` reaches an upsert only after `ClaimCampaignDispatch`
returned `claimed=true`, and that claim's whole job is to INSERT a placeholder `pending`
row on the same `(brief_id, platform, variant)` slot:

    INSERT INTO campaigns (project_id, brief_id, job_id, platform, variant,
                           campaign_name, status, created_by, updated_by)
    VALUES ($1,$2,$3,$4,$5,'','pending',$6,$6)
    ON CONFLICT (brief_id, platform, variant) WHERE status <> 'deleted' DO NOTHING

By the time `UpsertCampaign` runs the row therefore already exists, and the upsert takes
its CONFLICT arm — every time, not occasionally. The dispatcher computed `fromSystem`,
threaded it across the `internal/dispatch` → `internal/service` boundary on the returned
`*model.Campaign`, bound it as `$17`, and Postgres discarded it. Reduced to its three
states, the column only ever held the one it exists to distinguish from an answer.

The claim query's OWN doc comment already said this, in the passage justifying why
`created_by` is stamped on the claim: "the upsert that follows takes the conflict arm".
The two comments sat 260 lines apart in the same file, each internally coherent, asserting
opposite things about which arm runs. The provenance comment is what concealed the bug —
it stated "The INSERT arm above stamps it once" as settled fact, so a reader checking the
column's semantics found a confident answer and no reason to go look.

Every existing test agreed with the broken code because they all seeded through the same
door they tested. `TestLiveUpsertDoesNotRecomputeProvenanceOnUpdate` creates its row with
`UpsertCampaign` — so the INSERT arm stamps the value, and the update it then asserts
against really is an update of a stamped row. Drop a `ClaimCampaignDispatch` in front and
the same assertions fail. The unit tests could not see it at all: the fake repositories
have no conflict arm, and `TestOrchestrator_PersistsDispatcherProvenance` correctly proves
the orchestrator hands the value to the repository, which was never where it was lost. The
end-to-end dispatch test stops at the in-memory `*model.Campaign` and never persists.

**The fix** is a write-once guard rather than restoring the column to the arm outright:

    ran_on_system_account=CASE
        WHEN campaigns.ran_on_system_account IS NULL THEN EXCLUDED.ran_on_system_account
        ELSE campaigns.ran_on_system_account
    END,

Guarding on the STORED value is what preserves the ticket's semantics while fixing the
write. The claim row's provenance is `NULL`, so the first upsert after the claim stamps
it; every later update or status toggle sees a non-NULL value and leaves it alone. A
`COALESCE(EXCLUDED.…, campaigns.…)` — the shape used one line above for `updated_by` —
would fix the write and break the freeze, because it only defends against an incoming
`NULL`: a row holding `false` would still be upgradable to `true` by a later write
carrying one, which is precisely the rewrite of who-paid the column was added to prevent.

Carrying the value on `ClaimCampaignDispatch` instead was considered and rejected. The
claim fires BEFORE `Dispatch`, and `fromSystem` is not known until the dispatcher resolves
credentials inside it — the claim would have to stamp a value it cannot yet compute. It is
also the wrong layer: the claim exists to arbitrate single-flight ownership, and the fix
belongs where the fact arrives.

**Verification** — a live test drives the real sequence (claim, then upsert) and reads the
column back off disk, which is the step no existing test took. Against the unfixed code it
reports `NULL (unknown), want true` and `NULL (unknown), want false`; the `nil` case passed
before and after, which is itself the point — two of three states were silently wrong and
the third was right by accident.

Three mutations, each compiling and each reverted:

- Restoring the bare omission fails `TestLiveClaimThenUpsertPersistsProvenance` on the
  `true` and `false` cases, naming the column and the stored `NULL`.
- A bare `ran_on_system_account=EXCLUDED.ran_on_system_account` fails all four arms of
  `TestLiveProvenanceIsWrittenOnceThenFrozen`, including both erasure cases.
- `COALESCE(EXCLUDED.…, campaigns.…)` fails the two arms that matter — stored `false`
  upgraded to `true`, stored `true` downgraded to `false` — while passing the erasure
  cases, which is exactly the partial correctness that makes it the plausible wrong fix.

The stale claims were rewritten wherever they appeared rather than only at the query:
`model.Campaign.RanOnSystemAccount`'s doc comment, the header of
`TestLiveUpsertDoesNotRecomputeProvenanceOnUpdate` (which asserted "Only omitting the
column from the `DO UPDATE` arm holds both"), and the concept file.

**Docs** — the `## System-account provenance` section had also been inserted mid-way
through `## Actor attribution`, orphaning that section's closing actor-scan discussion and
its `### Audiences` subsection under the wrong heading. Moved after `### Audiences`, before
`## Migration numbering`.
