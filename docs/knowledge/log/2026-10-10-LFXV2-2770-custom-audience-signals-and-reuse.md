# 2026-10-10 — Custom audiences: every missing signal, and reuse before create

**Update** — the custom-audience criteria gain `education_topics` (brand-fit course filter,
property resolved from the portal's education event definition) and `page_view_urls` (ungated
`PAGE_VIEW` branches), so the Audience tab can offer "Create list" for all five legacy missing
signals. Interpretation now also returns `similar`: existing lists found by name search and ranked
by the model as reuse / update / unrelated, mirroring the legacy builder's "reuse existing lists
first" step. The model ranks only lists the search found and never writes filters.

Tests: `TestCustomListFilter_PageViewHasNoOperation`, `TestCustomListFilter_EducationTopics`,
`TestCourseTopicProperty`, `TestParseCustomReuse_OnlyRanksCandidates`,
`TestInterpretCustom_RanksExistingListsButNeverInventsOne`,
`TestPreviewCustom_ResolvesTheCourseTopicPropertyFromThePortal`, `TestEventTypeProperties`.

**Fix, same day, found by the live E2E** — against the `tlf` portal the topic picker chose
`course_code`, and a CONTAINS on a course code never matches a topic keyword, so every
topic-scoped education list would have been empty. `CourseTopicCandidates` now ranks word
properties (name/title/topic…) above plain ones and identifiers (code/id/number…) last; preview
returns `education_topic_candidates`, and an `education_topic_property` the operator sends is
honoured only when it is one of those portal candidates.

**Fix, 2026-10-12, found by the first live creates** — the page-view shape was confirmed by a real
create (list 31588, QA signal mapping PASS). A location audience was refused, and HubSpot's
list-filter reference explains why: a `UNIFIED_EVENTS` branch "cannot have any additional filter
branches", so event properties now go in the node's own `filters`; and the operators were narrowed
to the documented vocabulary (`CONTAINS` not `CONTAINS_TOKEN`; `ALL_PROPERTY` `IS_KNOWN`/`IS_NOT_KNOWN`
not `HAS_PROPERTY`; MULTISTRING "any of" is `IS_EQUAL_TO` over several values; `IS_NOT_EQUAL_TO`
for "not any of"), pinned by `TestCustomListFilter_OnlyDocumentedOperators`. The HubSpot client now
keeps a one-line, bounded `VALIDATION_ERROR` message (`hubspot.ValidationMessage`, never in
`Error()`), which the custom-create handler returns as a 400 so a refused definition is diagnosable.
Not changed, noted for follow-up: `filters.go`'s brief-build helpers (`registeredForEvent`,
`educationEnrolled`, `isAnyOf`) use the same nested/undocumented shapes.

**Fix, 2026-10-12 (later)** — a live speakers create was refused: `is_event_speaker_bool` is a
BOOL property and was filtered as MULTISTRING. Speaker signals are now typed from the portal's
registration event definition (`EventTypeProperties("6-48984571")`): BOOL and NUMBER operations
carry a single `value`, a property the portal lacks is skipped, and with no speaker property at all
a speakers restriction is refused (it would select every attendee). **QA now accepts send-time
suppression:** `qa/run` takes `send_suppression_list_ids` (the lists the send applies on top of the
list); they are read like the list's own exclusions, one hop into a combined suppression, and count
toward the GDPR/opt-out and exclusion-completeness checks. An id that cannot be read counts for
nothing. `exclusion_count` now includes them and `send_time_count` reports their share.
