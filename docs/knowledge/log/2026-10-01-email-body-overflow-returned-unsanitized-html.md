# 2026-10-01 Email body overflow returned unsanitized model HTML

**Fix** — `styledBodyHTMLWithinBound` (`internal/service/email_body_style.go`)
returned `html` — the model's bytes exactly as received — whenever styling the
section pushed it past `bound`. `styleEmailBodyHTML` is the ONLY sanitizer on
the `GenerateEmailCopy` response path (`sanitizeWizardHTML` guards the wizard
and never runs here, confirmed by its two call sites in
`email_wizard_sanitize.go` and `email_wizard_sections.go`), and
`styledBodyHTMLWithinBound` is its only non-test caller, so that one branch was
the whole allow-list going away: `onerror`, a `<script>` between two
paragraphs, a `url(javascript:)` style and a `javascript:` href all reached the
API response through it.

Worse than a rare edge: the model writes the section and therefore writes its
LENGTH. Padding a section to within a few hundred runes of `maxHTMLRunes`
(8000) makes styling — roughly 100-150 runes per tag — overflow on demand, so
the model chose whether the sanitizer ran at all. That is a reliable bypass,
not an accident.

The overflow form is now `sanitizeEmailBodyHTML`: the same tokenizer, the same
`emailBodyAllowedTags`, the same dropped content and the same `httpURL` check as
the styled pass, with no `style` attribute anywhere in the output. Both emitters
share `emailBodyLinkAttrs` rather than restating the anchor rule, because a
difference between them would only ever show on sections long enough to
overflow — the one length nobody reviews. Styling remains the half that may be
dropped; the allow-list is not.

The sanitized form is NOT in bounds by construction, which is why the signature
is now `(string, error)`. `rewriteHTML` escapes text, and `&`, `'` and `"` each
become five runes, so a section of ampersands -- or ordinary prose with a few
apostrophes -- can clear `bound` as written while exceeding it sanitized. There
is then no form of that section this service can safely send. The sole call site
of `styledBodyHTMLWithinBound`, in `parseEmailCopyResponse`, wraps the error with
the section's index so it reads like the "model response is unusable" rejections
its siblings in that function return. That phrasing lands in the LOG, not on the wire: the sole
caller (`GenerateEmailCopy`) logs it and answers with a fixed
`ConnServiceUnavailableError` whose message is "the AI platform returned an
unreadable response", so this text is the only record of which section was
unusable and why.

The replaced test could not have caught this. `TestStyledBodyHTMLWithinBoundFallsBackWhenStylingOverflows`
used `const in = "<p>hi</p>"` with a bound of 9 and asserted the fallback
returned its input — but `<p>hi</p>` sanitizes to itself, so the assertion held
whether the fallback sanitized or leaked. Its replacement,
`TestStyledBodyHTMLWithinBoundSanitizesWhatItCannotStyle`, runs four real
payloads through a 200-paragraph section at `maxHTMLRunes` itself and asserts
its own premise first (input inside the bound, styled form outside it), so it
cannot pass without reaching the overflow branch.
`TestStyledBodyHTMLWithinBoundErrorsWhenEscapingOverflows` pins the new error.
`docs/knowledge/code/internal-service-email-html.md` was updated to match.
