# 2026-08-22 — LFXV2-2643 sequence ownership is not the DEFAULT expression

**Fix** — the down-migration snapshot in `migrate_down_live_test.go` carried a
`pg_sequences` arm that could not fail on a down file which detached or
reassigned a serial sequence, and the comment explaining the omission asserted a
false property of PostgreSQL as its justification.

The comment read:

    Ownership is likewise omitted; it is carried by the column's DEFAULT
    expression, which the column arm already renders as
    `nextval('index_outbox_id_seq'::regclass)`.

That is false. Postgres records `OWNED BY` as a separate `pg_depend` entry —
`deptype = 'a'`, the auto-dependency from sequence to column — entirely
independent of the column DEFAULT. `ALTER SEQUENCE ... OWNED BY NONE` and
`ALTER SEQUENCE ... OWNED BY <other column>` both leave the DEFAULT
byte-identical, so the column arm renders exactly the same string in all three
states. The snapshot selected only `data_type, start_value, min_value,
max_value, increment_by, cache_size, cycle`, none of which moves either.

This is the shape worth naming: the omission was not an oversight but a
*reasoned* one, and the reasoning was the thing that was wrong. A comment that
justifies a gap is harder to audit than a bare gap, because it answers the
question a reader would otherwise ask.

**Why it matters beyond tidiness.** Ownership is what makes a serial sequence get
dropped along with its table. A down file that detaches one leaves an orphan
sequence behind after a rollback — the exact class `TestLiveMigrationsGoDownAndUpAgain`
exists to catch — and the old snapshot called that an exact inverse.

**Proven by mutation, both directions, on PG16.** The site is 000011's DOWN file:
`schemaAtVersion` builds its reference by migrating a fresh database UP, so
mutating an UP file moves both sides equally and proves nothing.

The first mutation attempt is itself worth recording, because it failed for the
wrong reason and would have been misread as confirmation. Appending

    ALTER SEQUENCE index_outbox_id_seq OWNED BY NONE;

made the test fail — but at **version 10**, not 11, with `UNEXPECTED (the down
did not remove it): sequence:index_outbox_id_seq ...`. Detaching the sequence
outright stops 000010's `DROP TABLE index_outbox` from cascading to it, so the
mutation perturbed a later step rather than exercising the property under test.
A red test is not evidence the guard works; the failure has to be the one the
guard would produce.

The isolating mutation reassigns ownership within the same table, keeping the
cascade intact:

    ALTER SEQUENCE index_outbox_id_seq OWNED BY index_outbox.object_id;

Before the fix, with that line in place:

    --- PASS: TestLiveMigrationsGoDownAndUpAgain (5.89s)

After adding the `pg_depend` join, same mutation:

    stepping DOWN from version 11 did not restore the schema that migrating UP
    to version 10 produces:
      UNEXPECTED (the down did not remove it): sequence:index_outbox_id_seq ...
        CACHE 1 NO CYCLE OWNED BY index_outbox.object_id
      MISSING (the down removed too much): sequence:index_outbox_id_seq ...
        CACHE 1 NO CYCLE OWNED BY index_outbox.id
    --- FAIL: TestLiveMigrationsGoDownAndUpAgain (2.78s)

A behaviour-preserving control was also run — `OWNED BY NONE` immediately
followed by `OWNED BY index_outbox.id`, which churns ownership and restores it —
and the test correctly PASSES. Without that control the new arm could have been
failing on any sequence statement rather than on the property.

The join is a LEFT JOIN with `COALESCE(o.owner, 'NONE')`. A standalone
`CREATE SEQUENCE` has no owning column, and an inner join would drop it from the
snapshot entirely — making an unremoved standalone sequence invisible, which is
the same class of blindness this entry is fixing.

**Two sibling fragments repeat the false claim and are deliberately left as
written.** `2026-08-20-LFXV2-2643-assertions-that-can-fail-correct-code.md:79`
carries the same "ownership is omitted; the column arm already renders it as the
DEFAULT expression" reasoning, and
`2026-08-19-LFXV2-2643-credential-roundtrip-live-tests.md:146` records mutation
output — `"returned version 1, want greater than the created 1"` — that a later
commit on this branch superseded when the assertion tightened to the exact
increment (`updated.Version != created.Version+1`, `credential_roundtrip_live_test.go:331`).
Both are dated entries: they record what was true and believed on their date, and
one entry never edits another's file. Corrected here instead, which is where the
correction is dated. A reader who follows either line arrives at this entry
through the concept file, which now carries the accurate account.
