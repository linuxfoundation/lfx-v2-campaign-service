# 2026-08-18 — LFXV2-3279 Microsoft Ads geo targeting

**Creation** — `internal/platform/microsoft/geo.go` attaches CAMPAIGN-level location criteria,
closing the last open scope item of LFXV2-3279. Every sibling client (LinkedIn, Meta, Reddit,
Google Ads) already attached geo; Microsoft did not, so a Bing campaign created by this service
targeted everywhere and spent its budget outside the intended region.

**The deferral's constraint held; one of its stated proofs did not.** `targeting.go` argued geo
was impossible without an invented ISO→LocationId table, partly on the claim that the ISO
country table in Microsoft's Geographical Location Codes guide is "explicitly scoped to account
business addresses". Checked against the primary sources that overstated it — the guide says
"In some contexts the API requires a country code string **e.g.**, for the business address of
an `AdvertiserAccount` object", an example rather than a scope limit. Three claims were
CONFIRMED and are what the design rests on: `LocationCriterion.LocationId` is **Add: Required**
and the only Add-writable element (`DisplayName`, `LocationType`, `EnclosedLocationIds` and the
inherited `Type` are all Add: Read-only); the locations file has **no ISO column** (its v2.0
columns are exactly `Location Id`, `Bing Display Name`, `Location Type`, `Replaces`, `Status`,
`AdWords Location Id`); and no documented operation resolves a code or name to a `LocationId`.
`POST /GeoLocationsFileUrl/Query` was confirmed as the current REST spelling. So the CONCLUSION
— the file must be ingested — survived, and the comment has been corrected rather than
inherited.

**No `LocationId` is hardcoded in this repo.** Resolution is ISO-2 → country name → Location Id:
`geo_countries.go` transcribes Microsoft's own published Country Codes table (which yields a
NAME), and the name is matched against the `Bing Display Name` of the file's `Country` rows.
Both halves are Microsoft's. A wrong name fails CLOSED — it matches no row and resolution
refuses — whereas a wrong id would target the wrong country while looking valid, which is why
the fragile half is the one delegated to the file.

**`Status` is enforced during parse.** A `PendingDeprecation` location "is no longer used for
targeting or exclusions" and deprecated criteria cannot be added, so non-`Active` rows are
dropped. Admitting one would recreate the untargeted-campaign harm through the front door.
Columns are located by NAME, never position: Microsoft warns new columns may be added at any
time and that row order is not guaranteed, and a positional parser fails by silently reading
the wrong column.

**Two wire details differ from every other create in this client.** `CriterionType` must be
`Targets`, not `Location` (`Location` is a READ-path value), and the response carries
`NestedPartialErrors` — `BatchErrorCollection` objects each holding their own `BatchErrors` —
not the flat `PartialErrors` used elsewhere. A flat decode sees zero errors and reports a
REJECTED criterion as success: an untargeted campaign reported as targeted.

**Fail closed, before the first mutating call.** Codes are shape-checked offline, then RESOLVED
to numeric ids **before** the campaign is created — not at attach time. Criteria can only be
attached after the campaign exists, so resolving there would leave a campaign with no location
criteria on failure, which Microsoft serves EVERYWHERE; it is PAUSED, but that is one click
from global spend with no signal the targeting went missing. This is the ss#1644 defect shape
(campaign POST at line 303, validation throwing at 334) refused up front instead. Resolution is
all-or-nothing — one unresolvable code fails the whole set rather than returning the ids that
did resolve, since a campaign targeting a subset still spends on a shape nobody approved — and
a rejected attach is an error carrying the campaign id, never a warning, and stops the cascade.

**Caching follows the existing token pattern.** The `FileUrl` is short-lived (~15 minutes,
"you should not depend on a fixed duration") so it is re-fetched every refresh and never
cached; only the parsed map is, under a 24h TTL with a leader/follower single-flight so N
concurrent creates trigger ONE multi-MiB download. The download carries no developer token or
bearer — the URL is pre-signed storage on another host — and the size cap applies to the
DECOMPRESSED stream.

**Testing.** Client-level and dispatch-level tests, each mutation-tested with a compiling revert: reordering resolve after
the campaign create, `Targets`→`Location`, ignoring nested `BatchErrors`, degrading a geo
rejection to a warning, ignoring `Status`, positional column parsing, silently dropping an
unresolvable code, leaking credentials onto the download, disabling the cache, admitting
non-country rows, accepting a short id array, dropping unknown codes, and returning file order
instead of caller order. All 13 were killed. The fail-closed tests assert that NO mutating call
was issued, not merely that an error was returned — a test checking only the error would pass
against the broken ordering. The CSV fixture uses Microsoft's published v2.0 header verbatim
rather than one derived from this package's own column constants, so a drift between the
vendor's format and the parser is a real failure rather than a shared assumption.
