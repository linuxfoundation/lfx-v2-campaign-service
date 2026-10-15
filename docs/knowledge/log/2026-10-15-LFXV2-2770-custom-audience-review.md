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

**Fix** — Greptile's review of the merged head:

- **Reuse is judged against the answers.** The reuse ranking saw only the original request, so after
  an answer narrowed a Seoul audience to speakers a general Seoul list could still be "reuse". The
  prompt now carries the answers and the settled restrictions, and `GuardReuse` holds a "reuse" of a
  list whose name does not show the restriction to "update".
- **Previewed names fit create's limit.** `VersionedName` and derived names are cut to the input's
  250-rune `MaxLength`, so a taken 250-character name previews as a `(v2)` create accepts.

**Fix** — the maintainer's second review:

- **The endpoint count is thirteen everywhere.** The API catalog, the HTTPRoute and RuleSet concept
  pages, `design.md`, `cmd-campaign-service.md`, `internal-service.md`, and the chart and parity-test
  comments all still said nine (or ten), with only two two-segment leaves. They now name thirteen
  leaves, five of them two segments deep.
- **The topic refusal names the real cause.** With string-only candidates, a portal whose only
  topic property is an enumeration was told it had none. The 400 now says it has no text (string)
  course or topic property.
- **The draft caps are pinned against the generated validator.** A new `internal/apivalidation`
  test parses an oversized model reply, decodes the unedited draft through the generated
  preview decoder, and checks one item past each capped field is refused (cities, countries,
  education_topics, speakers.event_names, property_filters and their values, page_view_urls,
  registered_for, and the property-name length), so a Go cap drifting from its
  `MaxLength` fails.
- **The HubSpot client documents its one body exception in code too**, and the concept page names
  the shared `customErr` mapper (interpret, preview and create) as `ValidationMessage`'s caller.

**Fix** — the maintainer's third review:

- **No filter value reaches the reuse model.** The restrictions quoted to the ranking model describe
  a property filter by its property and operator only: a value can be a real contact's email or
  phone, and the prompt goes to an external model.
- **Course topics are not a reuse restriction.** Every education audience carries them as its brand
  fit, so counting them made reuse unreachable for the canonical draft and re-created lists this
  service had already built.
- **`attach-existing` has its parity row**, so a leaf dropped from both chart files fails the build,
  as the API catalog says.
- **The drift test covers every capped field**, including `countries`, `education_topics`,
  `speakers.event_names` and a filter's `values`.
- Nits: `internal-service.md` says which file serves which endpoints, the two Kubernetes concept
  pages name both non-idempotent writers, and `CustomNameMaxLen` follows the length-limit naming.
