# 2026-11-13 — #582 LinkedIn website conversions with automatic bidding

**Update** — `linkedInConfig.objective` (`"traffic"` | `"conversions"`) lets a create choose
LinkedIn's Website conversions objective with Maximum delivery, the setup most of LF's 2025–26
LinkedIn campaigns use. Before, automatic bidding always created `WEBSITE_VISIT` / `MAX_CLICK`.

- `conversions` + automatic: `objectiveType` `WEBSITE_CONVERSION`, `optimizationTargetType`
  `MAX_CONVERSION`, `costType` `CPM`, no `unitCost`, name segment `Conversions`.
- `conversions` + manual: the manual create unchanged (`NONE`, `CPC`, `unitCost`).
- `traffic`, or omitted: unchanged; `traffic` with manual is a 400.
- Decision: automatic `conversions` requires a non-empty `linkedInConfig.conversions` (400
  otherwise, naming `LINKEDIN_ADS_CONVERSIONS_ENABLED` when it is off), because LinkedIn will not
  activate a MAX_CONVERSION campaign without an associated conversion. It inherits the conversions
  flag and the `global_marketing_ops` gate. A failed association leaves the PAUSED campaign
  `created_degraded`, as #552 does.
- The by-name reuse lookup also compares the bidding mode, since both conversions shapes share a
  name. The bid lever answers 409 for MAX_CONVERSION. The settings readback reads `objective` from
  the config snapshot.

Built from the LinkedIn Marketing API docs (li-lms-2026-02, the pinned `202602`). Not yet probed
live.
