# 2026-08-24 — backtick escaping is a Go SOURCE construct, not a comment one

**Fix** — `internal/infrastructure/postgres/dbtest/dbtest.go` carried this inside a `//`
comment:

```
//   - `*pgconn.ConnectError` renders `failed to connect to `+"`"+`user=%s database=%s`+"`"+“ from
```

The `` `+"`"+` `` sequence is how you embed a backtick in a Go RAW STRING LITERAL. In a
comment there is no literal to escape into, so it renders verbatim and the reader sees the
escaping machinery rather than the message being quoted. The final one had also degenerated
into a U+201C LEFT DOUBLE QUOTATION MARK — the only non-ASCII character in the file besides
em dashes.

## The smart-quote trap, and why it did not re-fire

`gofmt` is known in this repo to rewrite certain quote constructs in comments, and a fix
earlier the same day was silently reverted by it. So the correction was checked rather than
assumed: **the construct was removed instead of re-quoted.** The line now describes the
template's shape in plain prose (`a "failed to connect to ..." message embedding a
user=%s database=%s fragment`) with no nested backticks to mangle.

Verified by running the formatter and re-reading the bytes:

```
gofmt -s -w dbtest.go   -> exit 0, diff against the pre-format file: EMPTY
non-ASCII sweep (excluding em/en dash) -> no matches
```

**Do not fight a formatter over a construct you do not need.** Rewording to avoid the
construct is stable; re-quoting it invites the same rewrite next time anyone runs `gofmt`.

## The duplicated sentinel

`SafeDSNErr` and `SafeDSNErrFor` each inlined the same `*url.Error` message verbatim. Two
sites rendering "redacted" independently is precisely the drift that produced `pkg/redact`, so
the string is now the single `dsnUnparseableMsg` constant both return.

The refactor was mutation-checked rather than assumed behaviour-preserving: replacing the
constant's text failed `TestConnectAndMigrateDoesNotEchoTheDSN`,
`TestSafeDSNErrRedactsAWrappedMigratorError` and `TestSafeDSNErrKeepsCredentialsOutOfOutput`.
**An extraction that no test can distinguish from a broken one is not verified** — a surviving
mutation would have meant the assertions were matching something other than this string.
