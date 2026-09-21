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
// Sanitising per render path is what allowed that: every new consumer of the sections is a
// fresh chance to forget. The sanitise now happens where the sections are PARSED, so a
// consumer cannot receive unsanitised HTML without going around the parser entirely.
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
		t.Errorf("sanitising removed the legitimate text too: %q", html)
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
