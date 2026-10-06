# 2026-10-05 — Meta truncated error envelope keeps its structured blame

**Fix** — Follow-up to #269 (copilot). The Meta client's truncated-body path (a complete Graph
error envelope followed by a connection closed on a mismatched `Content-Length`) copied only
`type`, `code` and `fbtrace_id` onto the `APIError`, while the normal non-2xx path also copied
`error_subcode` and the bounded `error_data.blame_field_specs`. A 400 blaming `bid_amount` that
arrived truncated therefore lost its structured classification and the bid lever answered the
generic 503 instead of the documented amount-rejection 400. Both paths now go through one helper,
`(*APIError).copyEnvelope`, so neither can drop a field the other keeps. Pinned by
`TestUpdateAdSetBid_TruncatedEnvelopeKeepsBlame`, which fails with the old copy.
