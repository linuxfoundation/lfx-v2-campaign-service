# 2026-08-24 — LFXV2-2643 a probe that shares a host

**Fix** — the behavioural redaction test written one round earlier passed for the wrong
reason, and a reviewer caught it. Its doc comment claimed the probe DSN was "unrelated to
any environment value, so a regression that reintroduces `SafeDSNErr` fails on CI and
locally alike". That claim was false on CI.

`dsnIdentifiersPresent` compares the DSN's password, user, database AND **host**. The probe
used `127.0.0.1:1`, and a CI `TEST_DATABASE_URL` points at `127.0.0.1` too. A refused dial
names the host in its text, so redaction fired on the shared HOST alone — the user,
password and database comparisons the test is actually about never decided anything.
Verified rather than reasoned: with the fix reverted and `TEST_DATABASE_URL` set to
a DSN of the form `postgres://USER:PASS@127.0.0.1:5432/DB`, the test still **passed**. A
test that cannot fail in the environment it is written to protect is not evidence there.

The probe host is now `127.0.0.2` — still loopback, nothing listening, but a host no harness
DSN uses. Reverting the fix now fails under a CI-like env, under no env, and passes only
with the fix present: three configurations checked, not one.

The same review found the diagnosability assertion was vacuous. It asserted the output
contains `EnvDatabaseURL`, but the wrapper's own format string is `"migrate %s: ..."` with
`EnvDatabaseURL` as that argument, so the substring is present no matter what the redactor
returns — including `""`. It now asserts the output carries the REDACTOR's rendering, and
fails first if that rendering is empty. Mutating `SafeDSNErrFor` to return `""` fires it;
under the old form that mutation was invisible.

This is the SECOND accidental environment-coupling on this branch, not an isolated slip.
The first was a fixture spelling `host=localhost` while the runner's own DSN also said
`localhost`, in `a-dsn-has-two-legal-shapes`. Same class both times: **a test whose fixture
shares an identifier with the runner's real environment can pass for a reason that has
nothing to do with the code under test**, and it passes most reliably on the machine you
least want it to be vacuous on. The defence is not vigilance about hosts specifically — it
is choosing fixture values that CANNOT collide with any environment the suite runs in, and
proving it by reverting the fix under an environment that mimics the runner.

The pattern worth keeping alongside it: an assertion whose subject is produced by the
CALLER's format string tests the format string, not the thing under test. Both defects here were the test
agreeing with itself, one via a shared host and one via a self-supplied substring, and
neither was visible without running the mutation the test claims to catch.

Ref: LFXV2-2643
