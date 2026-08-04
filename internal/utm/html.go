// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package utm

import (
	"fmt"
	"io"
	"strings"

	"golang.org/x/net/html"
)

// DefaultLinkPrefix labels body links in utm_content ("body-link-1", "body-link-2", …), so a
// report can tell which link in an email was clicked.
const DefaultLinkPrefix = "body-link"

// TagHTMLLinks rewrites every <a href> in an HTML fragment to carry the UTM parameters,
// numbering utm_content as "<prefix>-N" in document order.
//
// It returns the input UNCHANGED when there is nothing to do (blank HTML, no campaign) or when
// the fragment cannot be parsed — email body HTML comes from a marketing tool and may be
// fragmentary or malformed, and returning a mangled body would break the email itself. An
// untagged email is a reporting gap; a broken one is a failed send.
//
// Links that Apply declines to tag (mailto:/tel:/anchors, already-tagged links) are left alone
// AND do not consume a number, so the numbering describes the links that were actually tagged
// rather than counting skipped ones.
func TagHTMLLinks(fragment string, p Params, prefix string) (string, error) {
	out, _, err := TagHTMLLinksFrom(fragment, p, prefix, 0)
	return out, err
}

// TagHTMLLinksFrom is TagHTMLLinks with an explicit starting count, returning the number of
// links it tagged so a caller can continue numbering across SEVERAL fragments.
//
// An email body is split across rich-text widgets. Tagging each one independently restarts at
// "<prefix>-1", so a multi-widget email emits duplicate utm_content values and a report cannot
// tell which link was clicked — defeating the point of labelling them at all.
func TagHTMLLinksFrom(fragment string, p Params, prefix string, startAt int) (string, int, error) {
	if strings.TrimSpace(fragment) == "" || strings.TrimSpace(p.Campaign) == "" {
		return fragment, 0, nil
	}
	if strings.TrimSpace(prefix) == "" {
		prefix = DefaultLinkPrefix
	}

	// TOKENIZE and rewrite hrefs in place; do NOT parse into a tree and re-render.
	//
	// A tree round-trip cannot be made safe for arbitrary email HTML. Parsing needs a context
	// element, and the HTML spec's insertion modes DISCARD content that is invalid for it: a
	// widget beginning "<tr><td><a …>" parsed in a body context loses its row and cell entirely,
	// so re-rendering silently returns a fragment with the table structure stripped. Choosing a
	// table context instead just moves the failure to non-table widgets. Email HTML is written
	// as table layouts, so this is the common case, not an edge one.
	//
	// Rewriting tokens sidesteps the whole problem: every byte OUTSIDE a tagged anchor survives
	// verbatim — malformed markup, conditional comments, unusual nesting, table structure and all.
	// That is a stronger form of the never-mangle contract this package already promised.
	//
	// The guarantee is per-token, and it stops at the tagged anchors themselves. A tagged <a> is
	// re-serialized from its parsed attributes (see rewriteAnchor), so its START TAG — and only
	// its start tag — is normalized: attribute names lower-case, values double-quoted and
	// HTML-escaped, and a VALUELESS attribute gains an empty value (`download` -> `download=""`).
	// All three forms are equivalent per the HTML spec and every email client parses them the
	// same way, so this is cosmetic rather than behavioural — but it is not byte-identity, and
	// claiming otherwise would invite a future change to rely on identity that does not hold.
	// Anchors the tagger declines to touch keep their ORIGINAL bytes exactly.
	var b strings.Builder
	b.Grow(len(fragment) + 128)
	z := html.NewTokenizer(strings.NewReader(fragment))
	n := startAt
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if err := z.Err(); err != nil && err != io.EOF {
				// Never emit a partial document: return the input untouched.
				return fragment, 0, fmt.Errorf("utm: tokenize email html: %w", err)
			}
			break
		}
		// COPY the raw token. z.Raw() returns a slice into the tokenizer's own buffer, and
		// x/net/html documents it as valid only until the next Next/Token/Text/TagName/TagAttr
		// call — TagAttr in particular unescapes attribute values IN PLACE. Since an anchor is
		// only written back after rewriteAnchor has called TagAttr (to discover whether its href
		// changed at all), holding the original slice would risk emitting a mutated buffer for
		// an anchor the tagger deliberately left alone. Copying costs one allocation per token
		// and makes the never-mangle guarantee hold by construction rather than by timing.
		raw := append([]byte(nil), z.Raw()...)
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			b.Write(raw)
			continue
		}
		name, hasAttr := z.TagName()
		if !hasAttr || string(name) != "a" {
			b.Write(raw)
			continue
		}
		// Re-emit the tag from its parsed attributes ONLY when an href actually changed, so an
		// untouched anchor keeps its original bytes (quoting, attribute order, spacing).
		rewritten, changed := rewriteAnchor(z, string(name), tt == html.SelfClosingTagToken, p, prefix, &n)
		if changed {
			b.WriteString(rewritten)
			continue
		}
		b.Write(raw)
	}

	// Nothing was tagged: return the ORIGINAL fragment, byte-identical.
	if n == startAt {
		return fragment, 0, nil
	}
	return b.String(), n - startAt, nil
}

// rewriteAnchor reads the current anchor's attributes and returns its re-serialized start tag,
// reporting whether the href was actually changed. The caller keeps the ORIGINAL bytes when it
// was not, so this never reformats an anchor it did not tag.
func rewriteAnchor(z *html.Tokenizer, name string, selfClosing bool, p Params, prefix string, n *int) (string, bool) {
	type attr struct{ key, val string }
	var attrs []attr
	changed := false
	for {
		k, v, more := z.TagAttr()
		key, val := string(k), string(v)
		if strings.EqualFold(key, "href") && !changed {
			// Compute the label from the count this link WOULD take, and only consume the
			// number if the link actually changed.
			candidate := fmt.Sprintf("%s-%d", prefix, *n+1)
			if tagged := Apply(val, p, candidate); tagged != val {
				val = tagged
				*n++
				changed = true
			}
		}
		attrs = append(attrs, attr{key, val})
		if !more {
			break
		}
	}
	if !changed {
		return "", false
	}
	var b strings.Builder
	b.WriteByte('<')
	b.WriteString(name)
	for _, a := range attrs {
		b.WriteByte(' ')
		b.WriteString(a.key)
		b.WriteString(`="`)
		b.WriteString(html.EscapeString(a.val))
		b.WriteByte('"')
	}
	if selfClosing {
		b.WriteString("/>")
	} else {
		b.WriteByte('>')
	}
	return b.String(), true
}
