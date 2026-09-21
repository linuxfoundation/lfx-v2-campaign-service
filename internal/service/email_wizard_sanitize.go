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
	// `href` only, and only on <a>. Every other attribute -- style, class, id, and every `on*`
	// handler -- is dropped: none is needed for formatting, and `style` alone carries
	// `expression()` and `url(javascript:)` in enough mail clients to matter.
	dropContent := map[string]bool{"script": true, "style": true, "iframe": true, "object": true, "embed": true}

	var b strings.Builder
	tokenizer := html.NewTokenizer(strings.NewReader(input))
	skipDepth := 0

	for {
		switch tokenizer.Next() {
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
				if tokenizer.Token().Type != html.SelfClosingTagToken {
					skipDepth++
				}
				continue
			}
			if skipDepth > 0 || !allowedTags[tag] {
				continue
			}
			b.WriteString("<" + tag)
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
			if a := atom.Lookup(name); a == atom.Br {
				continue
			}
			b.WriteString("</" + tag + ">")
		}
	}
}

// sanitizeSectionHTML runs a decoded section's `html` field through sanitizeWizardHTML.
//
// Takes and returns `any` because RawSections is the decoded JSON as received -- it is what the
// caller gets back as `sections`, so it cannot be re-typed without changing the wire shape. A
// section that is not an object, or carries no string `html`, is returned untouched: only the
// one field that reaches an HTML sink is rewritten.
func sanitizeSectionHTML(section any) any {
	obj, ok := section.(map[string]any)
	if !ok {
		return section
	}
	raw, ok := obj["html"].(string)
	if !ok {
		return section
	}
	// Copied, not mutated in place: the caller's map may be shared with the typed decode below,
	// and a silent aliasing bug here would be invisible until two sections disagreed.
	out := make(map[string]any, len(obj))
	for k, v := range obj {
		out[k] = v
	}
	out["html"] = sanitizeWizardHTML(raw)
	return out
}
