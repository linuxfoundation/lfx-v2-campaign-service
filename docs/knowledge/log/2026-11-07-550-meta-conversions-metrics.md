# 2026-11-07 — #550 conversions in the Meta metrics read

**Update** — The Meta metrics read can now report `conversions` and `conversions_status` under
the #175 rules (gap 6 of the Meta gap review, #550), as Google, LinkedIn and Microsoft already
do.

- It applies only to a campaign this service created with the conversions objective, whose
  stored create result records the conversion event (#559). The read then adds `actions` to the
  Insights fields and reports the value of that event's action type
  (`meta.ConversionActionType`). A row with no matching entry is a measured zero, and a zero
  against zero clicks resolves to `no_conversions`. A duplicate entry or an unreadable value
  fails the read.
- Every other Meta campaign is unchanged: `conversions_status` is `unavailable`, now set
  explicitly, and no `actions` are requested.
- Default-off deployment flag `META_CONVERSIONS_METRICS_ENABLED`; the chart sets `"false"`. The
  change is read-only, but the event-to-action mapping is from Meta's reference and has not been
  checked against a live account.
