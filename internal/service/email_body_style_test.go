// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"regexp"
	"strings"
	"testing"
)

// styled is the expected output for one tag carrying its table style and nothing else.
//
// Built from emailBodyTagStyles rather than from a hand-copied style string on purpose: the VALUE
// of each style is a design decision these tests must not freeze (a reviewer changing a font size
// should not have to update ten assertions), but the SHAPE is a contract -- one `style` attribute,
// on the right tag, with the text intact and no other attribute. That shape is what breaks when
// the rewriter is wrong, and it is what this helper pins. The values are held instead by
// TestEmailBodyStyleTableIsInert and TestEmailBodyPaletteMatchesTheCTAButton.
func styled(tag, text string) string {
	return "<" + tag + styleAttr(emailBodyTagStyles[tag]) + ">" + text + "</" + tag + ">"
}

func TestStyleEmailBodyHTMLStylesSemanticTags(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "heading", in: "<h2>Why attend</h2>", want: styled("h2", "Why attend")},
		{name: "subheading", in: "<h3>Who comes</h3>", want: styled("h3", "Who comes")},
		{name: "paragraph", in: "<p>Three days in Amsterdam.</p>", want: styled("p", "Three days in Amsterdam.")},
		{name: "bold lead-in", in: "<strong>Keynotes:</strong>", want: styled("strong", "Keynotes:")},
		{name: "light emphasis", in: "<em>new this year</em>", want: styled("em", "new this year")},
		{
			name: "list",
			in:   "<ul><li>Keynotes</li><li>Workshops</li></ul>",
			want: "<ul" + styleAttr(emailBodyTagStyles["ul"]) + ">" +
				styled("li", "Keynotes") + styled("li", "Workshops") + "</ul>",
		},
		{name: "pulled quote", in: "<blockquote>Worth the trip.</blockquote>", want: styled("blockquote", "Worth the trip.")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := styleEmailBodyHTML(tc.in); got != tc.want {
				t.Fatalf("styleEmailBodyHTML(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A divider is a VOID element, so it must not acquire a closer -- and the self-closing spelling a
// model is just as likely to write must produce the same bytes, not `<hr></hr>`.
func TestStyleEmailBodyHTMLStylesTheDividerWithoutClosingIt(t *testing.T) {
	want := "<hr" + styleAttr(emailBodyTagStyles["hr"]) + ">"

	for _, in := range []string{"<hr>", "<hr/>", "<hr />"} {
		if got := styleEmailBodyHTML(in); got != want {
			t.Fatalf("styleEmailBodyHTML(%q) = %q, want %q", in, got, want)
		}
	}
}

// The whole point of the styler: whatever the model wrote about appearance is discarded, and this
// service's own design is applied in its place. A model that styles its output anyway must not be
// able to make one variant of an A/B test look different from the other.
func TestStyleEmailBodyHTMLReplacesModelAuthoredStyling(t *testing.T) {
	in := `<p style="color:red;font-size:40px" class="hero" id="x" align="center">Register today.</p>`

	got := styleEmailBodyHTML(in)

	if want := styled("p", "Register today."); got != want {
		t.Fatalf("styleEmailBodyHTML(%q) = %q, want %q", in, got, want)
	}
	for _, leaked := range []string{"red", "40px", "hero", "align", `id="x"`} {
		if strings.Contains(got, leaked) {
			t.Errorf("model-authored %q survived styling: %q", leaked, got)
		}
	}
}

func TestStyleEmailBodyHTMLDropsScriptsHandlersAndStyleBlocks(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		absent  string
		present string
	}{
		{
			name:    "event handler",
			in:      `<p onclick="steal()" onmouseover="steal()">Read on</p>`,
			absent:  "steal",
			present: "Read on",
		},
		{
			name:    "script between paragraphs",
			in:      `<p>before</p><script>alert(1)</script><p>after</p>`,
			absent:  "alert(1)",
			present: "after",
		},
		{
			name:    "style block",
			in:      `<style>p{color:fuchsia}</style><p>copy</p>`,
			absent:  "fuchsia",
			present: "copy",
		},
		{
			name:    "self-closing script, which force-consumes the rest of the block",
			in:      `<p>before</p><script/>alert(1)<p>after</p>`,
			absent:  "alert(1)",
			present: "before",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := styleEmailBodyHTML(tc.in)
			if strings.Contains(got, tc.absent) {
				t.Errorf("styleEmailBodyHTML(%q) leaked %q: %q", tc.in, tc.absent, got)
			}
			if !strings.Contains(got, tc.present) {
				t.Errorf("styleEmailBodyHTML(%q) lost %q: %q", tc.in, tc.present, got)
			}
		})
	}
}

// An <img> is not in the allow-list, so a model that reaches for a remote image -- a logo, a
// speaker photo, or a tracking pixel pointing anywhere -- gets nothing. The schema has no image
// section for the same reason (see the variant prompts).
func TestStyleEmailBodyHTMLDropsImages(t *testing.T) {
	in := `<p>Our sponsors<img src="https://tracker.example/pixel.gif" width="1"></p>`

	got := styleEmailBodyHTML(in)

	if want := styled("p", "Our sponsors"); got != want {
		t.Fatalf("styleEmailBodyHTML(%q) = %q, want %q", in, got, want)
	}
}

// A tag that is merely not allowed loses its TAG, never its words: a model wrapping a sentence in
// a layout table has written copy inside markup this service will not send, and deleting the
// sentence with the markup would silently shorten the email.
func TestStyleEmailBodyHTMLKeepsTheTextOfDroppedTags(t *testing.T) {
	in := "<table><tr><td>Keynote at 09:00</td></tr></table>"

	if got, want := styleEmailBodyHTML(in), "Keynote at 09:00"; got != want {
		t.Fatalf("styleEmailBodyHTML(%q) = %q, want %q", in, got, want)
	}
}

func TestStyleEmailBodyHTMLStylesAValidLink(t *testing.T) {
	in := `<p>See the <a href="https://events.example/kubecon/agenda">full agenda</a>.</p>`

	got := styleEmailBodyHTML(in)

	want := "<p" + styleAttr(emailBodyTagStyles["p"]) + ">See the " +
		`<a href="https://events.example/kubecon/agenda" target="_blank" rel="noopener noreferrer"` +
		styleAttr(emailBodyTagStyles["a"]) + ">full agenda</a>.</p>"
	if got != want {
		t.Fatalf("styleEmailBodyHTML(%q) = %q, want %q", in, got, want)
	}
}

// A non-http(s) href is refused by the same httpURL every other link in this package goes
// through, and the anchor is then left UNSTYLED on purpose: an anchor with no href is inert, so
// styling it to look like a link would dress inert text as something to click.
func TestStyleEmailBodyHTMLRefusesANonHTTPHref(t *testing.T) {
	for _, href := range []string{
		"javascript:alert(1)",
		"data:text/html;base64,PHNjcmlwdD4=",
		"mailto:someone@example.com",
		"/relative/path",
		"https://user:password@events.example/register",
	} {
		in := `<a href="` + href + `">click here</a>`
		if got, want := styleEmailBodyHTML(in), "<a>click here</a>"; got != want {
			t.Errorf("styleEmailBodyHTML(%q) = %q, want %q", in, got, want)
		}
	}
}

// Idempotent BY CONSTRUCTION, because every attribute is dropped before ours is added. This is
// what makes the styler safe to apply at a point that might one day run twice (a refine pass over
// an already-generated section, say) without compounding its own output.
func TestStyleEmailBodyHTMLIsIdempotent(t *testing.T) {
	in := `<h2>Why attend</h2><p>Three days in <strong>Amsterdam</strong>.</p>` +
		`<ul><li>Keynotes</li><li>Workshops</li></ul><hr>` +
		`<p>See the <a href="https://events.example/agenda">agenda</a>.</p>`

	once := styleEmailBodyHTML(in)
	twice := styleEmailBodyHTML(once)

	if once != twice {
		t.Fatalf("styling is not idempotent:\n once: %q\ntwice: %q", once, twice)
	}
	if once == in {
		t.Fatal("styleEmailBodyHTML returned its input unchanged, so this proves nothing")
	}
}

// styleAttr writes its value into an attribute WITHOUT escaping it, which is only sound because
// every value is a constant in this file. This test is what holds that line: a style value that
// grew a quote or an angle bracket -- from a copy-paste, or from someone making the table
// configurable -- would turn the unescaped write into an injection point, and it would do so
// silently.
func TestEmailBodyStyleTableIsInert(t *testing.T) {
	for tag, style := range emailBodyTagStyles {
		for _, forbidden := range []string{`"`, `'`, "<", ">", "&", "\\"} {
			if strings.Contains(style, forbidden) {
				t.Errorf("emailBodyTagStyles[%q] contains %q, which styleAttr does not escape: %q", tag, forbidden, style)
			}
		}
		// `url(...)` in an inline style is a fetch -- and in several mail clients a
		// `url(javascript:...)` is worse than a fetch. No entry needs one.
		if strings.Contains(strings.ToLower(style), "url(") {
			t.Errorf("emailBodyTagStyles[%q] contains a url(): %q", tag, style)
		}
	}
}

// The link colour is NOT a free choice: addButtonSection in internal/platform/hubspot/content.go
// renders the CTA button in #2563eb (picked there for WCAG AA contrast on white), and a text link
// in a different blue would read as a different kind of affordance in the same email. The two
// cannot be shared as one constant without internal/platform/hubspot importing internal/service,
// which is the wrong direction and would cycle -- so they are pinned here instead, with this test
// as the reminder that changing one means changing the other.
func TestEmailBodyPaletteMatchesTheCTAButton(t *testing.T) {
	if emailLinkColor != "#2563eb" {
		t.Errorf("emailLinkColor = %q; addButtonSection in internal/platform/hubspot/content.go uses #2563eb -- change both or neither", emailLinkColor)
	}
	if !strings.Contains(emailBodyTagStyles["a"], emailLinkColor) {
		t.Errorf("the <a> style does not use emailLinkColor: %q", emailBodyTagStyles["a"])
	}
}

// bodyStyleRule tells the model which tags to write. A tag named there but absent from
// emailBodyTagStyles would be stripped from every email with no error anywhere -- the prompt
// asking for markup the service silently deletes. The converse matters too: a tag the rule calls
// forbidden must really be unstyled, or the prompt is lying about what this service accepts.
func TestBodyStyleRuleNamesOnlyStyledTags(t *testing.T) {
	const split = "Never write"
	cut := strings.Index(bodyStyleRule, split)
	if cut < 0 {
		t.Fatalf("bodyStyleRule no longer contains %q, so this test cannot tell its allowed half from its forbidden half; re-split it", split)
	}

	tagRE := regexp.MustCompile(`<([a-z][a-z0-9]*)`)

	for _, match := range tagRE.FindAllStringSubmatch(bodyStyleRule[:cut], -1) {
		if !isStyledEmailBodyTag(match[1]) {
			t.Errorf("bodyStyleRule asks the model for <%s>, which emailBodyTagStyles does not style, so it is stripped from every email", match[1])
		}
	}
	for _, match := range tagRE.FindAllStringSubmatch(bodyStyleRule[cut:], -1) {
		if isStyledEmailBodyTag(match[1]) {
			t.Errorf("bodyStyleRule tells the model <%s> is stripped, but emailBodyTagStyles styles it", match[1])
		}
	}
}

// isStyledEmailBodyTag reports whether a tag is in the style table, which is the same question
// as whether it survives styleEmailBodyHTML -- emailBodyAllowedTags is derived from that table.
func isStyledEmailBodyTag(tag string) bool {
	_, ok := emailBodyTagStyles[tag]
	return ok
}

// The allow-list is derived from the style table, not written twice. If that derivation is ever
// replaced by a second literal map, this fails.
func TestEmailBodyAllowedTagsTracksTheStyleTable(t *testing.T) {
	if len(emailBodyAllowedTags) != len(emailBodyTagStyles) {
		t.Fatalf("emailBodyAllowedTags has %d entries, emailBodyTagStyles has %d", len(emailBodyAllowedTags), len(emailBodyTagStyles))
	}
	for tag := range emailBodyTagStyles {
		if !emailBodyAllowedTags[tag] {
			t.Errorf("%q is styled but not allowed", tag)
		}
	}
}

// Styling inflates its input, so a section that was within the per-section limit can style past
// it. The overflow must degrade to the unstyled HTML rather than fail: the alternative is that a
// perfectly good model response becomes a 503 because the SERVICE's own stylesheet did not fit.
func TestStyledBodyHTMLWithinBoundFallsBackWhenStylingOverflows(t *testing.T) {
	const in = "<p>hi</p>"

	// A bound the unstyled HTML clears and the styled HTML cannot.
	got := styledBodyHTMLWithinBound(in, len(in))

	if got != in {
		t.Fatalf("styledBodyHTMLWithinBound(%q, %d) = %q, want the unstyled input", in, len(in), got)
	}
}

func TestStyledBodyHTMLWithinBoundStylesWhatFits(t *testing.T) {
	const in = "<p>hi</p>"

	got := styledBodyHTMLWithinBound(in, 8000)

	if want := styled("p", "hi"); got != want {
		t.Fatalf("styledBodyHTMLWithinBound(%q, 8000) = %q, want %q", in, got, want)
	}
}

// The bound is in RUNES, matching maxHTMLRunes and the Goa MaxLength it mirrors. A byte-counted
// bound would reject a section of accented or CJK copy that is well within the real limit.
func TestStyledBodyHTMLWithinBoundCountsRunesNotBytes(t *testing.T) {
	in := "<p>" + strings.Repeat("é", 40) + "</p>"
	styledLen := len([]rune(styleEmailBodyHTML(in)))

	if got := styledBodyHTMLWithinBound(in, styledLen); got == in {
		t.Fatalf("styledBodyHTMLWithinBound fell back at a bound of exactly %d runes, so it is counting bytes", styledLen)
	}
	if got := styledBodyHTMLWithinBound(in, styledLen-1); got != in {
		t.Fatalf("styledBodyHTMLWithinBound(_, %d) did not fall back one rune under the styled length: %q", styledLen-1, got)
	}
}
