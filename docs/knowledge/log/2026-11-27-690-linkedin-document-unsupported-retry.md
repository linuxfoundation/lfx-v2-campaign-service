# 2026-11-27 — #690 LinkedIn: a document retry reuses its own campaign while its format reads UNSUPPORTED

**Fix** — found by the #683 live paused probe on LF Events. Before any creative is attached,
LinkedIn reports a document campaign's format as `UNSUPPORTED`. `findCampaignByNameInGroup` mapped
that to the image kind and refused to reuse the campaign for a document create, so a retry after a
partial failure (campaign created, dark post or creative failed) aborted with "already exists …
with format UNSUPPORTED".

- `UNSUPPORTED` (`campaignFormatUnsupported`) is now treated like an unreported format in the
  format check: it cannot contradict the requested format. Image campaigns were not affected, and
  video and carousel campaigns set their format at create.

Tests: `TestFindCampaignByNameInGroup_DocumentIsItsOwnFormat` gains a row where a document retry
finds its own campaign with `format: UNSUPPORTED` and reuses it.
