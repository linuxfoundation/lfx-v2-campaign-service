# 2026-11-17 — #581: map a system-row decryption failure to 500 on adoption

**Fix** — An adopt-campaign with `account_id` reads through the LF system LinkedIn row, but an
authenticated-decryption failure on that row (a wrong or rotated application key, or corrupted
ciphertext) did not reach the 500 promised for a broken LF row. `decryptConn` wraps
`ErrCredentialDecryptionFailed`, not `ErrConnectionNotUsable`, so `res.systemScoped` adds no
`ErrSystemConnectionNotUsable`, and `AdoptCampaign`'s lookup switch fell through to its 503
default, which also logged the error chain. A new arm, on the `account_id` path only, answers
the same 500 and logs a fixed message naming the LF system row (`project_id`) and the requester
(`requested_by_project_id`), with no error text, as the metrics handler does: `domain.Encryptor`
implementations may quote the ciphertext or key they failed on. Pinned by
`TestAdoptCampaign_SystemRowDecryptFailureIs500AndLogsNoErrorText`.

- `list-linkedin-ads-adoptable-campaigns` already answered 500 with a fixed log line through
  `classifyDiscoveryError`'s decryption arm. No code change there; that behavior is now pinned by
  `TestListLinkedinAdsAdoptableCampaigns_SystemRowDecryptFailureIs500AndLogsNoErrorText`.
- Both endpoints' DSL descriptions and `docs/api-catalog.md` rows now list the decryption failure
  among the 500 causes; `gen/` and the kodata OpenAPI copies were regenerated.
- An adoption without `account_id` is unchanged: it resolves the project's own row only.
