# 2026-11-14 — #550 Meta settings readback of regions, cities and the ad schedule

**Creation** — The settings readback could not show the regions, cities (gap 13) and ad schedule
(gap 16) a Meta create now sends: it read only the budget, flight and bid strategy of the ad set.
While `META_TARGETING_READBACK_ENABLED` is `"true"`, the ad set read adds `targeting,adset_schedule`
and three COMPARED fields join the readback: `regions`, `cities` and `ad_schedule`. The recorded
side comes from the row's config snapshot (`metaConfig.targeting.regions` / `cities`, `adSchedule`).
Both sides render through the same functions: keys de-duplicated in numeric order and comma-joined,
a schedule as `days=<d,…> HH:MM-HH:MM <timezone_type>`, and none as `""`, so a create that asked for
no regions matches an ad set that holds none.

Graph omits empty fields, so an absent list on a read targeting object is "none". A list of an
unexpected shape or a non-numeric key is unknown, as is a row without a snapshot. None of these
fails the rest of the read. While the flag is off the readback is exactly as before (seven fields,
no targeting asked for).

Not live-verified: the shape of `targeting.geo_locations` and `adset_schedule` on a read.

Tests: `campaign_settings_targeting_test.go` (the fields asked for and read, the plain read
unchanged, twelve decode shapes) and `meta_settings_targeting_test.go` (a full match, divergence, no
snapshot, unreadable targeting with the rest still read, the flag off, the renderers). Twenty-five
hand mutations each fail a test; two more were equivalent (a null guard and an empty-snapshot guard
that the decoder already covers) and the redundant guards were removed.
