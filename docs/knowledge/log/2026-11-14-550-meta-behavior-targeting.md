# 2026-11-14 — #550 Meta behavior and work position targeting (gap 7, remainder)

**Creation** — The rest of gap 7 of the Meta gap review (#550). Interests were the only detailed
targeting a Meta ad set could carry. `metaConfig.targeting.behaviors` and `targeting.workPositions`
(numeric ids, at most 25 each) now join the interests in the same `flexible_spec` entry, so a person
matching any listed interest, behavior or work position qualifies (Meta ORs the keys of one entry).
Both are inclusions, so the Advantage+ flag is sent explicitly, as for interests.

Every id is checked before anything is created, in one ad account read:
`GET /act_<id>/targetingvalidation?targeting_list=[{"type":"behaviors"|"work_positions","id":…}]`.
An id Meta marks invalid is a refusal; a failed read, no `data`, an unanswered id or a row without
`valid` is an error. Either way nothing is created.

Behind its own default-off flag, `META_BEHAVIOR_TARGETING_ENABLED`, with the locale flag's rules. Not
gated by `global_marketing_ops` (Meta's taxonomy). Not live-verified: the validation read's row
fields (the reference names only the request) and the `flexible_spec` keys.

Tests: `detailed_targeting_test.go` (one validation read then one shared `flexible_spec` entry, six
failure shapes with no POST, normalization, the inclusion rule) and `meta_behavior_targeting_test.go`
(flag off and on, the flag interplay, wrong types, Dispatch's wiring and refusal with no request).
Fourteen hand mutations each fail a test.
