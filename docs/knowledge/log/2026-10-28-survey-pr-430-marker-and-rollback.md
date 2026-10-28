# 2026-10-28 — Body persistence by marker; honest survey rollback notes

**Fix** — two follow-ups from the #430 review.

- **`verifyContentSaved` checks a marker, not the whole body.** The rebuilt body now ends with an
  HTML comment carrying a hash of itself (`<!-- lfx-staged:… -->`, `stagedBodyMarker`), and the
  read-back body must contain it. The whole-html comparison added earlier the same day would have
  read HubSpot's own re-serialisation (`&` to `&amp;`, `<br>` to `<br />`, attribute order) as a
  revert, logging every staged draft at ERROR and refusing every send workflow. A comment because
  it renders nothing and email HTML relies on comments surviving (MSO conditionals). An undecodable
  widgets read is now `errVerifyReadFailed` (retryable), not a proven revert.
- **The 000052/000053 down-migration notes are corrected.** A failed CHECK validation aborts the
  implicit transaction and rolls back the DROPs, so the table keeps its CHECKs; it is not left
  without one. Incompatible survey rows must be deleted or migrated by SQL before a rollback:
  archiving changes only `status`, and the API cannot change a brief's delivery type or stage.

See [internal/platform/hubspot](../code/internal-platform-hubspot.md).
