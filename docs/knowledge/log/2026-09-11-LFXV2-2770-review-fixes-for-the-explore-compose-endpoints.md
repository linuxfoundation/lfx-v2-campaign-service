# 2026-09-11 — LFXV2-2770: review fixes for the explore/compose endpoints

**Fix** — Local post-commit review of the audience-builder explore/compose endpoints (the
commit this fixes up is [[2026-09-11-LFXV2-2770-audience-builder-moves-to-go]]) found several
correctness defects, all now fixed:

- `SuppressionCategoryEvent` was `"event"`, not the Goa enum's `"event_specific"`, so nothing
  ever classified as event-scoped suppression.
- `composeErr`'s orphaned-suppression partial reported `code: "409"`. [[2026-09-11-LFXV2-2770-compose-partial-is-a-body-not-a-header]]
  already documents that `ComposePartial` is one of `ComposeAudienceMaster`'s two HTTP 500s —
  the code field was simply wrong against the service's own documented contract, corrected to
  `"500"`.
- A rejected event URL (`eventurl.ErrEventURLInvalid` / `ErrEventURLForbidden`) fell through to a
  generic 500 instead of the 400 every other malformed-request case in `audienceExploreErr`
  already gets.
- `ComposeMaster` validated inclusion list ids only after the suppression list was already
  created upstream, and on a downstream master-filter-build failure it returned a bare error
  instead of `ComposePartialError`, discarding the created suppression list's id. Validation now
  runs before any mutating call (`audience.ValidateInclusionIDs`, extracted from
  `MasterListFilter`/`MasterListWithSuppressionFilter`'s previously duplicated checks), and the
  filter-build failure path now returns the partial result when a suppression list exists.
- `RunQA`'s numeric-list-ref path did not distinguish a HubSpot not-found from any other fetch
  error, so a bad list id read as a generic failure instead of `audience.ErrListNotFound`.
- `PickNameMatches` mishandled two or more exact matches — the switch's ambiguous-match case only
  fired when there were zero exact matches, so two-or-more-exact fell through unhandled.
- Removed dead code the review found no caller or test for: `builder_discovery.go`'s
  `behaviouralFilterTypes` map and `KeepBehavioural`, and `builder_types.go`'s
  `LastSentEmailSearchLimit` constant (email-search bounding is already independently enforced by
  `internal/platform/hubspot`'s `maxListPages`/`maxFilteredScan`/`maxUnfilteredEmails`).
- The `AudiencePreviewCount` design doc and its `estimate` field described the above-bound
  fallback as a "floor"/"lower bound"; the implementation reports the SUM of the selected lists'
  sizes, an upper bound that over-counts on overlap. Corrected the Goa design comments (and
  regenerated `gen/http/...` plus the `cmd/campaign-service/kodata` OpenAPI copies) and the same
  language in `docs/api-catalog.md`, which also had a `hubspotConfigured` → `hubspot_configured`
  field-name typo.

Part of [[2026-09-11-LFXV2-2770-audience-builder-moves-to-go]].
