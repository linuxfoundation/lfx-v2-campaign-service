// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

// sanitizeWizardHTML strips everything from a rich_text block that is not formatting.
//
// The block's HTML is markup by definition -- it is what the UI's rich-text editor produces, so
// escaping it would show operators their own tags as literal text. But the same field is also
// filled by a MODEL, and it reaches two sinks that execute or render it: the preview document
// served to the operator's browser, and the HubSpot draft body sent to recipients. Bounding its
// length was the only check, which stops nothing.
//
// The tokenizer walk, the dropped-content set and the raw-text/self-closing handling live in
// rewriteHTML, which this and styleEmailBodyHTML share; the rules below are the whole of what is
// specific to the wizard. Read rewriteHTML for why each of those steps is the way it is.
func sanitizeWizardHTML(input string) string {
	return rewriteHTML(input, htmlRewriteRules{
		allowed: wizardAllowedTags,
		attrs:   wizardAttrs,
	})
}

// wizardAllowedTags is the formatting subset a rich_text block may keep. Nothing structural
// (table, img, form) and nothing that carries behaviour: a block is prose with emphasis.
var wizardAllowedTags = map[string]bool{
	"p": true, "br": true, "strong": true, "b": true, "em": true, "i": true,
	"u": true, "ul": true, "ol": true, "li": true, "blockquote": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"a": true, "span": true, "div": true,
}

// wizardAttrs keeps `href`, and only on <a>. Every other attribute -- style, class, id, and every
// `on*` handler -- is dropped: none is needed for formatting, and `style` alone carries
// `expression()` and `url(javascript:)` in enough mail clients to matter.
func wizardAttrs(tag string, attrs []htmlAttr) string {
	if tag != "a" {
		return ""
	}
	return hrefAttr(attrs)
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
