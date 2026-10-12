# 2026-10-07 — LFXV2-2665 HubSpot monitor keeps an undecodable row's variant

**Fix** — One more PR #290 review thread on `monitor-hubspot-account`.

Resetting a row's `Result` on any decode error made a recorded A/B variant vanish: for
`{"id":"1207","name":123}` the row's own email was counted unattributable, but the variant was
neither read nor counted. When the reset happens, the blob is now checked again as a plain JSON
object, and a non-null `abTestVariant` adds a second unattributable email. Only a blob that is not
a JSON object at all records nothing more to count. A `mistyped-variant-name` row in
`TestHubSpotEmailMonitor_StoredIDsAreValidatedVerbatim` covers it.
