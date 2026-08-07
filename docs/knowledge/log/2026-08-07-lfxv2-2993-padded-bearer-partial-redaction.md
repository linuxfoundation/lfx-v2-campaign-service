# 2026-08-07 — A redaction that leaked the token and looked like it hadn't

**Update** — Closed a review finding on PR #72 (`internal/platform/meta/client.go`
and `client_test.go`).

`redactCredentials` decided WHICH `credentialRE` alternative had fired by searching the
match for a `=` or `:` delimiter. That inference is wrong for the bearer alternative,
because **base64 padding puts `=` inside the token**. `Bearer QUJ…WVo=` therefore split at
the padding and produced `Bearer QUJ…WVo=[REDACTED]` — the entire credential, followed by
a redaction marker.

The marker is what makes this worse than an unredacted leak. A raw token in a log line
looks like a bug; a token trailed by `[REDACTED]` looks like the mechanism worked, and a
reader scanning for leaks skips it. A partial redaction that advertises itself as complete
is a failure mode of its own.

The fix decides the alternative BEFORE any delimiter search: a case-insensitive `bearer`
prefix check picks the scheme branch, and only the key/value branch looks for `=` or `:`.

`TestRedactCredentialsHandlesPaddedBearerToken` is a table over padded, double-padded,
unpadded, and lowercase-scheme bearer values plus the two key/value forms, and it also
asserts no eight-character prefix of the token survives anywhere in the output — an exact
string comparison alone would pass a fix that leaked the token somewhere else in the line.
Revert-verified: restoring the delimiter-first order fails three of the six cases with the
full token in the diagnostic.

One expectation recorded rather than "fixed": the regex consumes the whitespace around the
delimiter, so `{"client_secret": "s3cr3t"}` redacts to `{"client_secret":[REDACTED]}`,
tighter than the input. That is correct for a diagnostic snippet, which is not required to
stay re-parseable JSON.
