# 2026-08-14 — LFXV2-3259 encoding review follow-ups

**Docs** — Three corrections from the review of the LinkedIn search-encoding
change (#133), which merged before they could land on it; they ship as a
follow-up rather than being lost.

**A literal `+` was handled but not pinned.** `restliEncode`'s correctness for a
literal plus depends on ORDER: `url.QueryEscape` runs first, turning a real `+`
into `%2B`, and only then does `ReplaceAll` rewrite the space-derived `+` to
`%20`. Correct, but the wire-format table covered `&`, `#`, `%`, the Rest.li
structural characters and CJK — and no `+`. Added `"AI + ML Summit"`. Reverting
to `url.PathEscape`, which leaves `+` alone, now fails that case specifically.

**A comment described the failure backwards.** The new test's rationale said a
bare `+` would be decoded back to a space, so the campaign would be looked up
under the wrong name and the lookup would miss. That is not what happens: the
Rest.li parser reads a bare `+` as a LITERAL plus and rejects the request with
`400 PARAM_INVALID` — the behaviour verified against the live Marketing API at
LinkedIn-Version 202602 and already documented on `restliEncode`. The
distinction is load-bearing in the direction that matters: a 400 ABORTS
find-or-create loudly, while a name miss would be a false absence that silently
creates a duplicate paid campaign. The comment named the safer outcome as the
dangerous one.

**A doc comment overstated what an option does.** `buildRawQuery`'s rationale
claimed `WithBaseURL` "trims its input to host+path". It does not — the body is
`strings.TrimRight(u, "/")`. The true justification is that `baseURL` is a
constant with no query string and every caller passes query data through the
`params` map, so there is nothing on the base to merge. Restated to say that.

Also re-wrapped `load-bearing` in the concept file, which was split across a
hard line break and rendered as `load- bearing`.

**Note on the filename.** An earlier draft of this entry reused
`2026-08-14-LFXV2-3259-review-followups.md`, which already exists and records
#129's findings — overwriting it. One file per entry means a distinct slug, not
just a distinct date and ticket.
