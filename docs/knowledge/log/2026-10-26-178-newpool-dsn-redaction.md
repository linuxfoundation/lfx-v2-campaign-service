# 2026-10-26 — NewPool is held to Migrate's redaction boundary (#178)

**Fix** — the sibling-parity sweep on the #178 branch found the same leak still open in
`NewPool`, sixty lines from the `Migrate` boundary that had just closed it. `NewPool` is
`Migrate`'s nearest sibling by every measure that matters here: same package, same file, same
`dsn string` parameter, and `internal/container/container.go` calls the two of them seventeen
lines apart inside `initDatabase` (`postgres.NewPool(ctx, dsn)` at line 1275,
`migrateDone <- postgres.Migrate(dsn)` at line 1292), wrapping and propagating both to a
logging caller.

The leak was not reasoned about but rendered. A throwaway probe built `NewPool` against
`postgres://probeuser:probepass@127.0.0.1:1/probedb` and printed the error text:

```
ping database: failed to connect to `user=probeuser database=probedb`: …
```

That is the `*pgconn.ConnectError` template `SafeDSNErr`'s own doc comment documents, verified
against pgx v5.9.2 (`go.mod` line 14). It fires on every failure to connect — which means a
database that is not up yet puts the configured user and database name into the pod log on
every restart, where migrations run only once. The higher-frequency half of #178's defect was
in the arm #178 did not touch.

Both the `pgxpool.NewWithConfig` and `pool.Ping` arms now go through `newSafeDSNError`, which
`redactMigrateErr` was factored down to so that the package's two DSN boundaries cannot drift
about what "redacted" means — the same reasoning that made `dsnUnparseableMsg` a constant.
`migrateDSNSource` is renamed `dsnSourceDescription`, since it is no longer Migrate's alone.
The arm labels `open pgx pool:` and `ping database:` survive the redaction: an operator still
has to be able to tell the two apart, and only the driver's prose is withheld.

The `pgxpool.ParseConfig` arm is deliberately left alone. It already returns `dsnParseError`,
whose message is static and names `DATABASE_URL`; putting it through the DSN redaction as well
would replace a precise diagnostic with the generic "does not parse as a URL" sentinel and tell
an operator less, for no gain, since both are already DSN-free. That is the double-redaction
hazard the #178 entry records, met inside one function rather than across a call.

The same hazard across a call also turned up and was closed: `dbtest.connectAndMigrate`'s
`NewPool` arm wrapped the error in `SafeDSNErrFor`, which was correct before this change and
degrading after it, for the two reasons that entry sets out. It now passes the error through,
exactly as its `Migrate` arm already did. The knowledge concept said in as many words that this
arm "KEEPS its `SafeDSNErrFor`, because nothing redacts behind it" — a sentence this change
made false, and the doc-vs-code check is what caught it.

Three tests pin the behaviour, all hermetic: port 1 on the loopback interface is closed, so the
connection is refused without reaching a network or a database.
`TestNewPoolDoesNotLeakIdentifiersWhenThePingFails` covers the probed arm and also asserts the
arm label survives; `TestNewPoolKeepsTheConnectErrorReachable` pins the `errors.As` chain, the
identity half that `TestRedactedMigrateErrorKeepsItsIdentity` pins for `Migrate`; and
`TestNewPoolDoesNotDoubleRedactTheParseArm` guards the arm left alone, because collapsing the
three arms into one wrapper is the tidying-up a later reader is most likely to attempt.

**Verification** — each test was mutation-tested, and each mutation was reverted after it was
observed. Replacing the ping arm's `newSafeDSNError(dsn, err)` with a bare `err` failed only
`TestNewPoolDoesNotLeakIdentifiersWhenThePingFails`, which reported the leaked `leakuser` and
`leakdb` in the error text. Routing the `ParseConfig` arm through `newSafeDSNError` failed only
`TestNewPoolDoesNotDoubleRedactTheParseArm`, which reported the generic sentinel in place of
`invalid DATABASE_URL`. Collapsing `newSafeDSNError` to `errors.New(SafeDSNErr(…))` — dropping
the `Unwrap` — failed both identity tests, `TestRedactedMigrateErrorKeepsItsIdentity` and
`TestNewPoolKeepsTheConnectErrorReachable`, which is the pair that stops a permanent failure
being misread as transient.
