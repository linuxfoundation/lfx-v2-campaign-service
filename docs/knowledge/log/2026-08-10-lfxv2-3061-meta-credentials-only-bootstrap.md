# 2026-08-10 — Meta's connection bootstrap, and the helper that only checks half of it

**Update** — `MetaAdsConnectionConfig` no longer declares `Required("account_id")`, so a Meta
connection can be created with credentials alone and have its ad account chosen afterwards:

```
POST   /projects/{id}/connection-meta-ads          (credentials + page_id, no account_id)
GET    /projects/{id}/connection-meta-ads/accounts (discovery, LFXV2-3062)
PUT    /projects/{id}/connection-meta-ads          (set the chosen account_id)
```

`page_id` stays `Required`. It names a Facebook page the operator already controls — nothing
about the token's reachable-account list resolves it, so relaxing it would just move the same
"you'll find out at dispatch time" problem the Google Ads entry describes onto a field discovery
can never help with.

## Why this doesn't repeat the "ship together or not at all" mistake

The Google Ads bootstrap entry (`2026-08-08-lfxv2-2023-connection-bootstrap.md`) states the rule
this repo learned the hard way: relaxing `account_id` without a discovery endpoint produces a
connection that cannot be finished from inside the API, and the only thing gained is a
half-configured row. Read narrowly, LFXV2-3061 looks like it might repeat that: LFXV2-3062 (the
Meta account-picker endpoint) was still open, CI-green, `REVIEW_REQUIRED`, not merged, at the
time this branched.

The distinction that matters: LFXV2-3062 was built as PR 1 of this same two-ticket sequence,
specifically so the completion path would exist before this PR needed it — not discovered
missing after the fact. And independent of merge order, every connection provider already gets a
generic `PUT /connection-{provider}` from `connectionMethods` (`design/connection.go`), which
lets an operator set `account_id` by hand the moment they have one, whether or not the discovery
endpoint has cleared review yet. So the state this PR creates was always completable from inside
the API; LFXV2-3062 makes completing it self-service instead of requiring someone to already
have the id in hand — which is exactly what Google Ads' own discovery endpoint does, no more.

## The helper covers credential state; it deliberately does not cover the account

`resolveMetaCredentials` replaces three inlined copies of the same credential-state check
(active status, decodable JSON, non-empty access token) at `Dispatch`, `ToggleStatus`, and
`ReadMetrics` — the same shape as `resolveRedditClient`, including the named-return
`defer func() { err = res.systemScoped(err) }()` so every return path is scoped without needing
to remember it individually. It does NOT check `account_id`. That split is deliberate, not an
oversight: `Dispatch` needs both `account_id` and `page_id` to create a campaign, but
`ToggleStatus` and `ReadMetrics` target an existing campaign by id and do not require the account id — they need only the credentials. Folding the account check into the credentials helper would make every caller pay for
Dispatch's extra requirement. `requireMetaAccountID` is the second, separately-called helper —
same wrapping shape as Google Ads' equivalent: `domain.ErrConnectionNotUsable` picks the status,
`domain.ErrAccountNotSelected` supplies the `account_not_selected` reason token, matched ahead of
the general unusable-connection arm in `unusableConnectionReason`.

## The system-account installer is the other surface, and it had its own gate

Dropping `Required("account_id")` relaxes the HTTP surface. `bootstrap-system-account` writes
PAST that surface, straight to the repository, so it carries its own rule —
`accountDiscoveryProviders` in `internal/bootstrap/sysacct.go` — and Meta was excluded from it.
That exclusion was correct at the time, and its comment named this ticket as the gate: discovery
alone is half a lifecycle, and Meta's `Dispatch` still answered an empty account id with a
generic error, so an operator holding an account-less Meta system row was told nothing about what
was missing or where to find it.

`requireMetaAccountID` is that second half, so Meta joins the map here. Leaving it out would have
shipped a branch whose design comment and API catalog both say Meta can be created
credentials-first while the one tool that installs the LF's own credentials refuses to.

The bar for the next provider is now stated as both halves together, because Meta is the only
provider where they ever came apart, and this map is exactly where someone would be tempted to
add a provider on the strength of a discovery endpoint alone.

Two tests used Meta as the exemplar of "no discovery, so `-account-id` is required"
(`TestInstallRequiresAnAccountIDWhereNothingCanSupplyOneLater`,
`TestClearAccountIDIsRefusedWhereNothingCanSupplyOneLater`). They now use LinkedIn, which has
neither half, and the first gained a Meta case asserting the OPPOSITE — that an account-less Meta
install is accepted and writes an empty `AccountID` for the picker to fill. Verified by mutation:
removing Meta from the map fails that case with the `-account-id` refusal.

## What changed to make `*string` build again

Goa's codegen makes a dropped-from-`Required` attribute a pointer on the generated request-body
type. `internal/service/connection.go`'s `CreateMetaAds`/`UpdateMetaAds` assigned
`cfg.AccountID` directly into the domain model's plain `string` field; both now go through
`strVal(cfg.AccountID)`, the same helper Google Ads' equivalent call sites already use. `PUT` is
a full replace, so omitting `account_id` on update clears a previously chosen one — same
semantics as Google Ads, and the second half of the bootstrap: the caller PUTs back the id
chosen from discovery.

## Why the transport-level test is the one that actually pins this

A service-level test proves the domain model accepts an empty `AccountID`; it cannot prove the
HTTP layer will let one through, because Goa's generated request-body validator runs BEFORE the
handler. If `Required("account_id")` were mistakenly re-added to the design, that validator
would reject the request before the service is ever reached, and a service-level test would keep
passing while the bootstrap silently broke. `TestValidateCreateMetaAds_AccountIDIsOptionalAtTheTransport`
calls `connsrv.ValidateCreateMetaAdsRequestBody` directly — the same pattern
`TestValidateCreateGoogleAds_AccountIDIsOptionalAtTheTransport` already established — with a
second sub-test asserting `page_id` is still required, so the two attributes' presence checks
can't drift together undetected.
