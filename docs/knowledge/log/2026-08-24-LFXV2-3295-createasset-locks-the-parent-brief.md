# 2026-08-24 — LFXV2-3295 CreateAsset locks the parent brief

**Fix** — the creative-asset insert was the one brief-parented write in this package that did not
lock its parent, and the gap was reachable.

`CreateAsset` ran a single unlocked autocommit statement,
`INSERT ... SELECT ... WHERE EXISTS (an active same-project brief)`, and the surrounding comments
argued that the gate was sufficient. It is not. Under READ COMMITTED each statement takes a fresh
snapshot of the last committed state, and a concurrent non-key status update does not conflict
with the foreign key's key-share lock — so the EXISTS subquery can read a brief that
`ArchiveBrief` is in the act of archiving, and the asset commits under an archived parent.

**Reproduced before it was fixed.** `TestCreativeAssetRepo_CreateAsset_SerializesAgainstArchival`
opens an `ArchiveBrief` in its own transaction and leaves it UNCOMMITTED, which holds the row
lock, then calls `CreateAsset` on another connection. A correctly ordered implementation blocks;
the unlocked one returned a stored asset immediately. The interleaving is forced by the held
transaction rather than raced between goroutines, because a timing race would go green on a fast
machine — which is how this survived several review rounds.

**The fix is a lock, and the lock is what binds.** `CreateAsset` now begins a transaction, takes
`SELECT status FROM campaign_briefs WHERE id = $1 AND project_id = $2 FOR UPDATE`, checks the
status, and runs the insert on that same transaction. Mutation-verified: reverting only the two
words `FOR UPDATE` to a plain `SELECT` compiles and turns the test red with the archived-parent
message. The transaction wrapper alone does not carry the property — the lock does.

**Why the single-statement intuition was wrong**, recorded because it is the reasoning that made
the unlocked version look safe: atomicity means the statement does not half-apply, and says
nothing about what its snapshot may miss. `campaign_repo.go`'s `lockAdoptBriefQuery` already
stated the rule from the other side — what is required is `FOR UPDATE`, "not a plain re-read, and
not the single-statement atomicity of the INSERT".

This makes one outlier consistent with the established call sites in the same package —
`AudienceRepo.CreateAudienceForApprovedBrief`, `JobRepo.CreateJobForApprovedBrief`, `BriefRepo`'s
guarded update, and `campaign_repo.go`'s `lockAdoptBriefQuery` — rather than introducing a new
pattern. (All four lock `campaign_briefs`. `campaign_repo.go`'s other `FOR UPDATE` locks the
`campaigns` row instead, so it is a different lock and not a fifth example of this one.)

**Cost, stated precisely:** uploads to the SAME brief serialize on that brief's row for the
duration of the insert. Uploads to different briefs never contend — the lock is per-brief-row,
not table-wide.

**Why "unreachable is harmless" was not an acceptable answer.** The previous rationale accepted
the race because an archived brief refuses every later read, so the blob would sit unreferenced.
But `creative_assets` has no prune and briefs are never hard-deleted (archive is a soft status
flip), so such a row is retained FOREVER with nothing able to read or clean it. Storage that only
grows and is unreachable by every code path is a different category from a row a later operation
would refuse.

The `WHERE EXISTS` gate is retained on top of the lock, but the lock is what now carries the
TENANT boundary. The lock query matches the parent on both `id` and `project_id`, and the
explicit status check rejects an archived brief, so every refusal — absent, archived, or owned
by another project — returns `ErrNotFound` before the insert runs. The gate re-states the same
predicate under the held lock as defense-in-depth, keeping the insert self-contained if it is
ever reused outside this transaction; it is no longer the clause that stops a cross-project
attach.

The `CreativeAssetRepository` port in `internal/domain/creative_asset_port.go` was updated to
match. Serialization is now a REQUIREMENT of the port rather than something an implementation
"MAY" do, because the consequence of not doing it is permanent rather than transient.

*(History note: `FOR UPDATE` had never previously appeared as SQL in this file — `git log -S`
matches only its mention in a prose comment, in both the file's creating commit and the later
docs commit. An earlier review reply saying this had been fixed with a transaction and
`FOR UPDATE` did not correspond to landed code.)*
