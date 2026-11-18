# 2026-11-17 — #589 a variant name equal to a default name is refused synchronously

**Fix** — from the PR #638 review. For a `KubeCon` brief, an unnamed first variant and a second
named `KubeCon | variant-1` passed `PreflightCreate` and failed only in the client's
`distinctCreativeNames`: a 202 and then a failed job. `PreflightCreate` now reads the brief
(`model.CreatePreflightBrief`, through `linkedinPreflightEventName`) and runs
`linkedin.ValidateCreativeNames`, which normalizes the event name exactly as `CreateCampaign`
does (`normalizeEventName`: trimmed, `|` to `-`). Without a brief it checks the config alone, as
before. Pinned by `TestLinkedIn_VariantNameCollidingWithADefaultIsASynchronous400`.
