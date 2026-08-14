# 2026-08-14 — LFXV2-3257 the variant index guard also requires indisready

**Fix** — Migration 000023's verification guard checked `indisvalid` but not
`indisready`, so it could pass on an index that arbitrates nothing.

The two flags fail APART. `pool.go`'s `requiredIndexQuery` already says why, at
the boot-time check: *"a CONCURRENTLY build that dies between its phases can
leave an index marked valid but not ready, which enforces nothing on new
writes."* 000022 builds the replacement CONCURRENTLY, which is exactly the build
that can land in that state.

The consequence is specific to this migration's position in the sequence: 000023
exists to prove the replacement enforces what the old arbiter enforced, and
000024 then DROPS the old one on the strength of that proof. A not-ready
replacement would have satisfied the guard, lost the real arbiter one migration
later, and left nothing stopping a retry from creating a second paid campaign —
the failure the whole slot key exists to prevent.

**Note** — The guard and the boot check must require the same thing, or a schema
that satisfies the migration fails at startup. They now both read
`indisvalid AND indisready`. Verified against live Postgres: the real index
reports `valid=true ready=true` and still passes.
