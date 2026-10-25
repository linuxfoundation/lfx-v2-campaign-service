# 2026-10-26 — a credentials column is no longer printed as a value (#161)

**Fix** — `dbtest/connection_live_test.go` compared a `credentials` column read back from
Postgres and rendered it with `%q` on failure, putting the column's bytes into the build log.
It now reports `len` plus `sha256` on both sides, matching
`credential_roundtrip_live_test.go`, which already did this.

Length AND digest, not length alone: the losing rotation's `ciphertext-v3` is the same length
as the expected `ciphertext-v2`, which is precisely the mismatch the assertion exists to
catch, so a lengths-only message would trade the leak for a test that fails without saying
why.

The class sweep found one site, not several. Only this test `SELECT`s a credentials column;
the other credential-shaped prints in the repo — `internal/dispatch/credcache_test.go`,
`internal/bootstrap/sysacct_test.go`, `cmd/verify-metrics/main_test.go` — print literals the
test itself wrote into an in-memory fake, which is a different category: the log learns
nothing the source file does not already say. The issue's class is a value read back OUT of
the database.

**Note** — the fixture here is harmless today; `ciphertext-v2` is synthetic. The rule is about
the SHAPE of what reaches a log rather than the sensitivity of one fixture, for the same
reason the repo drops an unmarshal cause that might someday quote plaintext: this test is
where the next credential test gets copied from.
