# 2026-10-07 — Adoption reports absence only when proven

**Fix** — four bot threads on #280. The rule now applied on all four adopters: `404` only when the
platform has PROVEN the campaign is absent; anything that could be an access, auth or account
problem is the `503` "could not be verified" arm, because an ambiguous `404` can lead an operator to
create a duplicate of a live campaign. Meta's Graph code 100/subcode 33 ("does not exist OR cannot
be loaded due to missing permissions") is no longer an absence on any status; only a returned
`DELETED`/`ARCHIVED` status is. A Reddit or X campaign `404` is followed by ONE confirming read of
the connection's own ad account (`confirmAccountReadable`) and is absent only if that answers 2xx
naming the account. Microsoft's adoption read now shares the monitor's documented, space-delimited
eight-value `allCampaignTypes` (ObjectiveBased included) instead of an incomplete comma list, so any
non-Search campaign gets the definite 409. See [internal/dispatch](../code/internal-dispatch.md) and
the [API catalog](../architecture/api-catalog.md).
