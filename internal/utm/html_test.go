// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package utm

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var hrefRe = regexp.MustCompile(`href="([^"]*)"`)

func hrefs(t *testing.T, htmlStr string) []string {
	t.Helper()
	var out []string
	for _, m := range hrefRe.FindAllStringSubmatch(htmlStr, -1) {
		out = append(out, strings.ReplaceAll(m[1], "&amp;", "&"))
	}
	return out
}

// TestTagHTMLLinks_TagsEveryWebLink covers the main path: each anchor gets the campaign and a
// distinct utm_content so a report can tell which link was clicked.
func TestTagHTMLLinks_TagsEveryWebLink(t *testing.T) {
	in := `<p>Hi</p><a href="https://events.lfx.dev/a">A</a><div><a href="https://events.lfx.dev/b">B</a></div>`

	out, err := TagHTMLLinks(in, testParams(), "")
	require.NoError(t, err)

	got := hrefs(t, out)
	require.Len(t, got, 2)
	for _, h := range got {
		assert.Contains(t, h, "utm_campaign=kubecon-korea-2026")
		assert.Contains(t, h, "utm_medium=LF-Events")
	}
	// Numbering is in document order and distinguishes the links.
	assert.Contains(t, got[0], "utm_content=body-link-1")
	assert.Contains(t, got[1], "utm_content=body-link-2")

	// Surrounding markup and link text must be untouched.
	assert.Contains(t, out, "<p>Hi</p>")
	assert.Contains(t, out, ">A</a>")
}

// TestTagHTMLLinks_NumberingSkipsUntaggedLinks pins that utm_content numbers describe the links
// actually TAGGED. If skipped links consumed numbers, the labels would have gaps and no longer
// correspond to anything a reader could count in the email.
func TestTagHTMLLinks_NumberingSkipsUntaggedLinks(t *testing.T) {
	in := `<a href="mailto:x@y.z">mail</a>` +
		`<a href="https://events.lfx.dev/a">A</a>` +
		`<a href="#agenda">anchor</a>` +
		`<a href="https://events.lfx.dev/b">B</a>`

	out, err := TagHTMLLinks(in, testParams(), "")
	require.NoError(t, err)

	got := hrefs(t, out)
	require.Len(t, got, 4)
	assert.Equal(t, "mailto:x@y.z", got[0], "mailto must be untouched")
	assert.Equal(t, "#agenda", got[2], "in-page anchors must be untouched")
	assert.Contains(t, got[1], "utm_content=body-link-1")
	assert.Contains(t, got[3], "utm_content=body-link-2", "the skipped links must not consume numbers")
}

// TestTagHTMLLinks_LeavesPreTaggedLinksAlone pins that a hand-tagged link survives untouched
// AND does not consume a number.
func TestTagHTMLLinks_LeavesPreTaggedLinksAlone(t *testing.T) {
	pre := "https://events.lfx.dev/pre?utm_campaign=hand-picked"
	in := `<a href="` + pre + `">pre</a><a href="https://events.lfx.dev/a">A</a>`

	out, err := TagHTMLLinks(in, testParams(), "")
	require.NoError(t, err)

	got := hrefs(t, out)
	require.Len(t, got, 2)
	assert.Equal(t, pre, got[0], "a deliberate campaign must never be overwritten")
	assert.Contains(t, got[1], "utm_content=body-link-1")
}

// TestTagHTMLLinks_NoOpWithoutWorkToDo pins the cases where the fragment must come back byte
// for byte, so a caller can safely call this unconditionally.
func TestTagHTMLLinks_NoOpWithoutWorkToDo(t *testing.T) {
	in := `<a href="https://events.lfx.dev/a">A</a>`

	out, err := TagHTMLLinks(in, Params{}, "") // no campaign
	require.NoError(t, err)
	assert.Equal(t, in, out)

	out, err = TagHTMLLinks("", testParams(), "")
	require.NoError(t, err)
	assert.Empty(t, out)
}

// TestTagHTMLLinks_PreservesFragmentStructure pins that the body is NOT wrapped in a
// synthesized <html>/<head>/<body>. Email bodies are fragments spliced into a template; adding
// document scaffolding would corrupt the rendered email.
func TestTagHTMLLinks_PreservesFragmentStructure(t *testing.T) {
	in := `<p>Register <a href="https://events.lfx.dev/r">here</a>.</p>`

	out, err := TagHTMLLinks(in, testParams(), "")
	require.NoError(t, err)

	assert.NotContains(t, strings.ToLower(out), "<html")
	assert.NotContains(t, strings.ToLower(out), "<body")
	assert.NotContains(t, strings.ToLower(out), "<head")
	assert.True(t, strings.HasPrefix(out, "<p>"), "the fragment must start where it did: %s", out)
}

// TestTagHTMLLinks_CustomPrefix pins the caller-supplied label prefix.
func TestTagHTMLLinks_CustomPrefix(t *testing.T) {
	out, err := TagHTMLLinks(`<a href="https://events.lfx.dev/a">A</a>`, testParams(), "hero")
	require.NoError(t, err)
	assert.Contains(t, hrefs(t, out)[0], "utm_content=hero-1")
}

// TestTagHTMLLinks_HandlesAnchorsWithoutHref guards against a nil-deref on markup that is
// legal but unusual — an <a> used purely as a named target carries no href.
func TestTagHTMLLinks_HandlesAnchorsWithoutHref(t *testing.T) {
	in := `<a name="top"></a><a href="https://events.lfx.dev/a">A</a>`

	out, err := TagHTMLLinks(in, testParams(), "")
	require.NoError(t, err)
	assert.Contains(t, out, `name="top"`)
	assert.Contains(t, hrefs(t, out)[0], "utm_content=body-link-1")
}

// TestTagHTMLLinksFrom_ContinuesNumberingAcrossFragments pins the multi-widget contract. An
// email body is split across rich-text widgets; tagging each independently restarts at
// "body-link-1", so a multi-widget email emits DUPLICATE utm_content values and a report cannot
// tell which link was clicked — which defeats the point of labelling them.
func TestTagHTMLLinksFrom_ContinuesNumberingAcrossFragments(t *testing.T) {
	w1 := `<a href="https://events.lfx.dev/a">A</a><a href="https://events.lfx.dev/b">B</a>`
	w2 := `<a href="https://events.lfx.dev/c">C</a>`

	out1, n1, err := TagHTMLLinksFrom(w1, testParams(), "", 0)
	require.NoError(t, err)
	assert.Equal(t, 2, n1, "the count returned must be the number of links TAGGED")

	out2, n2, err := TagHTMLLinksFrom(w2, testParams(), "", n1)
	require.NoError(t, err)
	assert.Equal(t, 1, n2)

	got := append(hrefs(t, out1), hrefs(t, out2)...)
	require.Len(t, got, 3)
	assert.Contains(t, got[0], "utm_content=body-link-1")
	assert.Contains(t, got[1], "utm_content=body-link-2")
	assert.Contains(t, got[2], "utm_content=body-link-3",
		"numbering must CONTINUE across widgets, not restart")

	// Every label must be distinct — that is the property reports depend on.
	seen := map[string]bool{}
	for _, h := range got {
		label := h[strings.Index(h, "utm_content=")+len("utm_content="):]
		if i := strings.IndexByte(label, '&'); i >= 0 {
			label = label[:i]
		}
		assert.False(t, seen[label], "duplicate utm_content %q: reports cannot distinguish these links", label)
		seen[label] = true
	}
}

// TestTagHTMLLinksFrom_SkippedLinksDoNotConsumeNumbers guards the interaction between the
// carried count and the skip rules.
func TestTagHTMLLinksFrom_SkippedLinksDoNotConsumeNumbers(t *testing.T) {
	in := `<a href="mailto:x@y.z">m</a><a href="https://events.lfx.dev/a">A</a>`
	out, n, err := TagHTMLLinksFrom(in, testParams(), "", 5)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only the tagged link counts")
	assert.Contains(t, hrefs(t, out)[1], "utm_content=body-link-6", "numbering resumes from the carried count")
}

// TestTagHTMLLinks_PreservesOutlookConditionalComments pins that re-serializing a widget does
// not corrupt Outlook's conditional comments. Email markup is full of `<!--[if mso]>…<![endif]-->`
// blocks, and escaping the `>` inside them would make Outlook stop recognising the block — a
// layout break visible only in Outlook, so it would ship unnoticed.
//
// x/net/html does not escape comment contents, but this pins the behaviour so a library bump
// that changed it would fail here rather than in someone's inbox.
func TestTagHTMLLinks_PreservesOutlookConditionalComments(t *testing.T) {
	cases := map[string]string{
		"wrapping table":     `<!--[if mso]><table><tr><td><![endif]--><a href="https://events.lfx.dev/r">Go</a><!--[if mso]></td></tr></table><![endif]-->`,
		"office settings":    `<!--[if gte mso 9]><xml><o:OfficeDocumentSettings/></xml><![endif]--><a href="https://events.lfx.dev/r">Go</a>`,
		"downlevel-revealed": `<!--[if !mso]><!--><a href="https://events.lfx.dev/r">Go</a><!--<![endif]-->`,
		"inside a div":       `<div><!--[if mso]><td width="100%"><![endif]--><a href="https://events.lfx.dev/r">Go</a></div>`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := TagHTMLLinks(in, testParams(), "")
			require.NoError(t, err)

			assert.NotContains(t, out, "&gt;", "a > inside a conditional comment must not be escaped")
			assert.NotContains(t, out, "&lt;")
			// The link was still tagged — the point is that tagging is safe here, not skipped.
			assert.Contains(t, out, "utm_campaign=kubecon-korea-2026")
		})
	}
}

// TestTagHTMLLinks_PreservesTableStructure is the regression test for a fragment that a
// tree round-trip destroyed.
//
// Parsing needs a context element, and the HTML spec's insertion modes DISCARD content invalid
// for it: "<tr><td><a …>" parsed in a body context loses its row and cell entirely, so
// re-rendering returned markup with the table structure stripped. Email HTML is written as
// table layouts, so this is the common case — a tagged campaign email would have shipped with
// its layout collapsed. Choosing a table context instead would only move the failure to
// non-table widgets, which is why the implementation rewrites tokens rather than a tree.
func TestTagHTMLLinks_PreservesTableStructure(t *testing.T) {
	cases := []struct{ name, in string }{
		{"row and cell", `<tr><td><a href="https://lf.dev/e">go</a></td></tr>`},
		{"bare cell", `<td><a href="https://lf.dev/e">go</a></td>`},
		{"nested table", `<table><tbody><tr><td><a href="https://lf.dev/e">go</a></td></tr></tbody></table>`},
		{"list item", `<li><a href="https://lf.dev/e">go</a></li>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, n, err := TagHTMLLinksFrom(tc.in, Params{Source: "s", Medium: "m", Campaign: "c"}, "", 0)
			require.NoError(t, err)
			assert.Equal(t, 1, n, "the anchor must still be tagged")
			// Every structural tag from the input must survive.
			for _, tag := range []string{"<tr", "<td", "<table", "<tbody", "<li", "</tr", "</td", "</li"} {
				if strings.Contains(tc.in, tag) {
					assert.Contains(t, got, tag, "%s must not be dropped from the fragment", tag)
				}
			}
			assert.Contains(t, got, "utm_campaign=c")
		})
	}
}

// TestTagHTMLLinks_LeavesUntaggedBytesVerbatim pins the never-mangle contract in its strongest
// form: anything the tagger does not deliberately change comes back byte-identical, including
// markup a parser would "repair".
func TestTagHTMLLinks_LeavesUntaggedBytesVerbatim(t *testing.T) {
	// A conditional comment, an unclosed tag, and a single-quoted attribute — all things a
	// tree round-trip normalizes. None contains a taggable anchor, so all must be untouched.
	for _, in := range []string{
		`<!--[if mso]><a href="https://lf.dev/e">o</a><![endif]-->`,
		`<p>dangling <b>bold`,
		`<a href='mailto:a@b.c'>mail</a>`,
		`<div class='x'   data-k=1 >text</div>`,
	} {
		got, n, err := TagHTMLLinksFrom(in, Params{Source: "s", Medium: "m", Campaign: "c"}, "", 0)
		require.NoError(t, err)
		assert.Zero(t, n)
		assert.Equal(t, in, got, "an untagged fragment must come back byte-identical")
	}
}
