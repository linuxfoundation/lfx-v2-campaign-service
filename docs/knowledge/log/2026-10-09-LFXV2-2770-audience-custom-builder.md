# 2026-10-09 — Custom audiences: build a HubSpot list from a described audience

**Creation** — three `audience-builder` endpoints port the legacy app's Custom Audience /
"Build From Scratch" tab:

- `POST .../custom/interpret` — a model turns a free-text request ("mailable contacts in Seoul for
  OSS Korea") plus answers to earlier questions into DRAFT criteria and blocking questions, and the
  service resolves the brand master gate and any existing speakers list. Creates nothing.
- `POST .../custom/preview` — validates reviewed criteria against the portal (every contact
  property must exist; its type sets the operation type), and returns the list name (versioned if
  taken), the branch count, and the gate's size as an upper bound when every branch is gated.
  Creates nothing.
- `POST .../custom/create` — rebuilds from the criteria and creates ONE list. Not idempotent; an
  unconfirmed create is the compose partial-failure response with the name.

Unlike legacy, the model never writes filter JSON or reaches the create call: `CustomListFilter`
builds the tree deterministically. Custom lists carry no suppression (they are inclusion building
blocks for the composed master). Supports location (city/country), event-history branches with an
optional master-list gate, exact-edition registrants, extra property filters, and speaker /
ambassador restrictions — which also covers the "create list" action for the missing
`event_speakers` and `event_registration` signals.

Tests: `builder_custom_test.go`, `builder_custom_interpret_test.go`,
`internal/dispatch/audience_custom_test.go`, `internal/service/audience_custom_test.go`,
`internal/platform/hubspot/properties_test.go`.
