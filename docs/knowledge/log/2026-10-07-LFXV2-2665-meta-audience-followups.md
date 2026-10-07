# 2026-10-07 — LFXV2-2665 Meta audience read: #283 late review follow-ups

**Fix** — Four late bot findings on the merged Meta audience read (`get-meta-ads-audience`):

- **Page-level duplicate keys (Greptile P1).** The duplicate-key guard covered row keys only. A
  page repeating `data` (or `data` + `Data`) decoded to the LATER value before rows were
  validated, so a populated audience followed by `"data":[]` read as a successful EMPTY answer.
  A repeated `paging` could end the walk early. Each page is now fetched raw and passed through
  `identityjson.Check` before decoding. Any key repeated at any level, exact or case-folded
  (including KELVIN SIGN and LONG S), now fails the read with 503 and no partial rows.
- **Linear guard (Copilot).** The row-level `rejectDuplicateKeys` used a pairwise `EqualFold`
  scan, which was quadratic in keys on a body of up to 10 MiB. It is deleted in favour of
  `identityjson.Check`'s single map-based pass. A 50,000-key row now completes in about 0.3s
  under `-race`.
- **Explicit null counters (Copilot).** Plain string fields decoded `"impressions": null` as ""
  and published an authoritative 0. The counters are now decoded raw. An ABSENT counter is still
  a measured 0, matching the metrics read. A PRESENT null or non-string counter fails the read.
  Nulls in `campaign_id`, `account_currency` and the breakdown values were already refused, and
  tests now pin that.
- **Impossible OpenAPI examples (Cursor).** Type-level `Example()`s were added to
  `MetaAdsAudienceBucket` (one bucket per dimension, own fields only) and to `MetaAdsAudience`
  (`bucket_count` == `len(buckets)`), then `make apigen` was run. A test pins the generated
  spec.
