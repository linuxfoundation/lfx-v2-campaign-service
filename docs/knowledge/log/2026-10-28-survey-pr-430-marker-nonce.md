# 2026-10-28 — The staged-body marker is unique to each rebuild

**Fix** — the staged-body token was derived from the body alone, so a rebuild that changed only
the native button's destination wrote the same token as the previous one. A silently reverted
content PATCH then read back the earlier write's identical token, verification passed, and
dispatch recorded `contentStaged` for a draft whose button still pointed at the earlier
destination.

- `stagedBodyMarker` now hashes the body together with a fresh 16-byte `crypto/rand` nonce, so the
  token differs on every rebuild and cannot be recomputed from the body.

Test: `TestRebuildEmailContent_ARebuildWithTheSameBodyWritesANewMarker` (identical body, changed
`ButtonURL`, a read-back of the previous write is `ErrContentNotPersisted`).

See [internal/platform/hubspot](../code/internal-platform-hubspot.md).
