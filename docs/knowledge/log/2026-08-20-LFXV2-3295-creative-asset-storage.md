# 2026-08-20 — LFXV2-3295 creative-asset storage

**Creation** — the storage layer for uploaded ad images: migration `000028`,
`model.CreativeAsset`, the `domain.CreativeAssetRepository` port, and `CreativeAssetRepo`.
This is PR 1 of 3 — nothing calls it yet. The upload endpoint and the dispatch-time asset
resolution land separately, so the repo is deliberately not wired into the container here.

Salvaged from the unmerged `feat/meta-image-video-creatives` branch and rebased onto main,
scoped down to storage. The branch was named against LFXV2-2665, a different and already
closed ticket; the work is LFXV2-3295 and that key was not carried over.

## Why the bytes are stored at all

An ad platform's image handle is per-ad-account and only resolvable at dispatch, so it cannot
be obtained when an operator uploads. The source bytes therefore have to survive the gap
between upload and campaign create, and they live in Postgres as `BYTEA` — the service is
single-datastore and does not have object storage. They are stored plaintext: an ad image is
not a secret, unlike the credential blobs the connection tables encrypt.

The table is INSERT-ONLY. There is no `version` and no `updated_at`/`updated_by`, unlike
`campaign_briefs` / `campaigns` / `campaign_audiences`, because an asset's bytes are immutable
once stored — a changed image is a NEW asset with a new checksum. A re-upload of identical
bytes is resolved by `UNIQUE (brief_id, checksum)` returning the existing row, not by mutating
one, so there is nothing for an optimistic-concurrency handle to protect.

## The migration is 000028, and 000027 is a recorded gap

Derived rather than assumed, because a wrong renumber is expensive in both directions. Main's
highest is `000026_campaign_jobs_retention_index`. Sweeping the migration filenames in the diff
of every open PR — not their titles or bodies — showed exactly one other claimant: PR #164
(LFXV2-3050) holds `000027_campaigns_ran_on_system_account`. So 000028 is the first free
version, and taking 000027 would have produced a collision that is green on both PRs and red
only on whichever merged second, which is the failure `TestMigrations_UniqueNumbering`'s
comment records from the 000015 incident.

The resulting gap is registered in `allowedVersionGaps[27]` naming #164. That entry is a
MERGE-ORDER obligation, not a numbering excuse: golang-migrate records only the highest applied
version and never applies a lower one afterwards, so if this branch deploys before #164, that
migration is skipped silently and permanently. It self-expires —
`TestMigrations_AllowedVersionGapsAreStillOpen` fails the moment 000027 exists in the tree.

Both directions were verified, not reasoned about: deleting the entry fails
`TestMigrations_NoVersionGaps` with `jumps from 000026 to 000028`; creating a placeholder
000027 fails `TestMigrations_AllowedVersionGapsAreStillOpen` with `allowedVersionGaps[27] is
stale`. Beyond the two filenames, the version number leaked into the concept doc's migration
list and the `CreativeAssetRepo` section — the reason the numbering guidance says to renumber
the branch with the FEWEST references to the number.

## The parent FK is composite, because the API gate is not the only writer

Pre-PR reviewer simulation caught this, and migration `000007` had already written the
argument down. `campaign_audiences` originally took a `brief_id`-only FK and `000007`
retrofitted a composite `(brief_id, project_id)` one, because that table copies `project_id`
and its read path trusts the copy for tenant scoping — so a worker, backfill, or direct write
could persist a row whose `project_id` named a different project than its brief, and the read
would serve it under the wrong tenant. Its comment is explicit that the API create path's
`WHERE EXISTS` guard is not sufficient, since "the DB is meant to be the source of truth for
ALL writers".

`creative_assets` has exactly that shape — it copies `project_id`, and `GetAsset` scopes on the
stored copy — so it was carrying the hole `000007` had already closed next door. It now takes
the composite FK directly rather than being retrofitted later.

The gate and the FK are not redundant: the gate protects the API path, the FK protects the
table. `TestCreativeAssets_CompositeTenantFKRejectsMismatchedProject` therefore inserts
DIRECTLY, bypassing the repository — a write through `CreateAsset` is stopped by the gate and
would prove only that the gate works. Reverting to the `brief_id`-only FK fails it with the
cross-tenant row actually created, not merely with a missing error.

## What else the reviewer pass changed

- **A `byte_size` constraint** — first added as `CHECK (byte_size >= 0)` and later superseded by
  the equality CHECK described below, which subsumes it. No UPPER bound at any point: that would
  have to equal the upload endpoint's request limit, and the endpoint lands in a later PR, so
  fixing a number here would mean two limits that silently disagree.
  `TestCreativeAssets_ByteSizeChecksBindSizeToPayload` also asserts `0` stays legal, so the
  constraint cannot be quietly tightened into rejecting a row the repository can write.
- **The retention question is recorded in the migration rather than left to a disk alert.**
  Nothing prunes this table and NOTHING BOUNDS IT. A first pass said growth was "bounded per
  brief by the checksum dedupe"; that is false and the bot review caught it. The dedupe collapses
  a re-upload of the SAME image to one row, while a brief can still accumulate unlimited DISTINCT
  images — each a raw blob rather than the small metadata row the two tables that DO have prunes
  grow by. Briefs are never hard-deleted (archive is a soft status flip), so there is no orphan
  path and no `ON DELETE` to choose, which also means an archived brief's images are kept
  forever. Two things are owed, and neither is guessed at here: a per-brief CAP (the upload
  endpoint's job, alongside the request-size limit) and a PRUNE (needs a retention policy the
  service does not have). Both are named in the migration so the exposure is recorded rather
  than discovered from a disk alert.
- **Two comments claimed a `design/brief.go` MIME Enum that does not exist** — the endpoint is a
  later PR, so there were two enforcement layers, not three. Both now say the contract Enum will
  mirror the set when it lands.
- **`GetAsset`'s comment now says what the shared convention does NOT give you:** the id column
  is uncast, so a MALFORMED id is a Postgres 22P02 and surfaces as a 500, not `ErrNotFound` —
  only a well-formed id matching no row is a 404. `GetBrief` and `GetAudience` behave
  identically, so this is the repo's convention, and the port doc already places the validation
  obligation on the caller. Recorded so the endpoint PR does not infer a 404 it will not get.
- **`GetAsset` did not require an ACTIVE parent, and the sibling read does.** Cursor caught
  this and it is a genuine convention deviation, not a style note: `GetAudience` carries an
  `EXISTS` on a non-archived brief with an explicit rationale — once a brief is archived its
  children leave the live lifecycle, so get must 404 rather than list 404-ing while get still
  succeeds on the same nested resource. Without it archival was half-applied here: `CreateAsset`
  refused an archived parent while `GetAsset` kept serving the bytes. Removing the new `EXISTS`
  fails the test with `err = <nil>` — the archived brief's image really was returned.
- **`CHECK (byte_size = octet_length(bytes))`, after a wrong performance claim of mine was
  checked.** A first pass declined this CHECK on the grounds that it "would detoast and hash the
  full image on every insert". A reviewer disputed it, and measuring settled it: `octet_length`
  reads the varlena size header via `toast_raw_datum_size` and does not detoast — **0.27 ms over
  150 MB of TOASTed rows, against 179 ms for `md5()` on the same rows**, a ~670x difference. The
  cost I had used to justify leaving the invariant unenforced did not exist. Since `CreateAsset`
  binds `ByteSize` and `Bytes` independently, the database was the only place the relationship
  could be enforced at all, and it now is. The measurement is recorded in the migration so the
  claim is not re-derived from intuition next time. Adding it also made a NEIGHBOURING constraint
  redundant — `CHECK (byte_size >= 0)`, added one round earlier, is implied by the equality —
  and the review caught that its mutation claim had gone stale. It is removed: a constraint whose
  revert fails no test is not a guard.
- **Two constraints no test reached.** Both were green-but-unbound, the classic shape: every
  successful case in the file used an `approved` brief and `image/png`, so the parent predicate
  could have been narrowed to `status = 'approved'` and the MIME allow-list dropped or narrowed
  to PNG, with the suite staying green. Narrowing the parent predicate would have broken real
  behaviour — uploading a creative is part of COMPOSING a brief, so ACTIVE (not APPROVED) is
  deliberate, unlike campaign creation which does gate on approval.
  `TestCreativeAssetRepo_CreateAsset_AcceptsDraftBrief` now drives a `draft` parent, and
  `TestCreativeAssets_MimeTypeCheckRejectsUnsupported` asserts BOTH sides of the allow-list —
  `image/gif` refused and `image/jpeg` accepted — so a CHECK narrowed to PNG fails too.
- **The archival-race claim was too strong, and the bot reviewers were right about it.** The
  gate was described as closing the window a separate `SELECT`-then-`INSERT` would open. It
  removes the APPLICATION-level window, but it takes no lock on the parent, so under READ
  COMMITTED the snapshot can still see an active brief while `ArchiveBrief` commits. The repo
  already documents this exact limitation on `CreateAudienceForApprovedBrief`, which is why it
  uses `SELECT … FOR UPDATE`. The fix here is the comment, not a lock: the plain
  `CreateAudience` holds the identical position with the identical `status <> 'archived'`
  guarded insert, and the asymmetry is deliberate — losing the APPROVED race builds real
  HubSpot lists, whereas storing an image under a just-archived brief leaves an unreferenced
  blob that an archived brief's own refusals keep unreachable. All three sites now say what the
  gate does and does not guarantee, and name the condition under which it would need locking.
- **The `ON CONFLICT` no-op is a no-op in its VALUES, not in its write** — a duplicate still
  writes a row version and leaves a dead tuple. Harmless at this volume, and the alternative
  reintroduces a read-after-write race, but "no-op" should not be read as "no write".

## The three things the single INSERT statement is doing

`CreateAsset` is one statement — `INSERT ... SELECT ... WHERE EXISTS ON CONFLICT ... DO UPDATE
... RETURNING` — and each clause carries behaviour that no source-text assertion can reach.
That is why this repo has a live-database test instead of the SQL-string tests the campaign and
connection repos use.

- **`WHERE EXISTS (active, same-project brief)`** is the parent gate, in SQL rather than as a
  preceding `SELECT`. That removes the application-level `SELECT`-then-`INSERT` window, and
  only that: the statement takes no lock on the parent, so a concurrent `ArchiveBrief` can still
  commit after the snapshot sees an active brief — see the correction above for why the answer
  is an accurate comment rather than a lock. The gate is also a tenant boundary: the `brief_id`
  FK proves only that the brief exists, not that it belongs to this caller.
- **`DO UPDATE SET byte_size = creative_assets.byte_size`** is a no-op that exists only to make
  the conflicting row eligible for `RETURNING`. `DO NOTHING` emits no row for a row it did not
  touch, so a repeat upload would surface `ErrNoRows` → a spurious `ErrNotFound` on what is
  actually success. The `SET` copies NOTHING from the incoming row (note the qualified
  `creative_assets.`, not `EXCLUDED.`): a matching checksum already means the bytes are
  identical, so the re-sender has nothing new to write and the FIRST uploader's `created_by`
  must survive. Re-sending an image does not re-author it.
- **`RETURNING` omits `bytes`.** Only `GetAsset` selects the blob, because it is the read whose
  whole point is the image. A write shipping a multi-megabyte column back would be pure waste.

## The assertion that was missing: byte_size against the payload

An earlier mutation audit of the source branch reported `ByteSize: 0` as a SURVIVOR, and
re-running it confirmed the shape of the hole, though not quite in the form reported.

The happy-path test compares `stored.ByteSize` against `asset.ByteSize`. Both sides trace back
to a single in-memory field, so it proves the value round-tripped — it does not prove the value
describes the image. A crude `0` mutation happens to be caught by it (both `0` and
`len(p.Bytes)` are observable there), but the class it belongs to is not:

- binding `int64(len(a.Checksum))` — a plausible nonzero wrong value — was caught, but only
  because it still disagreed with the caller's field.
- **persisting `a.Bytes[:len(a.Bytes)-1]` while binding the caller's `ByteSize` unchanged
  SURVIVED the happy-path test entirely.** Stored size and returned size agree with each other
  and with the caller; they simply do not describe the bytes on disk. Nothing noticed.

That last mutation is the real finding, and it matters because `byte_size` exists precisely so
callers and metrics can read the size WITHOUT loading the blob — the one column whose
disagreement with the image is invisible to every reader that uses it as intended.

`TestCreativeAssetRepo_CreateAsset_StoresByteSizeMatchingPayload` closes it by making the
DATABASE compute `length(bytes)` over the stored row and asserting `byte_size` equals that. It
fails on all three mutations above, including the one the existing test could not see.

**Verification** — every mutation compiled, was run against a live database, and was reverted:

- `a.ByteSize` → `0`: new test fails (`byte_size = 0 but the stored image is 557 bytes`).
- `a.ByteSize` → `int64(len(a.Checksum))`: new test fails (`byte_size = 35 ... 557 bytes`).
- persist `a.Bytes[:len(a.Bytes)-1]`, bind `a.ByteSize` unchanged: new test fails
  (`byte_size = 557 but the stored image is 556 bytes`) while
  `TestCreativeAssetRepo_CreateAsset_StoresAndReturnsMetadata` PASSES. This is the survivor.
- `DO UPDATE SET byte_size = creative_assets.byte_size` → `SET created_by = EXCLUDED.created_by`:
  the idempotency test fails with `created_by = {"principal": "second-uploader"}, want the first
  uploader preserved`. The no-op is load-bearing, not decoration.
- narrowing the parent predicate to `status = 'approved'`:
  `TestCreativeAssetRepo_CreateAsset_AcceptsDraftBrief` fails with `resource not found`.
- dropping the `mime_type` CHECK: the allow-list test fails with `'image/gif' was accepted`.
  Narrowing it to `image/png` alone fails the same test on the other side, with `image/jpeg`
  refused — so the test binds the allow-list, not merely the presence of a constraint.
- removing `GetAsset`'s active-parent `EXISTS`: the archived-parent subtest fails with
  `err = <nil>` — the read served an archived brief's bytes rather than 404-ing.
- removing `CHECK (byte_size = octet_length(bytes))`: the size test fails with `byte_size = 999
  was accepted for a 4-byte image`, and the negative case fails too — the equality is what pins
  all three edges.
- removing `CHECK (byte_size >= 0)` **SURVIVED**, which is why that constraint is gone. Once the
  equality CHECK existed the `>= 0` clause was implied by it (`octet_length()` is never
  negative), so reverting it broke nothing. An earlier revision of this entry claimed the
  negative case pinned it independently; that was true before the equality CHECK landed and
  false afterwards, and the review caught the stale claim. A constraint whose revert fails no
  test is doing nothing, so it was removed rather than left in with a wrong justification.
- reverting the composite FK to `brief_id`-only:
  `TestCreativeAssets_CompositeTenantFKRejectsMismatchedProject` fails with `a direct insert
  pairing brief ... with project_id "...foreign-proj..." succeeded` — the cross-tenant row was
  really created, so the diagnostic reports the vulnerability rather than an absent error.
- dropping the `WHERE EXISTS` clause: all three refusal paths fail — and the cross-project case
  fails with `err = <nil>`, meaning the mutation genuinely attached an asset to another
  project's brief rather than merely mis-mapping an error. The test asserts nothing was STORED
  as well as which error came back, because a broken gate can get one of those right and the
  other wrong.
