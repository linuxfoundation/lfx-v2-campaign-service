---
type: "Code Concept"
title: "Email HTML Rewriting and Body Styling (internal/service)"
description: "One tokenizer core rewrites every model-authored HTML block in this package, under two rule sets: the wizard sanitizer keeps formatting and drops the rest, while the email body styler additionally re-dresses the surviving semantic tags in the service's own palette, so the design is fixed in Go rather than written by the model."
resource: "internal/service"
---

# Email HTML Rewriting and Body Styling (`internal/service`)

## Overview

Two fields in this package hold HTML written by a language model and reach sinks that render it:
a wizard `rich_text` block, and an email-copy `rich_text` section. Both are rewritten before they
leave the service, by one shared tokenizer under two different rule sets.

| File | What it owns |
| --- | --- |
| `html_rewrite.go` | `rewriteHTML` — the tokenizer walk, the dropped-content sets, void-tag and self-closing handling, and `hrefAttr` |
| `email_wizard_sanitize.go` | The wizard rule set: a formatting allowlist, `href` on `<a>`, nothing else |
| `email_body_style.go` | The email-body rule set: the same tokenizer, plus this service's palette and type scale |

## Design Principles

* **Rewrite, never escape.** These fields are markup by definition — the wizard's field is what a
  rich-text editor produced — so escaping them would show operators their own tags as literal text.
* **The allowlist is the security boundary; the style table is a design decision.** They are
  separate concerns in the same pass, and only the first one is a vulnerability if wrong.
* **The model cannot style the email even by trying.** Every attribute it wrote is dropped before
  any of ours is added, which is what makes the design fixed rather than advisory.
* **One tokenizer.** A second hand-written HTML walk in this package would be a second place for
  every dropped-content bug to be re-introduced, and the bugs listed below were each found once.

## Key Types and Functions

### `rewriteHTML(input, rules)` (`html_rewrite.go`)

Walks `input` with `golang.org/x/net/html`'s tokenizer and emits only what `rules` admits:
`rules.allowed` names the tags that survive, and `rules.attrs(tag, attrs)` returns the attribute
string — the complete set of bytes — for each surviving open tag.

Attributes are read into a `[]htmlAttr` slice and handed to the emitter, which returns a string.
An emitter therefore **cannot** accidentally copy an attribute through: the only attribute bytes
in the output are the ones it returns. That is a property of the signature, not of the emitters'
care, and it is why both rule sets are a few lines each.

Three behaviours are **fixed in the core and not configurable**, because every one of them was a
bug before it was a rule:

* **`dropContent`** (`script`, `style`, `iframe`, `object`, `embed`) drops the tag *and* its text.
  Dropping only the tag leaves `alert(1)` as visible copy.
* **`rawTextDrop`** (`script`, `style`, `iframe`) tracks a skip depth, because the tokenizer treats
  those elements' contents as raw text rather than as markup.
* A **self-closing** non-void tag is closed in the output, so `<p/>` cannot swallow the rest of the
  document, and `<br>` is never given a closing tag.

### `sanitizeWizardHTML(input)` (`email_wizard_sanitize.go`)

`rewriteHTML` with `wizardAllowedTags` (prose and emphasis: headings, `p`, lists, `blockquote`,
`strong`/`em`/`u`, `a`, `span`, `div`) and `wizardAttrs`, which keeps `href` on `<a>` and nothing
else. Called from `sanitizeSectionHTML`, which drops a non-object section entirely and leaves an
object with no string `html` untouched — `divider` and `button` sections legitimately carry none.

This file used to contain the tokenizer loop. It was **extracted** rather than copied when the body
styler needed the same walk; the existing wizard sanitizer tests passed unchanged against the
extracted core, which is the evidence that the extraction preserved behaviour.

### `styleEmailBodyHTML(input)` (`email_body_style.go`)

The same walk under `emailBodyAllowedTags` and `emailBodyAttrs`. It is a sanitizer first and a
styler second, in that order: `style`, `class`, `id`, `width` and every `on*` handler are dropped,
and then the service's own `style` attribute is written onto the surviving tag.

`emailBodyTagStyles` is the one place a body tag's appearance is written down, and
`emailBodyAllowedTags` is **derived from it** so the two cannot disagree — adding a tag is one map
entry, not two. A tag mapped to `""` (`br`, `span`, `div`) is allowed and deliberately unstyled,
which is not the same as being absent from the map.

Styling for email, not for the web: inline attributes only (several clients strip `<style>`
blocks), no `rem`, no custom properties, bottom-only margins (top and bottom margins collapse in a
browser and do **not** collapse in several mail clients, which doubles every gap), and `<li>`
repeats its family, size and colour because Outlook resets list-item type inside a styled `<ul>`.

`emailLinkColor` is `#2563eb`, the colour `addButtonSection` already renders the CTA button in
(`internal/platform/hubspot/content.go`), so a text link and the button read as the same
affordance. The two cannot share a constant: `internal/service` already imports
`internal/platform/hubspot`, so a constant in the other direction would be an import cycle.
`TestEmailBodyPaletteMatchesTheCTAButton` pins the literal instead and says why.

An `<a>` whose `href` does not survive `httpURL` gets **no style and no target**, so it renders as
the plain copy it effectively is. An anchor left looking like a link while leading nowhere is a lie
the reader clicks; this mirrors the wizard's rule that a button with nowhere to go renders as text.

### `styledBodyHTMLWithinBound(html, bound)`

Styles `html`, and returns the **unstyled** input if the result would exceed `bound` runes.

Styling inflates its input by roughly 100-150 bytes per tag, so a section near the per-section
limit can style past it. That must never turn a valid model response into an error — the operator
asked for copy, not for a lecture about CSS budgets. An unstyled section in an otherwise styled
email is a visible imperfection; a 503 is a broken feature. The caller has already rejected `html`
for exceeding `bound`, so the fallback is in bounds by construction.

### Where the styler is applied, and why there

In `parseEmailCopyResponse`'s `rich_text` branch — at generation time, in this package.

That is the one place model-authored body HTML becomes a response, and it is upstream of the fork
that matters: the BFF flattens the response's `sections[]` into the single `body` string that
**both** the operator's preview renders and the HubSpot draft is built from. Styling here is
therefore what makes the preview show what the recipient will see. Styling in
`addBodySection` (`internal/platform/hubspot/content.go`) instead would leave the preview
permanently unstyled, or require the same palette written a second time in TypeScript.

`bodyStyleRule` is the prompt half of the same contract, kept in this file beside the styler rather
than inline in the prompt so the two cannot drift: it tells the model, in the words the code
enforces, to write semantic tags and no styling. A tag named in the rule but missing from
`emailBodyTagStyles` would be silently dropped from the email, which is the failure that is
invisible in review — `TestBodyStyleRuleNamesOnlyStyledTags` holds that seam. The rule ships on the
stage-aware prompt path only; LFXV2-1940 freezes the no-stage prompt byte for byte.

## Error Handling

`rewriteHTML` never returns an error and never panics on malformed input: the tokenizer's
`ErrorToken` terminates the walk and whatever was emitted so far is returned. Unparseable markup
therefore degrades to less output, never to a failed request.

## Testing

Wizard-sanitizer coverage lives in `email_wizard_sanitize_test.go` and was written against the
pre-extraction implementation; it is the regression test for `rewriteHTML` itself.
`email_body_style_test.go` covers the styler:

- **TestStyleEmailBodyHTMLStylesSemanticTags**: Each semantic tag the prompt asks for comes back
  carrying the table's style for that tag, built from the table rather than from a frozen literal —
  the VALUE of a style is a design decision a test must not freeze, but its SHAPE is a contract.
- **TestStyleEmailBodyHTMLStylesTheDividerWithoutClosingIt**: `<hr>`, `<hr/>` and `<hr />` all
  produce one styled void tag with no closing tag.
- **TestStyleEmailBodyHTMLReplacesModelAuthoredStyling**: A model-written `style` attribute is
  replaced by the service's, not merged with it.
- **TestStyleEmailBodyHTMLDropsScriptsHandlersAndStyleBlocks**: Script content, `on*` handlers and
  `<style>` blocks leave no trace, including the self-closing `<script/>` case.
- **TestStyleEmailBodyHTMLDropsImages**: An `<img>` is dropped, which covers a tracking pixel.
- **TestStyleEmailBodyHTMLKeepsTheTextOfDroppedTags**: A layout `<table>` loses its tags and keeps
  its text — dropping the content would delete real copy.
- **TestStyleEmailBodyHTMLStylesAValidLink** / **TestStyleEmailBodyHTMLRefusesANonHTTPHref**: A
  valid link gets href, target and style; `javascript:`, `data:`, `mailto:`, a relative path and an
  embedded-credentials URL each yield a bare `<a>` with none of the three.
- **TestStyleEmailBodyHTMLIsIdempotent**: Running the styler over its own output yields the same
  bytes. Idempotence is by construction (model attributes are dropped before ours are added), and
  this is what holds that construction.
- **TestEmailBodyStyleTableIsInert**: No entry contains a quote, an angle bracket, an ampersand, a
  backslash or `url(`. `styleAttr` does not escape its argument, which is sound only because every
  value is a constant in the file; this test fails the moment that stops being true.
- **TestEmailBodyPaletteMatchesTheCTAButton**: Pins `#2563eb` against `content.go` and records the
  import cycle that stops the two sharing a constant.
- **TestBodyStyleRuleNamesOnlyStyledTags**: Splits `bodyStyleRule` at its "Never write" sentence
  and checks that every tag named in the permitted half IS styled and every tag in the forbidden
  half is NOT. It fails loudly if that split string ever disappears, rather than silently checking
  nothing.
- **TestEmailBodyAllowedTagsTracksTheStyleTable**: The derived allowlist and the style table have
  the same keys.
- **TestStyledBodyHTMLWithinBoundFallsBackWhenStylingOverflows** /
  **...StylesWhatFits** / **...CountsRunesNotBytes**: The fallback fires on overflow, does not fire
  otherwise, and measures runes rather than bytes.

## Architectural Notes

The palette lives in `internal/service`, not in `internal/platform/hubspot`, and the direction is
forced: `internal/service` already imports the hubspot package in four non-test files, so a styler there
that this package called would be a cycle. Placing it here also keeps `content.go` — whose rendered
HubSpot layout is verified against a real draft — untouched by this change.
