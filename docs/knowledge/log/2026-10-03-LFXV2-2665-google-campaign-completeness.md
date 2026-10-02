# 2026-10-03 — Google Ads Search campaign completeness: geo depth, targeting criteria, extensions, multiple ad groups

**Update** — Four further slices on the Google Ads create cascade, each
optional, each a no-op when absent, and each validated inside
`preflightCampaignKind` before the first budget `:mutate` — so no bad local
input can orphan a paid resource, and `ValidateCampaignInput` refuses on the
adoption path exactly what the create path refuses. A config naming none of
these fields produces byte-for-byte the campaign it produced before they
existed.

**1. Geo depth** (`internal/platform/googleads/geo.go`). Three additions to the
country-code targeting LFXV2-3283 introduced.

`GeoTargets` now also accepts a RAW numeric geo target constant id, which is the
only way to address a city, region, metro or postal code — a metro-level event
campaign cannot be expressed in country codes, and curating those locations here
would mean shipping ~100k rows Google revises. `resolveGeoList` tells the two
spellings apart by SHAPE (two letters versus all digits), so they cannot collide
and no caller declares which kind an entry is. A numeric id is checked for shape
ONLY: verifying it would mean a lookup, which `ValidateCampaignInput`'s contract
forbids, so a well-formed id naming nothing is refused by Google AFTER the
campaign exists. Country codes are still checked locally and still fail before
any mutate.

`ExcludedGeoTargets` is the exclusion list, sharing `resolveGeoList` with a
different noun so a rejection names the list an operator has to fix —
`validateExcludedGeoTargets` is its own entry point for the same reason
`validateNegativeKeywords` is. A location in BOTH lists is refused: Google
resolves that contradiction by letting the exclusion win, so the campaign would
silently not serve where the caller plainly asked it to. Exclusions apply on
BOTH channels, so `maxGeoTargets` bounds each list separately.

`ProximityTargets` is radius targeting — the one location shape that is not a
place in Google's table, hence a struct rather than another geo-list entry.
Decimal degrees in, microdegrees out. `RadiusUnit` is required with no default:
a radius of 50 is two very different campaigns depending on the unit, and
guessing would silently buy ~2.5x or 0.4x the intended area. REFUSED on Demand
Gen rather than dropped, because that channel attaches location criteria on the
ad group where proximity is unverified against a real account; lift it when
someone confirms the behaviour live.

`maxGeoTargets` rose 30 → 60. The old value carried the note that the country
map's 30 entries made it unreachable without duplicates — which stopped being
true the moment raw ids were accepted. Sized by the 2026-08-13 precedent: a cap
the product's own callers exceed by default refuses creates Google would have
accepted.

**2. Language, ad schedule, device and demographic criteria**
(`internal/platform/googleads/campaign_criteria.go`). Four more campaign-level
criterion kinds, built by `validateCriteriaPlan` and posted in ONE
`campaignCriteria:mutate` kept separate from the geo and negative-keyword calls
— for the reason the negatives slice gives: a shared mutate would make either
list's failure discard the other, and each call's failure sentence would then be
false for the other's contents.

`Languages` resolve ISO 639-1 codes or raw numeric constant ids by the same
shape test the geo list uses. `AdSchedules` carry Google's enum-valued minutes
(`ZERO`/`FIFTEEN`/`THIRTY`/`FORTY_FIVE`), an end hour reaching 24 only at minute
0, and an end strictly after the start — with deliberately NO overlap check,
since Google rejects a true overlap itself and a local test would have to decide
whether 09:00-12:00 and 12:00-17:00 touch (they do not), a wrong answer refusing
an ordinary split-day schedule. `DeviceBidModifiers` refuse a repeated device,
which Google rejects as a criterion conflict only after the campaign exists.
`ExcludedAgeRanges`/`ExcludedGenders` are exclusions by construction, because
Google targets demographics by excluding the buckets you do not want.

`AdSchedule.BidModifier` is a `*float64` here AND on the dispatcher's wire type,
and the pointer is load-bearing: exactly `0` is Google's -100% opt-out, so it
cannot double as "unset", and a value-typed hop anywhere along the path would
turn an absent modifier into an instruction not to serve.
`DeviceBidModifier.BidModifier` is a plain `float64`, because a device listed
with no modifier would say nothing at all. All five are refused on Demand Gen,
for the reason proximity is.

**3. Ad extensions** (`internal/platform/googleads/assets.go`). Sitelinks,
callouts and structured snippets are created by `assets:mutate` and then LINKED
to the campaign by `campaignAssets:mutate` with an `AssetFieldType`. The link
references the resource name the create RETURNED, never one rebuilt from an id —
a rebuilt name that happened to be wrong would link a different asset and the
mutate would still succeed. Both calls verify one result per operation and that
each returned name parses to this account and this campaign.

Text limits are RUNE counts, not the double-width WEIGHT `ad_copy.go` uses:
Google applies the same CJK doubling to extension text, so a 25-rune Japanese
sitelink is refused upstream. Deliberate under-refusal — counting weight locally
would refuse mixed-script text Google might accept. Over-long extension text is
REFUSED rather than truncated, inverting the RSA path, because generated copy may
be cut (this service wrote it) while extension text was written by a human for a
reason. A sitelink needs both description lines or neither, since Google renders
one line as if it had none. A snippet needs 3..10 distinct values, below which
Google will not serve it, and its header is shape-checked only — the valid header
vocabulary is LANGUAGE-DEPENDENT and Google revises it, so a local allow-list
would refuse headers Google accepts. Search only.

**4. Multiple ad groups and multiple RSAs**
(`internal/platform/googleads/adgroup_plan.go`). `CampaignInput.AdGroups` turns
the single-group cascade into one group per theme, each with up to 3 responsive
search ads. `validateAdGroupPlans` takes the group the campaign-level fields
already produced as its `base` and uses it as BOTH the no-`AdGroups` answer and
the source of every per-group fallback — which is what keeps "inherit" meaning
exactly what the single-group path would have done rather than a second set of
defaults that could drift.

Inheritance is PER FIELD: a group setting only `Keywords` keeps the campaign's
bid, audiences and copy, and `CPCBid: 0` inherits because 0 already means
"unset" at the campaign level. Each override runs through the same validator as
its campaign-level counterpart, so a group's vocabulary cannot drift from the
campaign's. Group names are theme labels appended to the composed campaign name,
with duplicates refused CASE-INSENSITIVELY because Google's own check is and a
collision surfaces at the mutate, by which point earlier groups already exist.
Duplicate ad COPY across two ads in one group is NOT refused — Google accepts it,
it is wasteful rather than invalid, and a guard upstream would not apply is the
expensive kind of wrong. Groups are created in order, under the usual
partial-result contract: a failure at group N returns the error alongside the
non-nil result and says which group of how many failed with how many preceded it.

**5. Dispatcher wiring** (`internal/dispatch/googleads.go`). Twelve new
`googleAdsConfig` fields and seven JSON-tagged wire types carry all of the above
through the API, none with `omitempty` per the channel-recognisability rule. Each
mapper returns nil for an empty input, as `googleAdsKeywords` does, so an omitted
field stays nil end-to-end — and for the per-group fields nil is specifically
what the client reads as "inherit". The Search-only channel rules are
deliberately NOT restated in the adapter: the client refuses them in its own
preflight for both the create and the adoption path, and a second copy could only
drift. `docs/api-catalog.md` documents every new key field by field, since
`CreateCampaigns.config` is typed `Any` in `design/` and the catalog is therefore
the consumer-facing validation contract.
