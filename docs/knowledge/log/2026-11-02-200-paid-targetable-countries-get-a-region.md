# 2026-11-02 — 200: the six paid-targetable countries can build an email audience

**Fix** — Austria, Belgium, Czech Republic, Denmark, Finland and Norway were in
`googleads.geoTargetConstants` but not in `audience.countryToRegion` (nor `iso2ToCountry`), so a
brief in any of them could run paid ads while its email audience build failed with no region.
All six are now EMEA in both maps; `Czechia` is an alias whose HubSpot display value is
`Czech Republic`. Because group 7 (region-wide registrants) is built from `CountriesIn`, this
also widens group 7 for every EMEA brief rebuilt after the change.

`TestEveryGeoTargetCountryHasARegion` (googleads package) asserts every ISO code in the Google
map resolves through `CountryForCode` and `RegionFor`, with an explicit, reasoned exception list
that is empty today. With the region map reverted it fails naming the six codes.
`TestSixPaidTargetableCountriesAreEMEA` pins the region and the exact display value of each.
