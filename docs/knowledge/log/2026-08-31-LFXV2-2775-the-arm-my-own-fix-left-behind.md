# 2026-08-31 — LFXV2-2775: the arm my own fix left behind

**Fix** — three findings, and the first one is the same defect as the previous entry, in the arm
that fix did not touch.

`buildLogError`'s sentinel arms were changed to return the bare sentinel rather than the caller's
error, because `errors.Is` matches through a whole chain. The `IsAPIError` arm was left returning
`err` — and `IsAPIError` is `errors.As`, which matches through a chain in exactly the same way. So
`fmt.Errorf("token %s: %w", secret, apiErr)` still logged the wrapper verbatim. A fix that
enumerated three of four arms reads as complete because every arm it names is correct.

The remedy is a `hubspot.APIErrorOf` returning the UNWRAPPED error, so the caller gets the safe
method/path/status rendering the package's own godoc already promised. A bool cannot deliver that
promise: it tells a caller a safe error is somewhere in the chain, and the caller then logs the
chain.

Also fixed:

- `ErrCredentialsMalformed` was collapsed into `ErrCredentialDecryptionFailed`. Same log line for
  two failures with **opposite remedies** — re-store the credential row, vs. check the application
  key the service booted with.
- `ConfigSnapshot` carried the generated `subject` and `bodyHtml`, because the whole
  `hubspotConfig` was passed to `applyCampaignConfig`. That column is unencrypted and API-visible;
  generated body HTML is caller-supplied and its `href`s can carry query tokens. Now a separate
  `hubspotConfigProvenance` type carries only what the snapshot exists for — a field added to
  `hubspotConfig` is not persisted until someone adds it there deliberately.

**The general shape, twice over:** when a guard has several arms, the fix is the CLASS, not the
arm that was reported. And a test that feeds an unwrapped value cannot see a wrapper bug — every
one of these needed a wrapped fixture before it could fail.

Related: `docs/knowledge/log/2026-08-31-LFXV2-2775-a-named-arm-returned-the-wrapper.md`.
