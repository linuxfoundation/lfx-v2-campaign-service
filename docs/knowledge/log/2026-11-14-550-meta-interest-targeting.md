# 2026-11-14 — #550 Meta interest targeting (core of gap 7)

**Creation** — The core of gap 7 of the Meta gap review (#550). A Meta ad set could not prospect by
interest. `metaConfig.targeting.interests` (numeric detailed-targeting interest ids, at most 25) is
sent as one `flexible_spec` entry, so a person matching any listed interest qualifies. Detailed
targeting is an inclusion under Meta's v23.0 rule, so the Advantage+ flag is sent explicitly (0
unless the caller said `true`), the same rule as the age, gender and custom-audience inclusions.

Detailed targeting options are retired regularly, and a retired id would be refused at the ad set,
after the campaign exists. So every id is checked in one interest validation read
(`GET /search?type=adinterestvalid&interest_fbid_list=[…]`) after the account preflight and before
anything is created: an id Meta marks invalid is a refusal, a failed read or an unanswered id is an
error, and either way nothing is created.

Behind its own default-off flag, `META_INTEREST_TARGETING_ENABLED`, with the locale flag's rules.
Not gated by `global_marketing_ops` (Meta's taxonomy, not an account resource). Left for later:
behaviors and work positions, and the `/search?type=adinterest` picker read the UI needs to find
ids.

Tests: `targeting_options_test.go` (validation then `flexible_spec` with the flag, five failure
shapes with no POST, normalization and the cap) and `meta_targeting_options_test.go` (flag off and
on, the flag interplay, wrong types, Dispatch's wiring through the validation read). Eleven hand
mutations each fail a test.
