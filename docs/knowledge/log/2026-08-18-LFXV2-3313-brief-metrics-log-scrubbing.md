# 2026-08-18 — LFXV2-3313 brief-metrics log scrubbing is now tested

**Change** — added `TestGetBriefMetrics_ConnectionFailureLogsNoErrorText` and
`TestGetBriefMetrics_DecryptFailureLogsNoCauseText`.

The scrubbing itself already existed: `GetBriefMetrics` logs the fixed reason token for
connection arms and nothing at all for the decrypt arm, mirroring `GetCampaignMetrics`. What was
missing was any test that would fail if it were undone — and this is a guard where an untested
correct implementation is only one refactor away from a silent leak, because reintroducing
`safeErrSummary(merr)` on either arm compiles, passes every other test, and produces a plausible
log line.

**Why these arms carry no error text.** `safeErrSummary` normalises and truncates; it does not
REDACT. A decrypt cause comes from the Encryptor INTERFACE and may quote ciphertext or key
material; an unusable-connection cause can embed fragments of a decrypted blob. The brief-wide
read fans out across every campaign, so one malformed credential row would write those fragments
once per campaign into centralised logs.

**Both tests assert the line still EXISTS.** A "does not contain the secret" assertion passes
trivially if nothing is logged at all, which would trade a leak for an outage nobody can
diagnose. The connection test also requires a `reason=` token so an operator can still tell what
to fix, and the decrypt test requires `level=ERROR` — a rotated key fails every project at once
and the cheap discriminator is the COUNT of those lines, so at WARN it would page nobody.
