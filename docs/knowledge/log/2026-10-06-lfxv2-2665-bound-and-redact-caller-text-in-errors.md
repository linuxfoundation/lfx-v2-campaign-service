# 2026-10-06 — LFXV2-2665 Bound and redact caller text in Google Ads validation errors

**Fix** — `validateYouTubeVideoIDs` refused a URL-shaped video id by echoing the raw string.
That arm fires precisely BECAUSE the value is a URL, and these errors persist unencrypted as
`Steps` entries, so a pasted YouTube share link wrote its `?si=…` tracking or signing query
into the database to tell the caller something `scheme+host+path` already tells them. It now
reports through `redactURLForError`, the helper every other URL in this package already goes
through. Its sibling `demandgen_creative.go` validator had redacted every URL since it was
written; this one site had not.

**Fix** — Thirteen validation arms across `pmax_creative.go`, `demandgen_creative.go` and
`bidding.go` interpolated an unbounded caller string into an error: business names, asset
group names, display paths, calls to action, creative text, bidding strategies and conversion
actions. A length-limit refusal is emitted in exactly the case where the value exceeds the
limit, so echoing it raw is unbounded by construction — a megabyte of pasted text became a
megabyte of unencrypted `Steps` row. All thirteen now report through `capForError`.

**Fix** — `capForError` cut on a BYTE index. Several of the fields now pointed at it are
validated by DISPLAY WIDTH rather than byte length, so multibyte copy is expected in them and
a fixed-offset byte slice would write a half rune into a persisted message. The cut now walks
back to a rune boundary via `utf8.RuneStart`. Three regression tests pin all of this: the URL
arm must not echo the query, a 5000-character non-URL value must produce an error under 400
bytes, and a capped multibyte string must stay valid UTF-8.

**Docs** — A doc comment inserted by the previous round sat flush against the one above it, so
godoc attributed `googleAdsToggleTargets`' block — which carries its load-bearing three-return
contract — to `googleAdsActivationGate` and left `googleAdsToggleTargets` undocumented. The
block was moved above the function it describes. The ACTIVATE invariant comment at
`googleAdsToggleStatus` still named the keyword requirement as if it were universal, which
stopped being true when the gate became channel-aware; it now says the keyword rule is
Search-only and points at `googleAdsActivationGate` for the per-channel shape.
