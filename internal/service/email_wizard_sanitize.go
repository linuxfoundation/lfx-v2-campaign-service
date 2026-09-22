// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// sanitizeWizardHTML strips everything from a rich_text block that is not formatting.
//
// The block's HTML is markup by definition -- it is what the UI's rich-text editor produces, so
// escaping it would show operators their own tags as literal text. But the same field is also
// filled by a MODEL, and it reaches two sinks that execute or render it: the preview document
// served to the operator's browser, and the HubSpot draft body sent to recipients. Bounding its
// length was the only check, which stops nothing.
//
// An ALLOW-LIST, not a denylist: tags and attributes not named here are dropped, so a tag nobody
// anticipated fails closed rather than passing. Element CONTENT is preserved even when the tag is
// not -- dropping `<span>` should not delete the words inside it -- except for <script> and
// <style>, whose content is the payload rather than copy.
//
// Parsed with a real tokenizer rather than regex: `<scr<script>ipt>` and attribute-boundary
// tricks defeat pattern matching, and this input is adversarial by assumption.
func sanitizeWizardHTML(input string) string {
	allowedTags := map[string]bool{
		"p": true, "br": true, "strong": true, "b": true, "em": true, "i": true,
		"u": true, "ul": true, "ol": true, "li": true, "blockquote": true,
		"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
		"a": true, "span": true, "div": true,
	}
	dropContent := map[string]bool{"script": true, "style": true, "iframe": true, "object": true, "embed": true}

	var b strings.Builder
	tokenizer := html.NewTokenizer(strings.NewReader(input))
	skipDepth := 0

	for {
		tokenType := tokenizer.Next()
		switch tokenType {
		case html.ErrorToken:
			return b.String()

		case html.TextToken:
			if skipDepth == 0 {
				b.WriteString(html.EscapeString(string(tokenizer.Text())))
			}

		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := tokenizer.TagName()
			tag := string(name)
			if dropContent[tag] {
				// A self-closing spelling does NOT end the drop. None of these tags is a void
				// element, so `<script/>` is not self-closing in HTML at all -- the parser treats
				// it as an open tag and everything after it as script content, right up to a
				// `</script>` that may never come. Skipping the depth bump let that content out
				// as escaped text, and escaped the REST of the document with it:
				// `<p>a</p><script/>alert(1)<p>b</p>` emitted `alert(1)&lt;p&gt;b&lt;/p&gt;`.
				//
				// `tokenType`, not `tokenizer.Token()`: calling Token() mid-iteration re-reads the
				// current token and is easy to get wrong here. The value is already in hand.
				skipDepth++
				continue
			}
			if skipDepth > 0 || !allowedTags[tag] {
				continue
			}
			b.WriteString("<" + tag)
			// `href` only, and only on <a>. Every other attribute -- style, class, id, and every `on*`
			// handler -- is dropped: none is needed for formatting, and `style` alone carries
			// `expression()` and `url(javascript:)` in enough mail clients to matter.
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = tokenizer.TagAttr()
				if tag != "a" || string(k) != "href" {
					continue
				}
				// The SAME httpURL check every other link in this file goes through, so a
				// `javascript:` or `data:` href cannot reach an anchor here either.
				if href := httpURL(string(v)); href != "" {
					b.WriteString(` href="` + html.EscapeString(href) + `"`)
				}
			}
			b.WriteString(">")
			// A self-closing NON-VOID tag closes itself here, because no EndTagToken will ever
			// arrive for it: `<div/>` emitted a bare `<div>` that nothing closed, leaving the
			// rest of the email nested inside it. Void elements (br, img and friends) are
			// correct unclosed, so only the others get a closer.
			if tokenType == html.SelfClosingTagToken && !voidTags[tag] {
				b.WriteString("</" + tag + ">")
			}

		case html.EndTagToken:
			name, _ := tokenizer.TagName()
			tag := string(name)
			if dropContent[tag] {
				if skipDepth > 0 {
					skipDepth--
				}
				continue
			}
			if skipDepth > 0 || !allowedTags[tag] {
				continue
			}
			// `</br>` is swallowed rather than emitted: `br` is a VOID element, so a closing tag
			// for it is invalid HTML that some clients render as a second line break. Every
			// other allowed tag needs its closer, which is why this is the one exception.
			if a := atom.Lookup(name); a == atom.Br {
				continue
			}
			b.WriteString("</" + tag + ">")
		}
	}
}

// voidTags are the HTML elements that have no end tag, so a self-closing spelling of one needs
// no synthesised closer. Only those in allowedTags can actually appear, but the full set is
// listed so the rule reads as "void elements" rather than "the two we happen to allow".
var voidTags = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true,
	"img": true, "input": true, "link": true, "meta": true, "source": true, "track": true, "wbr": true,
}

// sanitizeSectionHTML runs a decoded section's `html` field through sanitizeWizardHTML,
// reporting whether the section may be kept at all.
//
// Takes and returns `any` because RawSections is the decoded JSON as received -- it is what the
// caller gets back as `sections`, so it cannot be re-typed without changing the wire shape.
//
// A section that is not an OBJECT is dropped, not passed through. The first version returned it
// untouched, which is fail-OPEN: nothing upstream requires `sections[i]` to be an object, so a
// model emitting the bare string `"<script>alert(1)</script>"` had it copied verbatim into the
// response while every object section beside it was sanitized. `decodeWizardSections` already
// ignores non-object entries, so dropping one loses nothing a consumer could render -- it only
// removes something no consumer should have been handed.
//
// An object with no string `html` is KEPT as-is: `divider` and `button` sections legitimately
// carry no HTML, and dropping them would silently delete real content. Only the one field that
// reaches an HTML sink is rewritten.
func sanitizeSectionHTML(section any) (any, bool) {
	obj, ok := section.(map[string]any)
	if !ok {
		return nil, false
	}
	raw, ok := obj["html"].(string)
	if !ok {
		return section, true
	}
	// Copied, not mutated in place: the caller's map may be shared with the typed decode below,
	// and a silent aliasing bug here would be invisible until two sections disagreed.
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		out[k] = v
	}
	out["html"] = sanitizeWizardHTML(raw)
	return out, true
}
