// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import "testing"

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
