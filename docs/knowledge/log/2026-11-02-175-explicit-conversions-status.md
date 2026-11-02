# 2026-11-02 — 175: campaign metrics report an explicit conversions status

**Fix** — a nil `conversions` meant two things: the channel cannot report a campaign-level count,
or the read came back incomplete. A LinkedIn campaign with one element that omitted
`externalWebsiteConversions` looked exactly like Meta. `model.CampaignMetrics` now carries
`ConversionsStatus`, and both metrics routes return a REQUIRED `conversions_status`:

- `reported` is the ONLY status that carries `conversions`;
- `unavailable` is a channel or account that reports no count;
- `incomplete` means part of the response omitted the metric;
- `no_conversions` means the window had no clicks and no conversion was reported.

LinkedIn sets all four. Its answered empty window, and a complete zero-click and zero-conversion
response, are `no_conversions` with no total; before, they were a measured `0`. Microsoft sets
`unavailable` when the column is missing or every cell is blank, and `incomplete` when only some
cells are blank. The other adapters are classified by `ResolvedConversionsStatus`: a total is
`reported`, no total is `unavailable`.

The brief service publishes, and passes the rules, only `ReportedConversions()`. So the
`no_conversions` action item never fires on a non-reported status, and #357's click floor is kept.
`zero_delivery` remains the finding for a window with no delivery.

Follow-up: self-serve does not yet show `conversions_status`.
