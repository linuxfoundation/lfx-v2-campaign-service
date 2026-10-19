# 2026-10-13 — A hero image that cannot be re-hosted is reported, not silent

**Fix** — a staged email whose hero could not be uploaded to HubSpot's file manager was created
without a hero section and reported a clean success. The only trace was a server log line, so an
operator found the missing banner only after opening the draft. This was observed on a production
draft where the sponsor logos and button were present and the hero was not.

- `uploadHeroImage` now returns an operator-facing warning alongside the hosted URL. The text comes
  from `heroFailureReason`'s fixed vocabulary, never from the error, because the error can carry
  upstream response content and this string is persisted and returned by the API.
- `Dispatch` stores the warnings in the campaign Result blob, `hubspotWarningsFromResult` reads them
  back, and `GetJob` exposes them as `PlatformResult.warnings` (`design/brief.go`; generated code
  for the briefs service updated). No warning is reported when the campaign asked for no hero.
- The dispatch stays best-effort: a failed hero never fails the create.

See [internal/dispatch](../code/internal-dispatch.md).
