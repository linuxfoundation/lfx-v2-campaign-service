# 2026-09-28 — LFXV2-2665: the shared test description overpromised for HubSpot

**Fix** — round 20 of review, raised independently by the general and repo-code reviewers
against a regression introduced by round 19's own docs fix. No dispatcher behaviour change; the
published API contract changes.

Round 19 corrected the `test-<platform>` Goa description from "Verify the stored `<Platform>`
credential against the provider" to "…credential **and the configured account**", because six of
the seven providers do check both and the old wording understated all six. That description is
written once and emitted for all seven, and the seventh is HubSpot, which checks no account at
all. So a correction for six became an overstatement for one, published into `gen/**` and both
embedded OpenAPI copies.

HubSpot's omission is deliberate and already recorded in `docs/api-catalog.md`: `portal_id` is
not an account. Nothing routes on it — its only readers build `app.hubspot.com` deep links for
assets that already exist — and the portal a campaign lands in is the token's own.
`HubSpotDispatcher.ProbeConnection` logs a mismatch as a warning and keeps it out of the
verdict on purpose. The description was therefore telling an integrator that a green HubSpot
test had cleared an identifier nothing ever looked at, which is the one direction an API
description must not be wrong in: it invites a caller to rely on a check that does not happen.

`testMethodDescription(key, title)` now holds the split — six providers keep the conjunction,
HubSpot gets "Verify the stored HubSpot private-app token against the provider. No configured
account is checked: the portal is the token's own." `TestResult.ok`'s own text keeps the
conjunction vocabulary the whole codebase reads it by and gains the same caveat, since its
existing "how deep that account check goes is provider-specific" note stretched to cover a
depth of zero without saying so.

The alternative — softening the shared sentence to something provider-neutral — was rejected.
"Passed its provider-specific verification" is true of all seven and tells an operator nothing
about any of them, which is the wording round 19 was fixing.
