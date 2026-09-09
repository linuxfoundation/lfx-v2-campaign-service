# 2026-09-09 — LFXV2-2775: generated copy lands in the draft's first rich-text block

**Fix** — Operator-approved email copy was being discarded on almost every real template, silently.
The body write was guarded on the draft having EXACTLY ONE rich-text widget; an LF template carries
roughly nine, so the guard fired, logged at INFO, and the generated body never reached the draft.
Nothing surfaced as an error, and the campaign looked staged.

The guard's reasoning was sound and its premise was wrong. It refused because there was no way to
know which block the body belonged in — true of the widget MAP, which is a JSON object whose Go
iteration order is randomised, and true of the per-widget `order` field, which is absent on every
drag-and-drop template observed. `content.flexAreas` is the one place a draft records the reading
order of its blocks, so reading it answers the question the guard could not, and the refusal is no
longer needed: the body goes to the first rich-text block in layout order.

That last sentence is REFINED, on the same branch, by
`2026-09-09-LFXV2-2775-the-first-block-needs-a-layout.md`: it holds only where a layout exists. A
classic (non-drag-and-drop) template has no `flexAreas` at all, so its blocks come back in sorted
key order and the "first" is as likely the unsubscribe footer as the lede — the write now requires
a layout-PLACED first block, and a draft without one keeps its template body.

`SetEmailHTMLWidgets` also changed shape, and this half is the one that would have destroyed data
rather than dropped it. It now READS the draft and PATCHes the whole `content` object back with
only `body.html` changed, every other byte re-sent verbatim. HubSpot treats submitted content as
AUTHORITATIVE on a drag-and-drop email rather than merging it, so the previous partial write did
not update the draft in place — it replaced it. Verified against a live LF portal draft: with the
fix the draft is 91.2KB with its banner and all blocks intact, and on the previous code the same
dispatch left a 21.8KB draft with an empty body.

That read is also what opens a LOST-UPDATE window, since the read and the write are two requests —
see `2026-09-09-LFXV2-2775-the-draft-write-has-a-lost-update-window.md` for why it is documented
rather than closed (the read cannot be dropped without destroying the draft, and Marketing Emails
v3 exposes no precondition to condition the write on).

A validated `registrationUrl` now reaches the email-copy prompt, sourced from the brief's `url`
column with a nested `event_details.registrationUrl` fallback. It is VALIDATED rather than trimmed
because it is interpolated into a prompt whose output goes straight into an `href`: a relative
path, a bare hostname or a `javascript:` scheme would be pasted into a marketing email verbatim.
An unusable value resolves to absent, and the prompt's link rule then has the model write the call
to action as plain text — a working email with no button, rather than one with a hostile link.

The validation described here was later found INCOMPLETE and is extended by
`2026-09-09-LFXV2-2775-registration-url-is-an-href.md`: `url.Parse` is structural, so it also
admitted a hostless `https://:443/path`, embedded `user:password@` credentials, and raw quotes in
the path, query or fragment. The value is now normalised rather than passed through, refused for
embedded credentials or a malformed query escape, and gated on raw size before parsing.
Previously absent was the only case, and the model filled the gap with `href='#'`, so the primary
CTA was a dead link the UTM tagger could not tag either.

Two defects found in review of the fix itself, both fixed here:

- `widgetWithHTML` panicked on `"body": null`. JSON `null` is four bytes, so the `len() > 0` guard
  passes; `json.Unmarshal` then nils the map and returns NO error; the write panics. A panic is not
  an error — `applyEmailContent` is best-effort and swallows failures, but a panic unwinds past
  that to the orchestrator's recover and fails the whole dispatch, orphaning the draft that
  contract exists to protect. Reachable despite `htmlBlocks` filtering null-bodied widgets out of
  selection, because the read and the write are separate requests and an operator can edit the
  draft in between.
- `TestComposedBoundClearsEveryStageFloor` measured the floor with an EMPTY registrationURL, so the
  19-rune `"\nRegistration URL: "` label fell outside the bound the test exists to guard. The worst
  valid composition is 8764, not 8745. No live 503 — 9300 clears both — but the guard had a blind
  spot for the field the same commit introduced. `worstStageFloor` now composes with a URL, which
  propagates to `TestConceptDocSizingArithmetic` and caught three stale figures in the concept doc.

Authored by Vinay U; carried onto main and reviewed here.
