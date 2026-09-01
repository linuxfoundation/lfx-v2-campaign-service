# 2026-08-31 — LFXV2-2775: the helper was tested, the wiring was not

**Verification** — `TestBuildLogError_DefaultDeny` calls `buildLogError` directly. It proves the
helper redacts correctly and nothing more: the one thing it cannot see is whether the service
still CALLS it.

Demonstrated by mutating the call site rather than the helper — `"error", buildLogError(buildErr)`
to `"error", buildErr`:

- the helper test **still passed**, every assertion intact;
- the new end-to-end test failed with the credential blob visible in real `slog` output.

`TestBuildAudience_FailureLogIsRedactedEndToEnd` drives a failing build through `BuildAudience`
with `slog.SetDefault` capturing to a buffer, and asserts the token is absent from what the
service actually emitted. It also asserts something WAS logged, so the check cannot pass by the
log being empty.

**The general shape:** a test that calls a helper directly pins the helper's contract, not its
use. Where the security property lives at a call site — a redactor, an escaper, an authz check —
at least one test has to run the path that reaches it. Mutating the CALL rather than the function
is what tells the two apart.

Related: `docs/knowledge/log/2026-08-31-LFXV2-2775-the-arm-my-own-fix-left-behind.md`.
