# 2026-10-15 — verify-metrics: escaped secrets, echoed signatures and pending exchanges

**Fix** — six review threads on #362.

- A credential containing a quote or backslash, echoed under an ordinary JSON key, was printed
  JSON-escaped and matched neither recorded form. Decoded string values are now scrubbed before
  re-encoding, and the secret set also records each value's JSON-escaped forms.
- A secret-named query value (`sig` on a Microsoft report download) was hidden in the displayed
  URL but never recorded, so a failure body echoing it alone was printed. Such values are now
  recorded before the request is sent.
- A JSON-looking body that does not decode whole (capture cap, interrupted read) fell back to raw
  text, where an escaped `&` ended the URL reduction before `sig=`. It is now withheld.
- A request that outlived the read (an asynchronous token refresh past the timeout) was written
  without the printer's lock and could print as status 0. Exchange updates are now made under the
  recorder's lock, the body copy is snapshotted under its own, and an unanswered exchange prints
  as pending.
- The `LFX_FORCE_SYSTEM_ADS_ACCOUNT` note claimed the system account was used; metrics reads
  ignore the flag and follow the row's recorded account. The note and the concept now say so.
- Each fix has a regression test that fails when the fix is removed; the pending-exchange test
  reports a data race under `-race` when the status write is unlocked.
- Updated [cmd/verify-metrics](../code/cmd-verify-metrics.md).
