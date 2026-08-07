# 2026-08-06 — GA-5: reconcile the duplicate metrics foundation against main

**Update** — The metrics foundation (LFXV2-3001) landed on `main`, so this branch's own copy
of the shared layer was superseded. Merging the updated base surfaced that as a conflict in
every foundation file — `internal/domain/errors.go`, `internal/service/orchestrator.go`,
`internal/service/brief.go`, `design/brief.go` and their generated output. The merge takes
`main`'s canonical version wholesale in each one; nothing on this branch was a later
improvement over it, and the foundation's version is strictly wider (it adds
`ErrMetricsWindowUnsupported`, the `(nil, nil)` MetricsReader contract guard, service-layer
window validation, and the response's use of the VALIDATED request window rather than an
adapter's echo).

Git merged the two `CampaignMetrics` declarations in
`internal/domain/model/campaign.go` textually rather than flagging them — both blocks landed
side by side and the package stopped compiling with `CampaignMetrics redeclared`. The
branch's copy (with `Window string`) is deleted; the foundation's (`Window
model.MetricsWindow`, alongside `MetricsWindow`/`IsValidMetricsWindow`) is the survivor.

**Update** — With the foundation's typed window in place, `MetricsReader.ReadMetrics` now
takes `model.MetricsWindow` rather than a platform-defined literal, so the Google Ads adapter
needs a translation step it previously did without — it used to pass the caller's string
straight through as a `googleads.MetricsWindow`, which is exactly the dialect leak the
platform-agnostic vocabulary exists to prevent.

`googleads.WindowFor` does the mapping, and it lives in the PLATFORM package, not the
dispatcher, matching how every other platform client maps the shared vocabulary to its own.
GAQL literals therefore never appear outside `internal/platform/googleads`. The translation
does not replace `validMetricsWindows`: the mapped literal still goes through that allow-list
in `GetCampaignMetrics`, because the allow-list is the GAQL-injection guard (the window is
concatenated into the query string) and a translation function is not a security boundary.

An unmappable window is caller input, not an upstream failure, so the adapter reports it as
`errors.Join(domain.ErrMetricsWindowUnsupported, err)` under a single `%w` — 400, not 503,
with the client's own cause still reachable via `errors.Is`. The guard runs before credential
resolution, so the platform is never contacted for a window it cannot express.

The adapter returns the REQUEST window in `CampaignMetrics.Window`, not the client's echoed
GAQL literal. Echoing would put `LAST_30_DAYS` into a response whose contract declares the
lowercase vocabulary, and the generated client would reject the otherwise-successful 200.

`TestGoogleAds_ReadMetrics_UnmappableWindowIs400AndNeverContactsPlatform` pins all of it
across three inputs — Google's own `LAST_30_DAYS` (valid downstream, not a member of the
model vocabulary), an unknown `last_90_days`, and the empty string — with a connection reader
that fails the test if it is consulted at all. Verified binding: making `WindowFor`'s default
branch fall through to `WindowLast30Days` fails it with `expected an error`.
