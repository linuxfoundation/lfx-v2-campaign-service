// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// htmlAttr is one attribute exactly as the tokenizer read it, before any validation.
type htmlAttr struct {
	key string
	val string
}

// htmlRewriteRules is the per-caller half of rewriteHTML: which tags survive, and what
// attributes each surviving start tag is written with.
//
// Only these two things are per-caller. The security properties below -- the allow-list being an
// allow-list at all, which tags lose their CONTENT as well as their tag, and the raw-text
// self-closing handling -- live in rewriteHTML and are not configurable, because every one of them
// was a bug before it was a rule and no caller has a legitimate reason to differ.
type htmlRewriteRules struct {
	// allowed names the tags that survive. A tag not named here is dropped while its text content
	// is kept, so dropping an unknown wrapper never deletes the words inside it.
	allowed map[string]bool

	// attrs returns the attribute text for one surviving start tag, INCLUDING a leading space for
	// each attribute it emits, or "" for none. It is the ONLY way an attribute reaches the output:
	// nothing the tokenizer read is copied through. A nil attrs emits every allowed tag bare.
	//
	// It is called with the attributes as read, unvalidated and unescaped -- validating them is
	// its job, and anything it returns is written verbatim.
	attrs func(tag string, attrs []htmlAttr) string
}

// rewriteHTML tokenizes `input` and rebuilds it under `rules`, dropping everything else.
//
// This is the shared core of THREE rule sets, not two: sanitizeWizardHTML (keeps formatting and
// nothing else), styleEmailBodyHTML (additionally re-dresses the surviving tags in the service's
// own inline styles) and sanitizeEmailBodyHTML (the same email-body allow-list with no style
// attribute at all, used when the styled form will not fit). They differ only in their allow-list
// and their attribute emitter; the tokenizer walk, the dropped-content set and the
// self-closing/raw-text handling below are identical, and were extracted here rather than copied so
// the comments recording what each line is for cannot drift out of sync with a second or third
// copy.
//
// An ALLOW-LIST, not a denylist: tags and attributes not named by the rules are dropped, so a tag
// nobody anticipated fails closed rather than passing. Element CONTENT is preserved even when the
// tag is not -- dropping `<span>` should not delete the words inside it -- except for the
// dropContent set below, whose content is the payload rather than copy.
//
// Parsed with a real tokenizer rather than regex: `<scr<script>ipt>` and attribute-boundary tricks
// defeat pattern matching, and this input is adversarial by assumption.
func rewriteHTML(input string, rules htmlRewriteRules) string {
	dropContent := map[string]bool{"script": true, "style": true, "iframe": true, "object": true, "embed": true}
	// The RAW-TEXT subset, and the distinction is load-bearing for the self-closing case below.
	// `golang.org/x/net/html` force-consumes the tail of these three as a single text token, so
	// `<script/>` really does open a region that only `</script>` closes. `object` and `embed`
	// get no such treatment -- the tokenizer resumes normal tokenization right after them --
	// verified against the tokenizer directly, not inferred from the spec.
	rawTextDrop := map[string]bool{"script": true, "style": true, "iframe": true}

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
				// Only the RAW-TEXT tags open a region a self-close does not end. For those the
				// tokenizer force-consumes everything after `<script/>` as script content, up to
				// a `</script>` that may never come -- so without the bump that payload escaped
				// as text AND took the rest of the document with it:
				// `<p>a</p><script/>alert(1)<p>b</p>` emitted `alert(1)&lt;p&gt;b&lt;/p&gt;`.
				//
				// `object` and `embed` must NOT bump here. The tokenizer resumes normal
				// tokenization after them, so no `</object>` ever arrives, `skipDepth` never
				// returns to 0, and every following paragraph is suppressed with no error and no
				// marker: `<p>before</p><object/><p>after</p>` lost `<p>after</p>` entirely.
				// Bumping for all five was this file's own previous fix over-generalising from
				// the raw-text tags -- a truncation traded for a leak.
				//
				// `tokenType`, not `tokenizer.Token()`: calling Token() mid-iteration re-reads the
				// current token and is easy to get wrong here. The value is already in hand.
				// ONLY the raw-text tags, in either spelling. This line has been wrong three
				// times, each time by naming the wrong property:
				//
				//   1. no bump at all          -> `<script/>` leaked its payload as text
				//   2. bump for all five       -> `<object/>` ate the rest of the block
				//   3. bump unless self-closing -> a BARE `<embed src=x>` ate it just the same
				//
				// The property is the TAG, never how it was written. Verified against the
				// tokenizer: `<p>a</p><embed src=x><p>b</p>` emits
				// `StartTag:embed, StartTag:p, Text, EndTag:p` -- the tail is never embed
				// content, so there is no region to hold open and a bump can only dangle.
				// script/style/iframe ARE raw text: the tokenizer force-consumes their tail as
				// one text token, so for them the bump is what stops the payload escaping.
				//
				// A closed `<object>payload</object>` KEEPS its text, like any other disallowed
				// wrapper -- there was never a region to suppress, so the words are copy. That
				// is safe because the allow-list does the work: a `<script>` inside one is still
				// a dropContent tag and an `onerror` is still an attribute nobody allows, so
				// only inert words survive (TestSanitizeWizardHTMLObjectPayloadIsInert).
				if rawTextDrop[tag] {
					skipDepth++
				}
				continue
			}
			if skipDepth > 0 || !rules.allowed[tag] {
				continue
			}
			// The attributes are read into a slice before the emitter sees them because
			// `tokenizer.TagAttr()` is only valid while the tokenizer sits on this token, and an
			// emitter that wants to look at two attributes together (an `href` and the tag it
			// belongs to) cannot do that from a streaming callback. Reading them here also means
			// an emitter CANNOT accidentally copy an attribute through: the only bytes that reach
			// the output are the ones it returns.
			var attrs []htmlAttr
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = tokenizer.TagAttr()
				attrs = append(attrs, htmlAttr{key: string(k), val: string(v)})
			}
			b.WriteString("<" + tag)
			if rules.attrs != nil {
				b.WriteString(rules.attrs(tag, attrs))
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
			if skipDepth > 0 || !rules.allowed[tag] {
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
// no synthesised closer. Only those a caller's allow-list names can actually appear, but the full
// set is listed so the rule reads as "void elements" rather than "the ones we happen to allow".
var voidTags = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true, "hr": true,
	"img": true, "input": true, "link": true, "meta": true, "source": true, "track": true, "wbr": true,
}

// hrefAttr returns the `href` attributes of `attrs` that survive httpURL, formatted for output.
//
// Shared by both rewriteHTML callers because both need the same answer: a `javascript:` or `data:`
// href must not reach an anchor in a preview document or in a sent email, and the check that
// decides is the SAME httpURL every other link in this package goes through.
//
// Returns "" when no href validates, which is the signal each caller interprets for itself --
// the wizard emits a bare `<a>`, the email body renders the anchor as plain text.
func hrefAttr(attrs []htmlAttr) string {
	var out strings.Builder
	for _, attr := range attrs {
		if attr.key != "href" {
			continue
		}
		if href := httpURL(attr.val); href != "" {
			out.WriteString(` href="` + html.EscapeString(href) + `"`)
		}
	}
	return out.String()
}
