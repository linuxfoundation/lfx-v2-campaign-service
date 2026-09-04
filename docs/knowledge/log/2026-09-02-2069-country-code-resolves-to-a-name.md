# 2026-09-02 — linuxfoundation/lfx-self-serve#2069: the UI writes countryCode, the audience build needed country

**Fix** — `decodeEventDetails` required `country` and deliberately refused `countryCode`, so EVERY
brief created through the campaigns UI failed with "no country in its details" and could never
build an audience. Observed on `app.lfx.dev` and reproduced locally.

The refusal was correct, and the comment explaining it said why: `Country` reaches HubSpot as an
exact `CONTAINS`/`IS_ANY_OF` filter value, so a literal `CN` matches no contact and the build would
SUCCEED while storing an empty inclusion list — a silent wrong answer on the list that decides who
receives an email. What was missing was not the fallback but the MAPPING, and the comment said that
too: "reading the code therefore needs an ISO-2 → country-name mapping first".

`audience.CountryForCode` is that mapping. It covers exactly the countries `countryToRegion` knows
(36 of each, verified 1:1), so an unmapped code still fails loudly rather than becoming a filter
value that matches nobody. An explicit `country` still wins: a name the brief states is more
trustworthy than one derived from two letters.

`GB` is the ISO code; `UK` is accepted too because it is what people type.

Both guards are mutation-confirmed. Passing the raw code through fails two decoder tests, and
`TestISO2MappingMatchesRegionCountriesExactly` catches drift in both directions — a country added
to the region map without a code is invisible to every UI brief, and a code pointing at a name the
region map lacks is the empty-list bug returning by another route.
