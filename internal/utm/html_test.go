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
