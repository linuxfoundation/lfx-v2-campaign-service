# 2026-10-15 — Custom audience builder: PR review fixes

**Fix** — addresses the maintainer review of the custom audience builder PR:

- **Speakers-list auto-pick fails closed.** `SpeakerListFits` accepted any speakers list for the
  parser's default `current_past` scope and for a draft naming no edition, and a filled `ListID`
  then replaced the scope and event-name filters — so the wrong speakers could be targeted. It now
  refuses a draft with no editions, requires a scope suffix to equal the requested scope, and meets
  `current_past` only with the explicit ` - Current + Past` suffix
  (`TestSpeakerListFits`, `TestSpeakerListFits_EmptyEventNamesFailsClosed`).
- **Interpreted drafts stay inside the contract.** `page_view_urls` has its own 10-item cap in
  `ParseCustomDraft` (was 25), and an event audience's `registered_for` keeps the 25 most recent
  warehouse editions (`capPastEditions`) with a note naming how many were left out, so an unedited
  draft no longer gets a 400 from preview (`TestInterpretCustom_CapsPastEditionsToTheContract`).
- **Countries are canonicalized.** `CanonicalCountries` (via `DisplayName`) runs where criteria
  enter — `ParseCustomDraft`, the service's `customCriteria`, and `previewCustom` — so "USA" filters
  on "United States"; preview warns about any country an enumeration `country_dropdown` has no
  option for.
- **Hero warnings are pinned at every call site.** The three `TestOrchestrator_*CarriesHubspotURL`
  tests now seed `warnings` in the Result blob and assert the persisted job result carries them;
  deleting any one of the three `res.Warnings` assignments fails exactly its own test.
- **An SSRF-refused hero says so.** `heroFailureReason` checks
  `errors.Is(err, eventurl.ErrEventURLForbidden)` before its string arms, which had reported a
  refused address as a failed download.
- **Contract and docs.** `send_suppression_list_ids` moved next to `targets_ca` in the design so
  `event_name` keeps its own rationale; `AudienceCustomInput` and `AudienceCustomPreviewResult`
  carry valid type-level examples. The API catalog documents `send_suppression_list_ids`,
  `send_time_count` and discovery's `hidden`; the HubSpot concept documents the `VALIDATION_ERROR`
  message exception to its body-free errors; the audience concept's speaker signals
  (`CONTAINS`, not `CONTAINS_TOKEN`), ported-features paragraph, education-topic-property rule and
  PAGE_VIEW confirmation (list 31588) are current; and the hero-failure notes moved under the
  HubSpot Result-blob section, whose field list now includes `Warnings`.

**Fix** — Copilot's two "previously missed" findings not covered above:

- **Only a string event property carries the education topic**, for the automatic pick and the
  caller's choice alike: the topic filter is always a string `CONTAINS`.
- **A suggested filter on a property name the contract refuses is dropped** (outside
  `^[A-Za-z0-9_]+$`, or over 100 characters), with the existing note.
