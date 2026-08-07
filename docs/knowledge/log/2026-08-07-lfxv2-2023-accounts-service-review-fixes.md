# 2026-08-07 — LFXV2-2023: account-discovery service-layer review fixes

**Update** — Review round on the account-discovery service layer: one status-mapping correction,
one knowledge-bundle structural fix, and one comment that documented a failure mode that cannot
occur.

**Fix** — The 400 arm keys on the sentinel alone, not on which layer raised it. Two of the five
sub-cases now originate in `credsSource.resolve` (empty credential blob, undecryptable blob) rather
than in `resolveGoogleAdsDiscoveryClient`, and reach this switch unchanged. Listing them separately
is the point: an arm that happened to work only for the resolver's own errors would pass a test
covering only those three.

**Fix** — The handler's `default` arm answered 503 for failures that had nothing to do with
Google. `ListGoogleAdsAccounts` now maps `domain.ErrConnectionNotUsable` to 400 — an inactive
connection, an incomplete credential blob, or a dashed stored `login_customer_id`. The sentinel
and the wrapping landed in the dispatch PR below this one, because that is the only layer that
still knows the failure happened before any request existed; by the time an error reaches here, a
setup failure and an upstream failure are the same type. The cause is logged, not returned: one of
the wrapped errors comes from `json.Unmarshal` over the DECRYPTED credential blob, and an
unmarshal error can quote its input, so echoing it would put credential bytes in a response body.

**Fix** — The new `## Account discovery` heading in `internal-service.md` was inserted ABOVE the
metrics section's closing prose, silently re-parenting two paragraphs about `MetricsWindow` and
`defaultMetricsWindowFor` under account discovery — where they describe a `window` parameter this
endpoint does not have. The section moved below them. This is the failure mode of appending a
heading by line number rather than by section boundary, and nothing catches it: the file is valid
Markdown, `okfvalidate` passes, and the prose reads fine until you notice which endpoint it is
supposedly about.

**Fix** — `ReadAccounts`'s nil-result comment claimed the conversion would panic. It would not:
`len` and `range` are nil-safe, and the handler would have produced a cheerful empty `[]`. That is
the actual hazard, and it is worse than a panic — the operator sees an empty account picker with
no error and goes looking for a permissions problem at the provider that does not exist. The
comment now states the real purpose: keeping "zero accounts" distinguishable from "no answer".
