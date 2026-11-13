# 2026-11-13 — #585 LinkedIn member interests facet

**Update** — `linkedInConfig.targeting` accepts `interests` (`urn:li:interest:<id>`,
`urn:li:adTargetingFacet:interests`), in an include clause or in `exclude`: LinkedIn's
targeting-criteria facet reference lists it, and it is not on the include-only "Special Note"
list (ageRanges, genders, groups, interfaceLocales). It carries the 100-value cap of the other
facets and, like them, is not gated on `global_marketing_ops` because it names LinkedIn's own
taxonomy, not an ad-account resource. Still behind `LINKEDIN_ADS_TARGETING_ENABLED`.
