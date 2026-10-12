# 2026-10-06 — LFXV2-2665 Bound caller text in Google Ads extension errors

**Fix** — Nine validation arms in `assets.go` and `assets_extended.go` interpolated an
unbounded caller string into an error that persists unencrypted as a `Steps` entry: a call
extension's country code, a promotion's occasion and language code, a price extension's type,
price qualifier and language code, a price offering's unit, and the two LENGTH-limit arms for
callout text and promotion target. The length arms are the sharper case — each fires in
precisely the situation where the value exceeds its limit, so echoing it raw is unbounded by
construction. The seven enum and code arms now report through `capForError`, applied to the
already-trimmed and already-normalized local rather than the raw field, so the message shows
the value the validator actually judged. The two length arms take the stricter
`validateEntityName` option instead: they already carry the extension's index, which locates
the offending entry on its own, and the measured count plus the limit says everything a
capped echo of an over-long value would. `assets_leadform.go` was already correct and is
unchanged.

**Docs** — The universal-invariant section of `docs/knowledge/code/internal-platform-googleads.md`
named only the creative and bidding validators among `capForError`'s callers; it now names the
extension fields too, and records that a length-limit arm reporting an index and a count —
rather than a capped echo — satisfies the same discipline.

**Fix** — The extension validators had no equivalent of the `_ErrorDoesNotEchoTheRawURL` tests
that pin the URL arms. `TestExtensionErrors_BoundTheCallerString` now exercises all nine arms
at two input sizes an order of magnitude apart and fails if ten times the input buys a
materially longer message, or if any run of the value longer than the cap survives into it.

**Docs** — Three doc comments still described conversion actions as SEARCH-only and refused on
Demand Gen, which stopped being true when `validateConversionActions` was widened to admit
VIDEO and DISPLAY — `campaign.selective_optimization` is defined for those channels, so
refusing them would have been over-refusal. The comments on `CampaignInput.ConversionActions`,
on the dispatch config's `ConversionActions`, and the `validateConversionActions` doc block now
say SEARCH, VIDEO and DISPLAY, refused on Demand Gen and Performance Max — matching the gate,
the inner comment beside it, and `docs/api-catalog.md`. A comment describing a NARROWER refusal
than the code performs invites a "fix" that reintroduces the over-refusal.
