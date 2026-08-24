# 2026-08-24 — LFXV2-2643 redacting against the wrong DSN

**Fix** — a reviewer flagged that `connectAndMigrate`'s pool arm wrapped `postgres.NewPool`
with `%w`. Rendering a real failed `NewPool` confirmed it: the output carried
``failed to connect to `user=leakuser database=leakdb` `` verbatim, because pgx builds
`*pgconn.ConnectError` out of the parsed Config and `Pool` prints the wrapped error.

Probing that arm surfaced a LARGER defect one line above it, in the arm a previous round
believed it had already fixed. The migrate arm did call a redactor — `SafeDSNErr(err)` —
so it read as handled. But `SafeDSNErr` redacts against the ENVIRONMENT's `DSN()`, while
`connectAndMigrate` is handed an EXPLICIT `dsn`. On the scratch path those differ, since the
scratch DSN is rewritten from `TEST_DATABASE_URL`, so the comparison ran against the wrong
string and withheld nothing.

The second half of the trap is which arm of the redactor runs. `SafeDSNErr` discards a
`*url.Error` outright, and the comment on that call asserted that arm applied. It does not:
golang-migrate wraps pgx's `*pgconn.ConnectError`, which is not a `*url.Error`, so control
reached the string arm — the one arm whose correctness DEPENDS on being given the right DSN.
A redactor call is not evidence of redaction; the arm it takes and the value it compares
against both have to be checked.

Both arms now use `SafeDSNErrFor(dsn, err)`, and the behavioural test
`TestConnectAndMigrateWithholdsTheExplicitDSN` pins the reachable one against a DSN
unrelated to any environment value, with a control asserting pgx's own error DOES name the
user so the assertion cannot pass vacuously. Reverting the fix fails it with the credential
visible, which is what makes it evidence rather than decoration.

Stated plainly: the POOL arm is fixed by inspection but is NOT pinned by a test. Reaching it
needs a DSN that migrates successfully and then fails its ping, and `postgres.Migrate`
connects first, so any unreachable DSN fails at migrate. Reverting the pool-arm fix leaves
the suite green. That arm rests on the same reasoning as the migrate arm, and it should be
read as unverified until a test can stage the race.

Ref: LFXV2-2643
