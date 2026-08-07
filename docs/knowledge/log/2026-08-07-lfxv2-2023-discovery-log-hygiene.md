# 2026-08-07 — LFXV2-2023: the discovery handler logs a reason token, and a decrypt failure is no longer a 400

**Update** — PR #85's `ListGoogleAdsAccounts` sanitized its RESPONSE while logging the full cause,
and answered 400 for a condition that is not a connection problem at all. Both are fixed here; the
dispatch-side half of the change landed in PR #84.

**Fix** — The `ErrConnectionNotUsable` arm logged `"error", aerr`. One of the errors reaching that
arm is computed over the DECRYPTED credential blob — `encoding/json` quotes its input — so the same
credential material the response deliberately withheld was reaching centralized logs, for exactly
the connection whose credentials are malformed. The cause is now dropped at the log call in favour
of `reason=`, produced by `unusableConnectionReason` from the reason sentinel that dispatch wraps
alongside `ErrConnectionNotUsable`: `connection_inactive`, `credentials_undecodable`,
`credentials_incomplete`, `provider_config_invalid`, `credential_blob_malformed`, `unclassified`.
A closed vocabulary is what a log line wants anyway — greppable, alertable, and with no payload to
carry a secret in. The cause is a fixed message today; the point of dropping it is that nothing
stops a future wrap from putting plaintext back into it.

**Fix** — A blob that fails AUTHENTICATED decryption now gets its own arm, ABOVE the
`ErrConnectionNotUsable` one, and answers 500. That failure means a wrong or rotated application
encryption key, or tampering — and the key is deployment-wide, so it hits every project's
connection in the same instant. 400 would send each of their operators to fix a row that is fine;
503 would promise that waiting helps. Both hide an outage behind a message about somebody's
connection. It logs at ERROR because this is the arm that should page someone, and the cause IS
logged here: the encryptor computes it from ciphertext and key material only, never from plaintext.

**Fix** — `TestListGoogleAdsAccounts_UnusableConnectionIs400` was pinning the old behaviour with a
"credential blob that will not decrypt" case, so that case was narrowed out and moved to
`TestListGoogleAdsAccounts_DecryptFailureIs500`. That test's second case carries BOTH sentinels,
which is the regression that would otherwise be silent: reordering the switch is enough to break
it, and nothing else in the file would notice. The log assertion is on the emitted RECORD rather
than the returned error, and plants a marker in the cause that must not appear anywhere in the
line — reverting the fix fails it on the reason token, the marker, and the echoed cause text.
