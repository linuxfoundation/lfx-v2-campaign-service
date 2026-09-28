# 2026-09-29 — A2: two concept files still called Reddit's reporting contract undocumented

**Fix** — LFXV2-3282 replaced the guessed Reddit v3 reporting shape with Reddit's official public
OpenAPI spec, and the 2026-09-26 entry in this log said the concept files were already current and
`pkg/constants` was the only place left describing the superseded state. That was wrong on both
counts. Two concept files still asserted the retired reason:

- `docs/knowledge/code/internal-dispatch.md`'s conversion-reporting table listed Reddit as
  **Unknown**, because "the v3 reporting contract has no public documentation at all".
- `docs/knowledge/code/internal-service-rules.md` explained the `no_conversions` rule's platform
  gate with "Reddit's reporting contract is undocumented".

Both now state the actual reason Reddit carries no conversion measurement, which is narrower and
survives the spec being published: **this service's report request never names a conversion field.**
`internal/platform/reddit/metrics.go` asks for `CAMPAIGN_ID`, `IMPRESSIONS`, `CLICKS` and `SPEND`
and nothing else, so `Conversions` stays nil on both `model.CampaignMetrics` and
`model.AccountCampaignMetrics` — the same nil that
[linuxfoundation/lfx-self-serve#3020](https://github.com/linuxfoundation/lfx-self-serve/issues/3020)
replaced the BFF's hardcoded literal `0` with. Nothing about the rule's behaviour changes; only the
reason given for it.

The 2026-09-26 entry's own "only place left" sentence is corrected in place to point here, rather
than leaving a reader to discover that the claim did not hold.

Still carrying the older framing, and deliberately left for its own change rather than swept into
this PR: `internal/platform/reddit/metrics.go:130` explains the nil `Conversions` by citing an
"UNVERIFIED-CONTRACT banner" and calling the three requested fields inferences from this package's
other endpoints. The fields come from the spec now. The conclusion there is still right — naming a
conversions field would be a guess, and a guess that decodes to zero is indistinguishable from a
campaign that converted nothing — but the premise it is argued from has moved.

Refs: LFXV2-2665
