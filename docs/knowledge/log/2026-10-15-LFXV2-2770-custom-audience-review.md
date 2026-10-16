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

**Fix** — Greptile on the merged head:

- **The warehouse's past editions are a default, not an override.** With an event URL, interpret
  replaced the model's `registered_for` with every past edition, so an operator who asked for one
  edition's registrants, or answered that past registrants are excluded, got the whole family.
  Named editions now win (kept only as exact known names; others dropped with a note), and a model
  reply with `include_past_editions: false` gets none.

**Fix** — Copilot's re-review of the current head:

- **A shared city is not an event word.** `GuardReuse` judged "same event" on every label word, so
  another event's list in the same city kept a model "reuse". It now uses only distinctive words:
  not the audience's own locations, not generic event words.
- **A speakers list is reused only when it is the right one**: the selected list, or one
  `SpeakerListFits` accepts for the scope and editions, as the auto-pick requires.
- **The hero warning points to the HubSpot editor.** Staging again reuses the stored campaign and
  its warnings, so "or stage again" invited retries that could not repair the draft.
- **A label of only generic words matches as a whole** (Greptile). "Open Source Summit Japan" has no
  distinctive word left, which marked the event's own lists unrelated; such a label must now appear
  whole, year aside, in the list's name.
- **Suppression hints match with separators normalised** (Copilot). "Seoul - Opt_Out Contacts" matched
  no hint, so discovery offered it for inclusion and reuse accepted it. `hasSuppressionHint` also
  reads the name through `normaliseSuppressionName`, and every reader of the hints uses it:
  `DiscoveryHidden`, `ClassifyList`, `MatchesBrandMaster` and `MatchesEventSuppression`. Test-list
  hints keep their punctuation, so "Contest" is still not a test list.
- **Named editions are gated under the stricter reading** (Greptile). With `gate_event_history`, the
  location and education history were gated but the `registered_for` branch was not, so the past
  editions an event draft adds by default reached registrants outside the brand master list. That
  branch now carries the gate too, and `AllBranchesGated` counts it.
- **This edition is named with its year** (Greptile). A yearless page title ("Synthetic Summit") went
  into the prompt and the known-editions check as this edition's name, so the exact filter missed
  registrations stored as "Synthetic Summit 2026". `customEventContext` now names it with
  `WithEventYear`.
- **A brand inside the event title still counts** (Copilot). `DominantBrand` dropped any brand-slot
  segment sharing a word with the event name, so "PyTorch" lost every vote for "PyTorch Conference
  2026". `isEventSegment` now treats a segment as the event's own name only when it shares two or
  more event words, or every word of a one-word event; the AGNTCon exclusion still holds.
- **A speakers-only audience is gated under the stricter reading** (Greptile). With no location or
  history branch, the one roles/properties branch never took `gate_event_history`, so create could
  include speakers outside the brand master list. It now carries the gate under that reading, and
  `AllBranchesGated` counts it.

**Fix** — the maintainer's fourth review, and one Copilot "previously missed" item:

- **A one-word label is never matched as a bare substring.** The all-generic whole-label fallback
  let `Label: "Seoul"` keep another event's Seoul list as reuse; it now needs at least two words.
- **Named editions are checked without a warehouse too.** The guard ran only when past editions
  came back, so a guessed name passed unverified for a first edition or an unconfigured warehouse.
  It now always runs, and the note says the name could not be verified.
- **`AllBranchesGated` reads the built base branches** instead of restating the gating rules, which
  had needed matching edits in two places twice in one round.
- **The interpret response reports the event as the page declares it**, as `discover` does; the
  year-qualified name stays internal (prompt and edition check).
- **A long-form brand votes as its short name** (Copilot). Lists filed under "The Linux Foundation"
  made `DominantBrand` return a phrase `MatchesBrandMaster` could not resolve; it now counts as "LF".
- Comments and docs: `DominantBrand` and `customBaseBranches` godocs, a test comment back on its
  test, the `CustomListFilter` concept bullet, the custom-handler paragraph moved under
  "Audience-builder handlers", and the suppression `ClassifyList` assertion given a page-view filter
  so it pins the rule.

**Fix** — Copilot and Greptile on the fourth-review head:

- **A speakers-only audience gets no past editions by default** (Greptile). The warehouse's editions
  are a regional audience's default; added to a roles-only audience they ANDed every speaker with a
  past registration, dropping first-time speakers.
- **A known long-form brand is a brand even when the title repeats it** (Copilot). "The Linux
  Foundation" for "Linux Foundation Member Summit" shared two event words and was discarded before
  the alias mapping ran; the alias is now checked first.
- **A blank answer settles nothing** (Copilot). It is left out of the prompt, yet it marked its
  question answered and counted toward the rounds that make the draft final.
- **Discovery resolves the fallback brand before classifying** (Copilot). It was inferred after
  every list was classified, so a brand-scoped education list stayed uncertain and education was
  reported missing.
- **The edition default needs a location** (Greptile). A topic-only education audience counted as
  regional, so past registrants who never enrolled were OR'd in. "Regional" now means a draft with
  cities or countries.
- **A label filled from the page title is cut to the limit** (Greptile). It was set after the
  parser's length checks, so a long or year-extended title failed preview; `FitLabel` cuts it to
  `CustomLabelMaxLen` (200 runes).
- **Exact edition names stay whole** (Greptile). The prompt and the parser cut every value to a
  display field's 200 characters, so a long edition name could never match its warehouse name and
  was dropped. Edition names (`registered_for`, `speakers.event_names`, the prompt's event name) now
  keep up to `customEditionMaxLen` (500).

**Fix** — the maintainer's fifth review (approved, with one minor and three nits):

- **The prompt states the regional-only default.** It still promised past editions to every event
  audience; it now says they are added only for an audience with cities or countries, and tells the
  model to copy the editions it wants for an audience without a location (`knownEditions` checks
  them).
- The regional rule and the two-word whole-label minimum are stated fully in `internal-dispatch.md`,
  `internal-audience.md` and `GuardReuse`'s godoc, and the long-edition test now also checks the
  prompt shows the whole name and that a long `speakers.event_names` value survives the parser.

**Fix** — Copilot and Greptile after the fifth review:

- **A send-time list that only some branches include is a real exclusion** (Copilot). QA ignored any
  send-time suppression the audited list referenced as IN_LIST, but a flattened reference proves
  nothing: for "A OR B", suppressing A still leaves B minus A. `RequiredInListIDs` walks the filter
  tree, and only a list every path requires (the whole audience is inside it) is ignored.
- **Page URLs are kept whole or dropped, never cut** (Greptile). They were cut to 200 characters,
  and a URL is matched as a substring, so a cut one matched other pages too. They now keep up to
  2,000 characters, and a longer one is dropped with a note; the prompt quotes the event URL at the
  same length.
- **Check 4 ignores this edition's registrants when the send suppresses them** (Copilot). A list
  including them as one branch and excluding them at send time still failed check 4; a verified
  send-time suppression is now left out of the inclusions check 4 reads. A branch-local
  `NOT_IN_LIST`, or an unverifiable send-time id, proves nothing and is not used.
- **Check 4 credits everything the send provably excludes** (Greptile, Copilot). Send-time ids are
  verified independently of check 3's dedup, so one a list already excludes in one branch still
  counts, and a verified suppression wrapper's fully covered children (`CoveredListIDs`: a list
  that alone is one alternative of an OR, as ComposeMaster's "R OR OptOut" combined suppression)
  are credited too. An intersection ("R AND OptOut") covers nothing.
- **Property-filter values are kept whole or dropped** (Greptile), as page URLs are: a CONTAINS on a
  cut value matches contacts the full value would not.
- **A suppression wrapper credits only what it fully covers** (Copilot). `exclusionNames` read a
  wrapper's flattened IN_LIST references, so "GDPR AND Global Opt-Outs" satisfied both suppression
  checks though a contact in only one child stays eligible. It now credits `CoveredListIDs`, the
  same rule check 4 uses: an OR wrapper applies each child, an AND wrapper none.
- **A wrapper covering the audience is no suppression** (Copilot). A send-time wrapper that fully
  covers the audited list, or a list every branch requires, excludes every contact; it now gets the
  same treatment as the audited list sent directly and does not count.
- **A wrapper covering every alternative is no suppression** (Copilot). "1 OR 2" suppressed by a
  "1 OR 2" wrapper leaves nobody, though neither list is required on its own. `TreeCoveredBy` checks
  whether the wrapper's covered lists together cover every path through the audience; a wrapper
  covering one alternative still counts.
- **Separate suppressions covering the audience together are no suppression** (Copilot). The
  coverage guard ran per suppression, so `[A, B]` for an "A OR B" audience passed though together
  they leave nobody. The union of verified suppressions and their covered children is now checked,
  and when it covers the audience the set's credit is withheld, as for a single wrapper.
- **A suppression, test or staff rollup is not expanded in discovery** (Copilot). Discovery expanded
  rollups before its hygiene check, so a child of an "Opt-Outs" rollup was offered for inclusion.
  Such a parent is now classified, and hidden, as itself; a master rollup is still expanded.
- **Test and staff markers win over "master"** (Copilot, review body). "Synthetic Summit - Master
  [test]" was classified as a master, so discovery still expanded it and offered its children. The
  test and staff hints are now checked first.

**Fix** — three Copilot findings that sat only in review bodies ("Previously missed"), found in a
sweep after review pagination hid them:

- **The hero warning no longer claims the banner is absent.** A failed upload can leave the
  template's banner on the draft (a hero-only request skips the rebuild), so it now asks the
  operator to check the banner in the HubSpot editor and add or replace it.
- **Check 4 reads registration predicates before the name.** `RegisteredEditions` takes a list's
  `event_name` filter values, so "26Q4 - KubeCon 2026 - Event Registration" built for 2025 is a
  historical audience; the name stays the fallback for a static or opaque list.
- **Only the event's own lists vote for its brand.** A search hit sharing just a generic word
  ("summit") no longer votes in `DominantBrand`.
- **Only positive registration predicates count** (Greptile). `RegisteredEditions` read every
  `event_name` value as registrants, so a list of people who had NOT registered for this edition
  failed check 4. A negated event branch or a negative comparison now returns nil, and the
  name-based check decides.
  Tightened further (Copilot): only exact equality comparisons, in a list whose registration
  events are all "has completed", count; a substring ("contains KubeCon") names no edition and
  falls back to the name too.
- **Edition evidence is read from the filter tree, on every registration path** (Copilot, review
  body). With "registered for 2025 OR any registration", the flattened read took only 2025 and
  passed check 4 for 2026. `RegisteredEditions` now walks the tree and returns editions only when
  every registration branch is a completed registration with an exact event_name equality.
- **An inline registration suppression excludes its edition** (Copilot, review body).
  `CoveredEditions` reads a verified suppression's tree for a lone completed-registration branch
  with only an exact event_name equality, and check 4 leaves those editions out; one narrowed by any
  other condition covers nothing.
- **Conditions in one registration branch intersect** (Greptile). `exactEditions` unioned separate
  event_name filters inside a branch, so "2025 or 2026" AND "2025" read as both years and check 4
  skipped 2026. The editions are now what every condition allows.
- **Inline edition suppression reaches referenced registration lists** (Copilot, review body). A
  referenced list whose own predicates name exact editions is judged by those editions, less any a
  verified suppression excludes whole; a referenced list without such predicates keeps its name.
- **Editions replace a name only when every path is a registration** (Greptile). A list "2025
  registration OR an imported membership list" yielded only 2025, so a referenced "KubeCon 2026
  Registrants" list passed check 4 though the membership branch may hold 2026 registrants.
  `RegisteredEditions` now also requires every path through the tree to run through an exact
  completed-registration branch.
- **The API contract no longer promises a missing banner** (Copilot, review body). The `warnings`
  description in `design/brief.go` and the API catalog now say the requested hero could not be added
  and the banner needs checking, matching the operator warning.
- **Registration branches are positive evidence wherever they sit** (Greptile, Copilot).
  All-or-nothing edition evidence dropped an explicit "registered for KubeCon 2026" branch from a
  mixed list, so check 4 passed on a neutral name. `RegistrationEvidence` now collects every exact
  completed-registration branch's editions as evidence, less any suppressed whole, and sets the name
  aside only when those branches are the list's whole membership.
- **An inline suppression that empties a registration-only audience earns no credit** (Copilot).
  When every edition of an exclusive registration audience is in the verified suppressions'
  `CoveredEditions`, the send has no recipients, and the set's credit is withheld.
