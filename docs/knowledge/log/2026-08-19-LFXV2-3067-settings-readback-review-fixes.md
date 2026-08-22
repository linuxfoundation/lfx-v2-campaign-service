# 2026-08-19 — LFXV2-3067 the settings readback hid removals and discarded a recorded field

**Fix** — three defects in the settings readback, two of them in the two findings the
endpoint most exists to produce, plus a latent one and three stale documents.

## A removed campaign answered 404 instead of reporting the removal

`GetCampaignSettings` built its query as:

    SELECT <fields> FROM campaign WHERE campaign.id = <id>

with no status predicate, while the doc-comment directly above asserted the opposite of
what that means:

    // REMOVED campaigns are NOT excluded here, unlike GetCampaign.

Both halves cannot be true. GAQL excludes removed resources BY DEFAULT unless the status
filter names `REMOVED`, so the absence of a predicate is not neutral — it IS the exclusion,
applied silently by the server. A removed campaign therefore returned zero rows, which this
method reads as a clean absence `(nil, nil)`, which the dispatcher maps to
`ErrPlatformCampaignAbsent` and the API renders as a 404.

That is the worst possible answer for this case. "The campaign you are tracking has been
removed upstream" is the single most actionable divergence this endpoint can report, and it
was being rendered as "no such campaign" — indistinguishable from a bad id. The query now
names all three statuses explicitly, matching the single-quoted enum style and shared
`Status*` constants already used by the two predicates in `campaign_lookup.go` (which filter
in the opposite direction, `status != 'REMOVED'`, so a tombstone cannot be adopted).

The existing test asserted only that the query does NOT contain `status != 'REMOVED'` — a
condition a query with no predicate at all satisfies, which is why it passed throughout. The
stub server returns whatever rows the test handed it and performs no filtering, so it cannot
reproduce GAQL's implicit default; only the query text can be checked. The assertion is now
affirmative on all three statuses.

## `advertising_channel_type` discarded the side it had

The readback passed `nil` as the recorded value, filing the field with the four genuine
upstream-only observations. It does not belong there: `googleAdsConfig.Channel` is marshalled
whole into `ConfigSnapshot` by `applyCampaignConfig` on BOTH the create and the adoption path,
so the row records which channel was asked for. `googleAdsVariantForChannelType` already
mapped Google's enum in the other direction.

The consequence is that a campaign dispatched as `demand-gen` but running upstream as
`SEARCH` — a real misconfiguration, and one an operator cannot see any other way — was
permanently `unknown`. The finding could not be produced at all.

`googleAdsRecordedChannelType` now decodes the snapshot and expresses the recorded channel in
Google's own vocabulary, because that is what an operator recognises from the Google Ads UI
and because mapping the other way would need an invented slot for `PERFORMANCE_MAX`. An
ABSENT channel maps to `SEARCH`, matching what `googleAdsConfig.Channel` documents on itself:
every caller predating the field omits it and they all mean Search. `nil` is still the answer
where nothing interpretable was recorded — no snapshot, a snapshot that is not this adapter's
JSON object, or a channel outside the closed set this service creates — because claiming
`diverged` from an uninterpretable recorded value is a fabricated finding, not an observed one.
The `SEARCH`/`DEMAND_GEN` spellings are now shared constants used by both directions so the
two mappings cannot drift apart.

The test that asserted the old behaviour listed channel type among the fields where "no row
column expresses this". It has been rewritten: that claim is now false for channel type, and
the field is covered by three new tests instead (diverged, match including the absent-channel
case, and the three uninterpretable cases).

## Trimming before validation could fabricate a match (latent)

`trimmedOrNil` — now `blankToNil` — returned the TRIMMED value. It runs on the decode step,
upstream of every consumer that validates these strings, so it handed them well-formed values
the platform never sent. `googleAdsDateOnly` parses with a strict layout and passes an
unparseable value through WHOLE, so `"2026-08-01 "` trimmed to `"2026-08-01"` is byte-equal to
a recorded `YYYY-MM-DD` and reports `match` for a value that never parsed. `" DAILY "` has the
same shape through `googleAdsBudgetTypeFromPeriod`.

**This is currently LATENT**: `recordedStart` and `recordedEnd` are always nil for Google Ads
today, because `googleAdsConfig` carries no dates, so the compared date fields read `unknown`
and no match can be fabricated yet. It is fixed anyway because the code comment beside those
fields explicitly anticipates a future config populating them — the comparison is wired
through precisely so that config starts diverging without anyone having to remember, and this
defect would have made it start MATCHING instead.

`blankToNil` still maps whitespace-only to absence: there is no value under the whitespace to
preserve, so that withholds a comparison rather than manufacturing one. The renaming is the
point — the old name described the behaviour that was wrong.

The direct `TestGoogleAdsDateOnly` already covered `"2026-08-01 "`, and passed the whole time,
because it calls the helper with a literal and never exercises the decode step that mangles it.
The guard is pinned at the client layer instead.

**Verification** — three mutations, each compiling and each reverted:

- Removing the status predicate (`WHERE campaign.id = <id>` alone) fails
  `TestGetCampaignSettings_RemovedCampaignIsReported` on all three statuses, printing the
  predicate-free query.
- Passing `nil` as the recorded channel type fails
  `TestGoogleAds_ReadSettings_RecordedChannelTypeDiverges` and both subtests of
  `..._RecordedChannelTypeMatches`:

      channel type Recorded = nil; googleAdsConfig.Channel IS persisted in ConfigSnapshot on
      both the create and the adoption path, so discarding it makes an upstream/recorded
      channel mismatch unreportable

- Restoring the trim in `blankToNil` fails
  `TestGetCampaignSettings_MalformedValuesSurviveDecodeVerbatim`, reproducing both fabricated
  values exactly:

      startDateTime = "2026-08-01", want "2026-08-01 " VERBATIM
      period = "DAILY", want " DAILY " VERBATIM

**Docs** — four corrections, all from suppressed review comments.

`docs/api-catalog.md` advertised a **409 for unknown provenance** that cannot be observed.
`ReadSettings` guards on `created != "" && created != <current>`, so an empty provenance
proceeds to the read, and Google Ads is the only `SettingsReader`. The CATALOG is the side
that changed: the empty-provenance pass-through is a deliberate cross-path convention shared
with the metrics read and the status toggle — a row written before its adapter stamped the
account is waved through as "unknown" rather than being made unreadable until a re-dispatch —
and narrowing it here would leave Google Ads inconsistent with itself. The catalog now
describes the actual behaviour and says why, including that the residual risk is bounded by
this endpoint reporting rather than writing back.

The catalog and the `unknown_count` log fragment both **double-counted `status`**, describing
"the five upstream-only fields plus `status` plus the two flight dates" as eight when the code
produces fewer: `status` is itself one of the upstream-only fields. Both are reworded to name
`status` once. The arithmetic also MOVED with the channel-type fix in this same change, and
was re-measured rather than re-reasoned: a row whose `config_snapshot` records a channel now
measures `unknown=6`, and only a legacy row without one still measures `7`. The floor is a
range, not a constant — which if anything strengthens the original point, since a floor an
operator cannot predict from the response is even less distinguishable from a real failure.
The compared/upstream-only lists in both the catalog and `internal-dispatch.md` now put
`advertising_channel_type` on the compared side where it belongs.

`internal-dispatch.md` claimed "**Both sides are rendered to two decimals by one helper**".
That is stale and contradicts `googleAdsUpstreamBudgetAmount`, whose own comment calls the
exception "the whole point of this function's care": two decimals is applied only when the
upstream value is a whole number of cents, and a SUB-CENT budget is rendered at FULL precision
so that rounding `10.004` to `"10.00"` cannot report `match` against a recorded `10.00`.
