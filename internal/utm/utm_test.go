// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package utm

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// query parses a tagged URL's query so assertions read by parameter rather than by string
// matching (parameter ORDER is not part of the contract; url.Values.Encode sorts by key).
func query(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u.Query()
}

func testParams() Params {
	return Params{Campaign: "kubecon-korea-2026"}
}

// TestApply_AddsTheLFEventsConvention pins the source/medium pair. medium is "LF-Events", NOT
// "email" — the warehouse's channel reporting keys on this exact pair, so a change silently
// re-buckets historical comparisons instead of failing.
func TestApply_AddsTheLFEventsConvention(t *testing.T) {
	got := query(t, Apply("https://events.lfx.dev/kubecon", testParams(), "hero-cta"))

	assert.Equal(t, "email", got.Get("utm_source"))
	assert.Equal(t, "LF-Events", got.Get("utm_medium"))
	assert.Equal(t, "kubecon-korea-2026", got.Get("utm_campaign"))
	assert.Equal(t, "hero-cta", got.Get("utm_content"))
	assert.Empty(t, got.Get("utm_term"), "term is optional and omitted when unset")
}

// TestApply_PreservesExistingQueryAndFragment pins that tagging is additive. Event URLs
// routinely carry their own query parameters (registration codes, tracking ids); dropping one
// would break the destination, and losing the fragment would break in-page navigation.
func TestApply_PreservesExistingQueryAndFragment(t *testing.T) {
	raw := "https://events.lfx.dev/reg?code=ABC123&tier=speaker#agenda"
	tagged := Apply(raw, testParams(), "")

	got := query(t, tagged)
	assert.Equal(t, "ABC123", got.Get("code"))
	assert.Equal(t, "speaker", got.Get("tier"))
	assert.Equal(t, "kubecon-korea-2026", got.Get("utm_campaign"))
	assert.True(t, strings.HasSuffix(tagged, "#agenda"), "the fragment must survive: %s", tagged)
}

// TestApply_NeverDoubleTags pins the most damaging silent failure. A link an author tagged by
// hand carries a deliberate campaign; overwriting it produces a URL that still WORKS but
// reports to the wrong campaign, so nothing surfaces the loss.
func TestApply_NeverDoubleTags(t *testing.T) {
	raw := "https://events.lfx.dev/kubecon?utm_campaign=hand-picked&utm_source=newsletter"
	assert.Equal(t, raw, Apply(raw, testParams(), "hero-cta"),
		"a link that already carries a campaign must be left exactly as-is")

	// A BLANK utm_campaign is not a real tag — it attributes nothing — so it may be replaced.
	blank := "https://events.lfx.dev/kubecon?utm_campaign="
	assert.Equal(t, "kubecon-korea-2026", query(t, Apply(blank, testParams(), "")).Get("utm_campaign"))
}

// TestApply_SkipsNonWebTargets pins that mailto:/tel:/anchor links are never tagged. Appending
// a query string to any of them produces a broken link, not an attributed one.
func TestApply_SkipsNonWebTargets(t *testing.T) {
	for _, raw := range []string{
		"mailto:events@linuxfoundation.org",
		"MAILTO:events@linuxfoundation.org", // HTML is case-insensitive about schemes
		"tel:+15551234567",
		"#agenda",
		"  #agenda  ",
	} {
		assert.Equal(t, raw, Apply(raw, testParams(), "cta"), "must not tag %q", raw)
	}
}

// TestApply_RefusesWithoutACampaign pins that no campaign means no tagging. Emitting
// utm_source/utm_medium with an empty utm_campaign is WORSE than leaving the link bare: the
// session looks tagged in reports while attributing to nothing.
func TestApply_RefusesWithoutACampaign(t *testing.T) {
	raw := "https://events.lfx.dev/kubecon"
	for _, p := range []Params{{}, {Campaign: "   "}, {Source: "email", Medium: "LF-Events"}} {
		assert.Equal(t, raw, Apply(raw, p, "cta"))
	}
	assert.Empty(t, Apply("", testParams(), "cta"))
}

// TestApply_ReturnsUnparseableURLsUnchanged pins the fail-safe direction: a link this package
// cannot parse must be passed through, not dropped or mangled. A broken link in a sent email
// is far worse than an untagged one.
func TestApply_ReturnsUnparseableURLsUnchanged(t *testing.T) {
	raw := "https://events.lfx.dev/\x7f\x00bad"
	assert.Equal(t, raw, Apply(raw, testParams(), "cta"))
}

// TestApply_HonoursExplicitOverrides pins that a caller can override the defaults and add an
// optional term.
func TestApply_HonoursExplicitOverrides(t *testing.T) {
	p := Params{Source: "hubspot", Medium: "nurture", Campaign: "c1", Term: "keynote"}
	got := query(t, Apply("https://events.lfx.dev/x", p, ""))

	assert.Equal(t, "hubspot", got.Get("utm_source"))
	assert.Equal(t, "nurture", got.Get("utm_medium"))
	assert.Equal(t, "keynote", got.Get("utm_term"))
	assert.Empty(t, got.Get("utm_content"), "content is omitted when the caller passes none")
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Register Now":            "register-now",
		"  KubeCon + CloudNative": "kubecon-cloudnative",
		"Already-A-Slug":          "already-a-slug",
		"2026 Edition":            "2026-edition",
		"!!!":                     "",
		"":                        "",
		"   ":                     "",
	}
	for in, want := range cases {
		assert.Equal(t, want, Slug(in), "input %q", in)
	}
}

// TestSlugWithSuffix pins the suffix rules, including the two cases where appending would
// produce a doubled suffix ("register-cta-cta").
func TestSlugWithSuffix(t *testing.T) {
	cases := []struct{ text, suffix, want string }{
		{"Register Now", "cta", "register-now-cta"},
		{"Register Now", "", "register-now"},
		{"", "cta", "cta"},                      // nothing sluggable: the suffix carries it
		{"!!!", "cta", "cta"},                   // same
		{"Register CTA", "cta", "register-cta"}, // already ends with the suffix
		{"cta", "cta", "cta"},                   // slug IS the suffix
		{"", "", ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, SlugWithSuffix(c.text, c.suffix), "text=%q suffix=%q", c.text, c.suffix)
	}
}

// TestResolve_PrefersTheConfiguredCampaign pins the precedence. An operator-set utmCampaign on
// the brief config is a deliberate choice and must win over anything derived from a
// generated name.
func TestResolve_PrefersTheConfiguredCampaign(t *testing.T) {
	got := Resolve("kubecon-eu-2026-hs", "KubeCon Korea 2026 — brief-1")

	assert.Equal(t, "kubecon-eu-2026-hs", got.Params.Campaign)
	assert.Equal(t, SourceBriefConfig, got.Source)
	assert.Equal(t, DefaultSource, got.Params.Source)
	assert.Equal(t, DefaultMedium, got.Params.Medium)
}

// TestResolve_DerivesFromTheEmailName covers the common case: no HubSpot campaign, so the
// campaign is slugified from the deterministic email name.
func TestResolve_DerivesFromTheEmailName(t *testing.T) {
	got := Resolve("", "KubeCon Korea 2026 — brief-1")

	assert.Equal(t, "kubecon-korea-2026-brief-1", got.Params.Campaign)
	assert.Equal(t, SourceDerived, got.Source)
}

// TestResolve_NeverYieldsAnEmptyCampaign pins the invariant the whole package rests on. An
// empty utm_campaign makes a session unattributable while still LOOKING tagged, so every input
// — including ones with nothing sluggable — must produce a real value.
func TestResolve_NeverYieldsAnEmptyCampaign(t *testing.T) {
	for _, name := range []string{"", "   ", "!!!", "—"} {
		got := Resolve("", name)
		assert.Equal(t, FallbackCampaign, got.Params.Campaign, "name %q", name)
		assert.Equal(t, SourceFallback, got.Source)
		assert.NotEmpty(t, got.Params.Campaign)
	}
	// Whitespace-only campaign values are not real configuration either.
	assert.Equal(t, SourceDerived, Resolve("   ", "Some Email").Source)
}

// TestApply_NeverDoubleTagsWithRepeatedParam pins the multi-VALUE case. url.Values.Get returns
// only the FIRST value, so `?utm_campaign=&utm_campaign=hand-picked` slipped past the
// never-retag guard and Set then deleted the author's deliberate campaign — the exact silent
// overwrite the guard exists to prevent, hidden behind a leading empty value.
func TestApply_NeverDoubleTagsWithRepeatedParam(t *testing.T) {
	raw := "https://events.lfx.dev/x?utm_campaign=&utm_campaign=hand-picked"
	assert.Equal(t, raw, Apply(raw, testParams(), "cta"),
		"a non-empty campaign in ANY position must protect the link")

	// All-empty values are still not a real tag, so tagging may proceed.
	blank := "https://events.lfx.dev/x?utm_campaign=&utm_campaign="
	assert.NotEqual(t, blank, Apply(blank, testParams(), ""))
}

// TestApply_PreservesTemplateTokens pins that tagging never breaks HubSpot personalization.
//
// HubSpot substitutes {{...}} at SEND time. url.Parse/String percent-encodes the braces (and any
// spaces inside), so a tagged link would carry %7B%7B…%7D%7D, HubSpot would never recognise the
// token, and every personalized link in the email would break. Tagging must not change where a
// link goes.
func TestApply_PreservesTemplateTokens(t *testing.T) {
	cases := []struct{ name, raw, mustContain string }{
		{"query position", "https://events.lfx.dev/r?id={{contact.hs_object_id}}", "id={{contact.hs_object_id}}"},
		{"path position", "https://events.lfx.dev/{{event.slug}}/register", "/{{event.slug}}/register"},
		{"token with spaces", "https://events.lfx.dev/{{ event.slug }}/r", "{{ event.slug }}"},
		{"several tokens", "https://events.lfx.dev/{{a}}/x?u={{b}}", "{{a}}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Apply(c.raw, testParams(), "cta")
			assert.Contains(t, got, c.mustContain, "the token must survive tagging")
			assert.NotContains(t, got, "%7B", "no brace may remain percent-encoded")
			assert.NotContains(t, got, "%7D")
			// The tag must still have been applied.
			assert.Contains(t, got, "utm_campaign=kubecon-korea-2026")
		})
	}
}

// TestRestoreTemplateTokens_OnlyRestoresWhatWasThere guards against inventing a token the
// author did not write — the restore is driven by the ORIGINAL url, not by pattern-matching the
// tagged output.
func TestRestoreTemplateTokens_OnlyRestoresWhatWasThere(t *testing.T) {
	// An escaped sequence that was never a token in the original must be left alone.
	tagged := "https://events.lfx.dev/x?q=%7B%7Bnot-a-token%7D%7D"
	assert.Equal(t, tagged, restoreTemplateTokens(tagged, "https://events.lfx.dev/x?q=literal"))

	// A URL with no tokens is returned untouched.
	plain := "https://events.lfx.dev/x?a=1"
	assert.Equal(t, plain, restoreTemplateTokens(plain, plain))
}
