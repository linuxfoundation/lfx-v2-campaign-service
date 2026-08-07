# 2026-08-07 — LFXV2-2023: a decrypt failure is two conditions, and only one of them is the connection's fault

**Update** — PR #84 no longer flattens every `Encryptor.Decrypt` failure into
`domain.ErrConnectionNotUsable`. The classification now splits at the port: a blob that was never
authenticated is bad row data (400), a blob that FAILED authentication is an infrastructure and
security condition (500, page ops), and an unclassified failure defaults to the latter.

**Fix** — `credsSource.resolve` tagged the decrypt branch `ErrConnectionNotUsable`
unconditionally. The follow-up PR maps that sentinel to 400, so a wrong or rotated application key
— which is deployment-wide and fails EVERY project's connection in the same instant — would have
answered 400 to every one of them, told each operator to go fix a connection that was fine, and
erased the only 500 that says the deployment is broken. `internal/infrastructure/crypto/aesgcm.go`
had already drawn the line the caller was ignoring: `ErrCiphertextTooShort` is documented as a
malformed-input error and `ErrDecryptionFailed` as "an infrastructure/security condition (map to
500 and alert ops)".

**Fix** — The distinction had nowhere to live that a caller could see. Nothing above
`internal/infrastructure` imports `crypto` — the dispatch and service layers depend on the
`domain.Encryptor` PORT — so the two `crypto.Err…` sentinels were invisible to exactly the code
that had to choose between blaming the row and paging ops. `domain.ErrCredentialsMalformed` and
`domain.ErrCredentialDecryptionFailed` now carry it, the port's doc states the wrapping obligation
as part of the `Encryptor` contract, and each `crypto` sentinel wraps its domain counterpart —
which keeps `errors.Is` working across the boundary without inverting the dependency, and leaves
both usable as `crypto.Err…` inside the package. `ErrCiphertextTooShort` is still returned by
identity, so the existing `==` comparison in its test is untouched.

**Fix** — The default arm is the authentication path, not the row-data path. An `Encryptor`
whose error carries neither sentinel has proven nothing about the stored blob, and reading that
silence as an accusation is the expensive direction of the call: mistaking an outage for user error
sends everyone chasing their own data while the real fault goes unreported. Only an explicit
`ErrCredentialsMalformed` earns the 400.

**Fix** — The test that pinned the old behaviour was pinning the bug. The "credential blob that
will not decrypt" case in `TestGoogleAds_ListAccounts_StillRejectsUnusableConnections` used a fake
returning an unclassified error and asserted `ErrConnectionNotUsable`; it is now a structurally
malformed blob, which is the only decrypt failure that belongs in that table.
`TestGoogleAds_ListAccounts_DecryptFailureIsNotAConnectionProblem` covers the other half and runs
the REAL `AESGCM` for its main case — sealing under one key and opening under another, the rotated-
key scenario itself — because the classification is a two-package contract and a fake asserting its
own error back would prove neither half. `TestAESGCM_DecryptErrorsCarryDomainClassification` pins
the half that leaves `crypto`: without it, dropping the domain wrap leaves every existing crypto
test green while a short ciphertext silently falls into the dispatch layer's 500 default.
