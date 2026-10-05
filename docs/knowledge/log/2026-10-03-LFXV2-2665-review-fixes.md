# 2026-10-03 — Review fixes: kind-aware validation, Demand Gen bid refusal, currency-blind bid ceiling

**Fix** — Three defects found by the pre-PR review of the LFXV2-2665 campaign
completeness work, all of them created or exposed by that change rather than
pre-existing.

**1. Validation ran as Search for every channel
(`internal/dispatch/googleads.go`, `internal/platform/googleads/campaign.go`).**
`preflightCampaignKind` used to take its `kind` only to compose a name, so
`ValidateCampaignInput` hardcoding `campaignKindSearch` was harmless. The
completeness slices made the kind GATE refusals — proximity, the four criteria
kinds, extension assets, the ad-group list — and the dispatcher was still
validating every request as Search, before the channel was even resolved.

On the create path that was only a late error: `CreateDemandGenCampaign` runs
its own preflight with the right kind and refuses. On the ADOPTION path, which
returns before any create runs, it was the real defect — a Demand Gen request
carrying Search-only fields was accepted and snapshotted config that is never
applied. That is precisely the accepted-when-a-campaign-happens-to-exist
asymmetry the unconditional validate call exists to prevent, reinstated for the
new fields.

Fixed by adding `ValidateCampaignInputKind(kind, in)` and moving the
dispatcher's channel switch ABOVE the validate call so the resolved kind is
available to it. The switch is pure-local and contacts nothing, so the
no-upstream-call guarantee is intact. `ValidateCampaignInput` remains, assuming
Search, for callers that have no kind. An unknown kind is deliberately not
rejected: it gates nothing and falls through to the un-restricted Search
treatment, which cannot refuse a create Google would have accepted.

**2. `maxCPCBid` was currency-blind
(`internal/platform/googleads/adgroup_ad.go`).** The ceiling was `1_000.0`,
copied from the Microsoft client's window, checked against a bid the file's own
comment says is in the ad ACCOUNT's currency and is never converted. That reads
as generous in dollars and refuses ordinary bids in the zero-decimal and
low-unit currencies an account can be opened in — 1000 JPY is under $7, 2000
KRW under $2 — so the guard whose comment names over-refusal as the worse
failure was committing it. Raised to `100_000.0`, which still catches the
micros-for-units mistake it exists for (a micros-shaped bid starts at
`1_000_000` for one unit) while clearing any real bid in any currency. The
Microsoft window is unchanged; the two numbers are no longer the same, and that
is deliberate.

**3. A CPC bid was silently dropped on Demand Gen
(`internal/platform/googleads/campaign.go`).** `CPCBid` was validated for both
kinds, but `demandGenAdGroupCreate` has no `cpcBidMicros` field and
`demandgen.go` never reads `pf.cpcBidMicros`, so a bid supplied on that channel
was converted and discarded with a successful create and no signal — the same
defect LFXV2-3283 fixed for geo, and the only Search-only input in this feature
set that was dropped rather than refused. Now refused in the preflight alongside
the others. The refusal keys on a SUPPLIED bid, not on the channel, so the unset
shape every existing Demand Gen caller sends still validates, and it costs
nothing upstream: Demand Gen bids via `targetSpend` and rejects `manualCpc`.

`docs/api-catalog.md`'s `cpcBid` entry carried both stale facts — the `1000`
ceiling and a "SEARCH only" line that never said whether that meant refused or
ignored — and is corrected with them, since `CreateCampaigns.config` is typed
`Any` in `design/` and the catalog is the consumer-facing validation contract.
