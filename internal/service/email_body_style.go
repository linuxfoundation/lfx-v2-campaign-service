// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

// The email body's palette and type scale, owned by this service rather than by the model.
//
// The model is told to write SEMANTIC html -- <h2>, <p>, <ul>, <strong>, <a> -- and nothing else;
// styleEmailBodyHTML below dresses those tags. That split is deliberate: a model asked to write
// its own inline CSS produces a different design on every generation, and the two variants of one
// A/B test then differ in appearance as well as in copy, which makes the test unreadable. Fixing
// the design here means the A/B test measures the framing, which is the only thing that varies.
//
// The hexes are not invented here. `#2563eb` is the colour addButtonSection already renders the
// CTA button in (internal/platform/hubspot/content.go), chosen there for WCAG AA contrast against
// white -- links in the body use the same blue so a text link and the button read as the same
// affordance. The inks are the body/heading pair the wizard preview already uses.
//
// Email, not web: values are inline `style` attributes with no shorthand a mail client is likely
// to drop, no `rem`, no custom properties, no classes. Several clients strip <style> blocks
// entirely, so anything not inline is not styling at all.
const (
	emailInkColor      = "#1b1b1f" // body copy
	emailHeadingColor  = "#0f172a" // headings and bolded lead-ins, a shade darker than the ink
	emailLinkColor     = "#2563eb" // links, matching the CTA button in content.go
	emailQuoteColor    = "#334155" // pulled-quote copy, de-emphasised against the body ink
	emailQuoteFill     = "#f1f5f9" // pulled-quote background
	emailRuleColor     = "#e5e7eb" // <hr> dividers between major blocks
	emailFontStack     = "Arial, Helvetica, sans-serif"
	emailMonoFontStack = "Consolas, Monaco, monospace"
)

// emailBodyTagStyles is the one place a body tag's appearance is written down.
//
// A tag mapped to "" is emitted with no `style` attribute, which is not the same as being absent
// from the map: absent means "this tag is not in emailBodyAllowedTags and never reaches here",
// mapped-to-empty means "allowed, deliberately unstyled, inherits its parent". <span> and <div>
// are the latter -- they carry no meaning of their own, so giving them a look would only fight
// whatever they wrap.
//
// Margins are bottom-only (`margin:0 0 Npx`) because a top margin and a bottom margin collapse in
// a browser and do NOT collapse in several mail clients, which doubles the gap between every pair
// of blocks. One direction can never collapse wrongly.
var emailBodyTagStyles = map[string]string{
	"h1": "margin:0 0 12px;font-family:" + emailFontStack + ";font-size:26px;line-height:132%;font-weight:bold;color:" + emailHeadingColor + ";",
	"h2": "margin:0 0 10px;font-family:" + emailFontStack + ";font-size:21px;line-height:136%;font-weight:bold;color:" + emailHeadingColor + ";",
	"h3": "margin:0 0 8px;font-family:" + emailFontStack + ";font-size:17px;line-height:140%;font-weight:bold;color:" + emailHeadingColor + ";",
	// h4-h6 share one look. The model is asked for h2/h3; the deeper levels exist so a stray one
	// renders as a heading instead of losing its tag and merging into the paragraph above.
	"h4": "margin:0 0 8px;font-family:" + emailFontStack + ";font-size:15px;line-height:140%;font-weight:bold;letter-spacing:0.02em;color:" + emailHeadingColor + ";",
	"h5": "margin:0 0 8px;font-family:" + emailFontStack + ";font-size:15px;line-height:140%;font-weight:bold;letter-spacing:0.02em;color:" + emailHeadingColor + ";",
	"h6": "margin:0 0 8px;font-family:" + emailFontStack + ";font-size:15px;line-height:140%;font-weight:bold;letter-spacing:0.02em;color:" + emailHeadingColor + ";",

	"p":  "margin:0 0 14px;font-family:" + emailFontStack + ";font-size:16px;line-height:160%;color:" + emailInkColor + ";",
	"ul": "margin:0 0 14px;padding:0 0 0 22px;font-family:" + emailFontStack + ";font-size:16px;line-height:160%;color:" + emailInkColor + ";",
	"ol": "margin:0 0 14px;padding:0 0 0 22px;font-family:" + emailFontStack + ";font-size:16px;line-height:160%;color:" + emailInkColor + ";",
	// The <li> repeats the family, size and colour rather than inheriting them from its list,
	// because Outlook resets list-item type inside a <ul> that carries its own font rules.
	"li": "margin:0 0 7px;font-family:" + emailFontStack + ";font-size:16px;line-height:155%;color:" + emailInkColor + ";",

	"blockquote": "margin:0 0 16px;padding:12px 16px;border-left:4px solid " + emailLinkColor + ";background-color:" + emailQuoteFill + ";font-family:" + emailFontStack + ";font-size:16px;line-height:155%;font-style:italic;color:" + emailQuoteColor + ";",
	"hr":         "border:0;border-top:1px solid " + emailRuleColor + ";margin:22px 0;height:1px;",

	"strong": "font-weight:bold;color:" + emailHeadingColor + ";",
	"b":      "font-weight:bold;color:" + emailHeadingColor + ";",
	"em":     "font-style:italic;",
	"i":      "font-style:italic;",
	"u":      "text-decoration:underline;",
	"code":   "font-family:" + emailMonoFontStack + ";font-size:14px;",

	"a": "color:" + emailLinkColor + ";font-weight:bold;text-decoration:underline;",

	"br":   "",
	"span": "",
	"div":  "",
}

// emailBodyAllowedTags is derived from emailBodyTagStyles so the two cannot disagree: a tag is
// allowed exactly when this file says how it looks. Adding a tag is one map entry, not two.
var emailBodyAllowedTags = func() map[string]bool {
	allowed := make(map[string]bool, len(emailBodyTagStyles))
	for tag := range emailBodyTagStyles {
		allowed[tag] = true
	}
	return allowed
}()

// styleEmailBodyHTML rewrites a model-authored rich_text block into the service's own design.
//
// It is a sanitizer first and a styler second, and in that order: every attribute the model wrote
// is dropped -- `style`, `class`, `id`, `width`, and every `on*` handler -- and the only attributes
// in the output are the ones this file puts there. So the model cannot style the email even by
// trying, which is what makes the design fixed rather than advisory, and the usual payloads
// (`onerror`, `style="...url(javascript:)"`, a <script> between two paragraphs) are removed by the
// same pass. The tokenizer walk and the dropped-content handling are rewriteHTML's, shared with
// sanitizeWizardHTML.
//
// Not idempotent-by-accident but idempotent by construction: since model attributes are dropped
// before ours are added, running this over its own output yields the same bytes
// (TestStyleEmailBodyHTMLIsIdempotent).
func styleEmailBodyHTML(input string) string {
	return rewriteHTML(input, htmlRewriteRules{
		allowed: emailBodyAllowedTags,
		attrs:   emailBodyAttrs,
	})
}

// emailBodyAttrs writes the href (validated), the link target, and the service's style, in that
// order, and nothing else.
//
// An <a> whose href does not survive httpURL gets NO style and no target, so it renders as the
// plain copy it effectively is: a browser and a mail client both style only anchors that have an
// href, so an anchor left looking like a link while leading nowhere would be a lie the reader
// clicks. This mirrors the wizard's button rule -- a button with nowhere to go is rendered as text.
//
// `target="_blank" rel="noopener noreferrer"` on a real link because this HTML renders in two
// places that both need it: a mail client that opens links in place would otherwise replace the
// message, and the operator's preview renders in a sandboxed iframe where an in-frame navigation
// would swap the preview for the event site.
func emailBodyAttrs(tag string, attrs []htmlAttr) string {
	if tag == "a" {
		href := hrefAttr(attrs)
		if href == "" {
			return ""
		}
		return href + ` target="_blank" rel="noopener noreferrer"` + styleAttr(emailBodyTagStyles[tag])
	}
	return styleAttr(emailBodyTagStyles[tag])
}

// styleAttr formats one style declaration, or nothing for a deliberately unstyled tag.
//
// The value is NOT escaped because it is not caller data: every value comes from
// emailBodyTagStyles above, which is a constant table in this file. TestEmailBodyStyleTableIsInert
// holds that line -- it fails if any entry ever grows a quote or an angle bracket, which is the
// only way this could start needing escaping.
func styleAttr(style string) string {
	if style == "" {
		return ""
	}
	return ` style="` + style + `"`
}

// styledBodyHTMLWithinBound styles `html` and returns the result only if it still fits `bound`
// runes, falling back to the UNSTYLED html otherwise.
//
// Styling inflates its input: every tag gains an inline style of roughly 100-150 bytes, so a
// section near the per-section limit can style past it. That must never turn a valid model
// response into an error -- the operator asked for copy, not for a lecture about CSS budgets -- so
// the overflow case degrades to the semantic HTML, which is what shipped before this file existed
// and is already known to fit. An unstyled section in an otherwise styled email is a visible
// imperfection; a 503 is a broken feature.
//
// The caller has already rejected `html` for exceeding `bound`, so the fallback is in bounds by
// construction.
func styledBodyHTMLWithinBound(html string, bound int) string {
	styled := styleEmailBodyHTML(html)
	if len([]rune(styled)) > bound {
		return html
	}
	return styled
}

// bodyStyleRule is the prompt half of the contract this file implements: the model is told, in the
// same words the code enforces, that markup is semantic and styling is not its job.
//
// Kept here beside the styler rather than inline in the prompt so the two cannot drift -- a tag
// named here but missing from emailBodyTagStyles would be silently dropped from the email, which
// is exactly the failure that is invisible in review (TestBodyStyleRuleNamesOnlyStyledTags).
//
// Goes on the stage-aware prompt path only. LFXV2-1940 freezes the no-stage prompt byte for byte.
const bodyStyleRule = `- Markup is SEMANTIC, styling is not yours to write: use <h2>/<h3> for headings, <p> for
  paragraphs, <ul>/<ol>/<li> for lists, <strong> for a bolded lead-in, <em> for light emphasis,
  <blockquote> for a pulled quote, <hr> between two major blocks, and <a href="..."> for a link.
  Never write a style= or class= attribute, a <style> block, a <font> tag, a layout <table> or an
  <img> -- they are stripped, and this service applies the Linux Foundation's own fonts, colours
  and spacing to the tags above. Emphasis comes from structure (a heading, a short paragraph, a
  tight list), never from CAPITALS or rows of emoji`
