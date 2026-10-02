# 2026-09-29 HubSpot hero alt text, CTA color contrast, and the hubspotUrl result field

**Fix** — Three related changes landed in the same commit as the segment-conditional
content blocks (see `2026-09-29-email-copy-segment-conditional-content-blocks.md` for
that change; this entry covers the other three).

`RebuildEmailContentInput.HeroImageAlt` (`internal/platform/hubspot/content.go`) sets the
hero image's alt text, forwarded from `hubspotConfig.HeroImageAlt`
(`internal/dispatch/hubspot.go`). `addHeroSection` trims it and falls back to a generic
"Event banner" when blank, replacing the prior hardcoded "Email Banner" — which described
the widget rather than the event and shipped identically on every campaign regardless of
what the image actually showed.

The CTA button's and footer link's `background_color`/`link_font.color` moved from
`#0094ff` to `#2563eb` (Tailwind blue-600, matching the frontend preview's CTA) in the same
change: the old value contrasted white text at only ~3.14:1, failing the WCAG AA 4.5:1 text
threshold, where `#2563eb` clears it at ~5.17:1.

`campaignFromHubSpot` (`internal/dispatch/hubspot.go`) started persisting `HubspotURL:
e.AppURL` under the JSON key `hubspotUrl` in the campaign's `Result` blob — restating
`hubspot.Email.AppURL`, which is tagged `json:"-"` on the embedded type and would not
otherwise serialize. `internal/service/orchestrator.go`'s `hubspotURLFromResult` reads that
key back out and populates `platformResult.HubspotURL` (JSON key `hubspot_url,omitempty`)
on the polled job result, for the HubSpot email channel only — every other platform's
`Result` blob has no `hubspotUrl` key, so the field decodes to `""` and is omitted.

`docs/knowledge/code/internal-platform-hubspot.md`, `internal-dispatch.md` and
`internal-service.md` were updated to match. `docs/api-catalog.md` gained the
`heroImageUrl`/`heroLinkUrl`/`heroImageAlt` config fields and the `hubspotUrl` `PlatformResult`
field in this same follow-up fix — the original commit had not documented either.
