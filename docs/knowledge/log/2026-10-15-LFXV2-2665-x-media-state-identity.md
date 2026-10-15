# 2026-10-15 — X media state is read only from a body identityjson accepts

**Fix** — a late review thread on #320 (X media follow-up to #311).

- `decodeMediaState` decoded `resp.Data` without first checking the raw body. The envelope is
  decoded last-wins, so a finalize or STATUS body carrying a duplicated `data` key (for example
  `"data":null` or a failed `processing_info` beside a ready object) could still read as ready,
  and the create would attach media X never confirmed.
- It now runs `identityjson.Check` on `resp.raw` first, as the campaign lookup and audience
  reads already do. A refused body is an unknown media state, reported UNCONFIRMED before the
  library add and the campaign create.
- `TestCreateCampaign_ImageMediaStateMustBeReported` gains a duplicated-key case for finalize
  and for STATUS; both fail without the check.
- Concept updated: `architecture/x-creative-assets.md` (Readiness).
