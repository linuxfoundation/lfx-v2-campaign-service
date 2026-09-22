// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"strings"
	"testing"
)

// The rich_text block is filled by a MODEL as well as by the editor, and reaches two sinks that
// render it: the preview document served to the operator's browser, and the HubSpot draft body
// sent to recipients. Length was the only bound before this.
func TestSanitizeWizardHTMLStripsExecutableContent(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"script content is dropped, not just the tag", `<p>hi</p><script>alert(1)</script>`, `<p>hi</p>`},
		{"style content is dropped", `<p>hi</p><style>body{}</style>`, `<p>hi</p>`},
		{"an event handler cannot survive on an allowed tag", `<div onclick="alert(1)">words</div>`, `<div>words</div>`},
		{"style attributes go, since url(javascript:) rides in them", `<p style="expression(alert(1))">styled</p>`, `<p>styled</p>`},
		{"a disallowed tag drops but keeps its words", `<img src=x onerror="alert(1)">after`, `after`},
		{"a javascript href is dropped, the label is kept", `<a href="javascript:alert(1)">click</a>`, `<a>click</a>`},
		{"a data href is dropped too", `<a href="data:text/html,<script>alert(1)</script>">x</a>`, `<a>x</a>`},
		{"an http href survives", `<a href="https://ok.example/x">click</a>`, `<a href="https://ok.example/x">click</a>`},
		{"formatting survives", `<p><strong>bold</strong> and <em>italic</em></p>`, `<p><strong>bold</strong> and <em>italic</em></p>`},
		{"lists survive", `<ul><li>one</li><li>two</li></ul>`, `<ul><li>one</li><li>two</li></ul>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeWizardHTML(tc.in); got != tc.want {
				t.Errorf("sanitizeWizardHTML(%q)\n got: %q\nwant: %q", tc.in, got, tc.want)
			}
		})
	}
}

// Regex-based stripping is defeated by split tags; this input is adversarial by assumption, so
// it is parsed with a real tokenizer.
func TestSanitizeWizardHTMLDefeatsSplitTagTricks(t *testing.T) {
	for _, in := range []string{
		`<scr<script>ipt>alert(1)</script>`,
		`<SCRIPT>alert(1)</SCRIPT>`,
		`<script/xss>alert(1)</script>`,
		`<a href="jAvAsCrIpT:alert(1)">x</a>`,
	} {
		got := sanitizeWizardHTML(in)
		if containsAny(got, "<script", "javascript:", "alert(1)</") {
			t.Errorf("sanitizeWizardHTML(%q) left executable content: %q", in, got)
		}
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

// TestParsedSectionsAreSanitizedBeforeTheyReachTheCaller pins the THIRD sink.
//
// The first version of this sanitizer ran in `renderWizardSections` and in the preview
// assembly, which covered the HubSpot draft body and the assembled `html`. It missed
// `RawSections`, which is returned to the caller verbatim as `sections` /
// `variant_a_sections` -- so raw model HTML reached the client untouched while both other
// paths were clean.
//
// Sanitizing per render path is what allowed that: every new consumer of the sections is a
// fresh chance to forget. The sanitize now happens where the sections are PARSED, so a
// consumer cannot receive unsanitized HTML without going around the parser entirely.
func TestParsedSectionsAreSanitizedBeforeTheyReachTheCaller(t *testing.T) {
	body := `{"subject":"s","preview_text":"p","sections":[` +
		`{"type":"rich_text","html":"<p onclick=\"steal()\">hi</p><script>alert(1)</script>"}]}`

	parsed, err := parseWizardContentResponse(body)
	if err != nil {
		t.Fatalf("parseWizardContentResponse: %v", err)
	}
	if len(parsed.RawSections) != 1 {
		t.Fatalf("expected 1 raw section, got %d", len(parsed.RawSections))
	}

	obj, ok := parsed.RawSections[0].(map[string]any)
	if !ok {
		t.Fatalf("raw section is not an object: %T", parsed.RawSections[0])
	}
	html, _ := obj["html"].(string)

	// The value the CALLER receives, not the rendered body.
	if strings.Contains(html, "<script") {
		t.Errorf("a script tag survived into the returned sections: %q", html)
	}
	if strings.Contains(html, "onclick") {
		t.Errorf("an event handler survived into the returned sections: %q", html)
	}
	if !strings.Contains(html, "hi") {
		t.Errorf("sanitizing removed the legitimate text too: %q", html)
	}
}

// A self-closing NON-VOID tag must close itself: no EndTagToken ever arrives for `<div/>`, so
// the sanitizer emitted a bare `<div>` and everything after it nested inside. Void elements are
// correct unclosed, which is why they are asserted in the same test -- a fix that closed
// everything would produce `<br></br>`, which is invalid.
func TestSanitizeWizardHTMLClosesSelfClosingNonVoidTags(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`<div/>x`, `<div></div>x`},
		{`<p/>y`, `<p></p>y`},
		{`<span/>w`, `<span></span>w`},
		{`<br/>z`, `<br>z`},
	} {
		if got := sanitizeWizardHTML(tc.in); got != tc.want {
			t.Errorf("sanitizeWizardHTML(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The whole allow-list, and the drop/keep interaction when they nest.
//
// The earlier tests covered the tags an attacker reaches for and the ones a paragraph needs,
// which left most of `allowedTags` unexercised: an entry could be dropped from the map and
// nothing would fail. The nesting cases matter more than the flat ones -- `dropContent` removes
// a tag's CONTENT, and getting that wrong inside an allowed parent either leaks the script text
// as visible copy or swallows the surrounding paragraph.
func TestSanitizeWizardHTMLCoversTheAllowListAndNesting(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"bold", `<b>x</b>`, `<b>x</b>`},
		{"italic", `<i>x</i>`, `<i>x</i>`},
		{"underline", `<u>x</u>`, `<u>x</u>`},
		{"strong", `<strong>x</strong>`, `<strong>x</strong>`},
		{"emphasis", `<em>x</em>`, `<em>x</em>`},
		{"blockquote", `<blockquote>x</blockquote>`, `<blockquote>x</blockquote>`},
		{"heading h1", `<h1>x</h1>`, `<h1>x</h1>`},
		{"heading h6", `<h6>x</h6>`, `<h6>x</h6>`},
		{"line break", `a<br>b`, `a<br>b`},
		{"list", `<ul><li>x</li></ul>`, `<ul><li>x</li></ul>`},
		{"ordered list", `<ol><li>x</li></ol>`, `<ol><li>x</li></ol>`},

		// A dropped tag INSIDE an allowed one: the parent survives, the content does not.
		{"script inside div", `<div>a<script>evil()</script>b</div>`, `<div>ab</div>`},
		{"style inside p", `<p>a<style>.x{}</style>b</p>`, `<p>ab</p>`},
		{"iframe inside blockquote", `<blockquote><iframe src="x"></iframe>keep</blockquote>`, `<blockquote>keep</blockquote>`},

		// A disallowed-but-not-dropped tag: the tag goes, its TEXT stays -- unlike dropContent.
		{"unknown tag keeps its text", `<marquee>keep</marquee>`, `keep`},

		// An allowed tag nested inside a dropped one is removed with it, not resurrected.
		{"allowed inside dropped", `<script><b>gone</b></script>after`, `after`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeWizardHTML(tc.in); got != tc.want {
				t.Errorf("sanitizeWizardHTML(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// sanitizeSectionHTML's two fallback branches, which decide what happens to a section the
// sanitizer cannot rewrite. They pull in opposite directions, so both are pinned:
//
//   - a NON-OBJECT entry is DROPPED. The first version returned it untouched, which is
//     fail-open: nothing requires `sections[i]` to be an object, so a bare
//     `"<script>alert(1)</script>"` was copied verbatim into the response while every object
//     section beside it was sanitized.
//   - an object with NO string `html` is KEPT. `divider` and `button` sections legitimately
//     carry none, and dropping them would silently delete real content.
func TestSanitizeSectionHTMLFallbackBranches(t *testing.T) {
	t.Run("drops a non-object section", func(t *testing.T) {
		got, keep := sanitizeSectionHTML("<script>alert(1)</script>")
		if keep {
			t.Errorf("a non-object section was kept as %v -- it reaches the caller unsanitized", got)
		}
	})

	t.Run("drops a section that is a number", func(t *testing.T) {
		if _, keep := sanitizeSectionHTML(42.0); keep {
			t.Error("a numeric section was kept")
		}
	})

	t.Run("keeps an object with no html field", func(t *testing.T) {
		in := map[string]any{"type": "divider"}
		got, keep := sanitizeSectionHTML(in)
		if !keep {
			t.Fatal("a divider section was dropped -- real content silently deleted")
		}
		obj, ok := got.(map[string]any)
		if !ok || obj["type"] != "divider" {
			t.Errorf("divider section came back as %v", got)
		}
	})

	t.Run("keeps an object whose html is not a string", func(t *testing.T) {
		if _, keep := sanitizeSectionHTML(map[string]any{"type": "rich_text", "html": 7.0}); !keep {
			t.Error("a section with a non-string html was dropped rather than passed through")
		}
	})

	t.Run("does not mutate the input map", func(t *testing.T) {
		in := map[string]any{"type": "rich_text", "html": `<p onclick="x()">t</p>`}
		if _, keep := sanitizeSectionHTML(in); !keep {
			t.Fatal("section was dropped")
		}
		if in["html"] != `<p onclick="x()">t</p>` {
			t.Errorf("the caller's map was mutated in place: %v", in["html"])
		}
	})
}

// A self-closing spelling of a drop-content tag must still suppress what follows it.
//
// None of script/style/iframe/object/embed is a VOID element, so `<script/>` is not
// self-closing in HTML at all: the parser treats it as an open tag and everything after it as
// script content. Not bumping skipDepth therefore leaked the payload as escaped text AND
// escaped the rest of the document with it, so ordinary copy after the tag rendered as visible
// markup.
func TestSanitizeWizardHTMLSuppressesSelfClosingDropTags(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{
			// The bug: `alert(1)` survived, and `<p>after</p>` came back as `&lt;p&gt;...`.
			name: "self-closing script drops its content and does not escape the tail",
			in:   `<p>before</p><script/>alert(1)<p>after</p>`,
			want: `<p>before</p>`,
		},
		{
			name: "self-closing style",
			in:   `<p>a</p><style/>body{x:1}`,
			want: `<p>a</p>`,
		},
		{
			// The NEGATIVE case: a properly closed drop tag ends the skip, so text after it is
			// ordinary copy and must survive. Without this, "fix" the bug by never resuming and
			// every email loses everything after its first <script>.
			name: "a closed script resumes normal output",
			in:   `<p>a</p><script>evil</script>kept`,
			want: `<p>a</p>kept`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeWizardHTML(tc.in); got != tc.want {
				t.Errorf("sanitizeWizardHTML(%q):\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// The EDIT path must store sanitized sections, like the generation path already does.
//
// Not exploitable when written: every current reader of `sess.Sections` re-sanitizes before
// emitting. That is exactly the problem -- it makes the stored invariant depend on all future
// readers remembering to, and a new export, admin tool or raw dump reading the column directly
// would reintroduce the third-sink bug this round closed for the generation path.
func TestSanitizeSectionHTMLCleansAndPreservesUnmodelledFields(t *testing.T) {
	in := map[string]any{
		"type": "rich_text",
		"html": `<p onclick="steal()">hi</p><script>alert(1)</script>`,
		// A field this service does not model. The UI round-trips these, so the sanitizer must
		// copy them through -- persisting a re-marshalled typed form would silently drop them.
		"unmodelled": "keep me",
	}

	out, keep := sanitizeSectionHTML(in)
	if !keep {
		t.Fatal("section was dropped; a rich_text block with html must be kept")
	}
	obj, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("expected a map, got %T", out)
	}
	if got := obj["html"].(string); got != "<p>hi</p>" {
		t.Errorf("html not sanitized:\n got %q\nwant %q", got, "<p>hi</p>")
	}
	if got := obj["unmodelled"]; got != "keep me" {
		t.Errorf("unmodelled field lost: got %v", got)
	}
	// The input must not be mutated: the caller's map may be shared with the typed decode.
	if in["html"].(string) == "<p>hi</p>" {
		t.Error("input map was mutated in place")
	}
}

// Only the RAW-TEXT drop tags open a region a self-close does not end. `object` and `embed` must
// not, or they silently truncate the rest of the block.
//
// Verified against the tokenizer itself rather than inferred: after `<script/>`, `<style/>` or
// `<iframe/>`, golang.org/x/net/html force-consumes the tail as ONE text token, so the region
// really does stay open until a matching end tag. After `<object/>` or `<embed/>` it resumes
// normal tokenization, so no `</object>` ever arrives, `skipDepth` never returns to 0, and every
// following paragraph is suppressed with no error and no marker.
//
// This is the regression the previous round's self-closing fix introduced by over-generalising
// from the raw-text tags -- a truncation traded for a leak, which is worse: the leak was visible.
func TestSanitizeWizardHTMLSelfClosingObjectEmbedDoNotTruncate(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{
			// The regression: `<p>after</p>` was gone entirely.
			name: "self-closing object keeps the trailing content",
			in:   `<p>before</p><object/><p>after</p>`,
			want: `<p>before</p><p>after</p>`,
		},
		{
			name: "self-closing embed keeps the trailing content",
			in:   `<p>before</p><embed/><p>after</p>`,
			want: `<p>before</p><p>after</p>`,
		},
		{
			// The other direction, which must NOT regress: these three ARE raw text, so the
			// payload after a self-close is still suppressed. Fixing object/embed by dropping the
			// bump for all five would reopen the leak this test set exists to hold closed.
			name: "self-closing script still suppresses its payload",
			in:   `<p>before</p><script/>alert(1)<p>after</p>`,
			want: `<p>before</p>`,
		},
		{
			name: "self-closing iframe still suppresses its payload",
			in:   `<p>before</p><iframe/>evil<p>after</p>`,
			want: `<p>before</p>`,
		},
		{
			// A properly closed object must still drop its CONTENT while keeping the tail.
			name: "closed object drops content and resumes",
			in:   `<p>before</p><object>payload</object><p>after</p>`,
			want: `<p>before</p><p>after</p>`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeWizardHTML(tc.in); got != tc.want {
				t.Errorf("sanitizeWizardHTML(%q):\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}
