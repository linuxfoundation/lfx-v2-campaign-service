# 2026-10-07 — LFXV2-2665 HubSpot monitor distrusts a partly decoded record

**Fix** — Two more PR #290 review threads on `monitor-hubspot-account`.

- **Partial decode.** `hubspotMonitorTargets` discarded the `json.Unmarshal` error on a row's
  `Result`. A type error (for example `"abTestVariant":"oops"`) still leaves the fields decoded
  before it, so the row's portal matched, its own email was read, and the malformed variant was
  neither read nor counted. The record is now reset on any decode error, so the row records no
  portal and its email counts as unattributable. A `"oops"` variant row in
  `TestHubSpotEmailMonitor_StoredIDsAreValidatedVerbatim` covers it.
- **Comment.** A stray `//` left inside the `ReadEmailMonitor` doc comment by a re-wrap is gone.
