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

// TestApply_SkipsEveryNonHTTPScheme pins the ALLOWLIST.
//
// The original guard was a DENYLIST of mailto:/tel:/#, which let every other non-web action
// through. `javascript:void(0)` — the standard placeholder href emitted by marketing tools for a
// no-op link — became `javascript:void(0)?utm_source=…`, changing the expression the browser
// evaluates. sms:, data: and ftp: mangled the same way. Only http/https may be tagged.
func TestApply_SkipsEveryNonHTTPScheme(t *testing.T) {
	for _, raw := range []string{
		"javascript:void(0)",
		"JavaScript:void(0)", // schemes are case-insensitive (RFC 3986 §3.1)
		"sms:+15551234567",
		"data:text/html,<b>hi</b>",
		"ftp://lf.dev/file.zip",
		"webcal://lf.dev/cal.ics",
	} {
		assert.Equal(t, raw, Apply(raw, testParams(), "cta"),
			"a non-http scheme must come back byte-identical: %q", raw)
	}
}

// TestApply_TagsSchemelessWebLinks is the other half of the allowlist: narrowing to http/https
// must not start REFUSING the ordinary relative links templated email HTML is full of.
func TestApply_TagsSchemelessWebLinks(t *testing.T) {
	p := Params{Source: "s", Medium: "m", Campaign: "c"}
	for _, raw := range []string{
		"/register",              // root-relative
		"//events.lfx.dev/x",     // protocol-relative
		"register",               // bare relative
		"register?ref=1",         // relative with a query
		"/agenda:day-1/sessions", // a colon that is PATH data, not a scheme delimiter
	} {
		assert.Contains(t, Apply(raw, p, ""), "utm_campaign=c",
			"a schemeless web link is still eligible: %q", raw)
	}
	// An uppercase http scheme is tagged (and normalized by url.Parse, as before).
	assert.Contains(t, Apply("HTTPS://lf.dev/e", p, ""), "utm_campaign=c")
}

// TestApply_PercentEncodedUTMKeysAreSeen pins that query KEYS are decoded before comparison.
//
// `?utm%5Fcampaign=hand-picked` decodes to utm_campaign for every normal query reader and for the
// analytics backend, but a raw string comparison sees the literal "utm%5Fcampaign". The
// never-retag guard missed it and appended a SECOND campaign, while the strip left the author's
// original in place AHEAD of it — two conflicting utm_campaign values in one link, with the
// author's the one a first-occurrence reader picks.
func TestApply_PercentEncodedUTMKeysAreSeen(t *testing.T) {
	p := Params{Source: "s", Medium: "m", Campaign: "NEW"}

	t.Run("an encoded utm_campaign key trips the never-retag guard", func(t *testing.T) {
		raw := "https://lf.dev/e?utm%5Fcampaign=hand-picked"
		assert.Equal(t, raw, Apply(raw, p, ""),
			"utm%5Fcampaign IS utm_campaign; the author's campaign must not be overwritten")
	})

	t.Run("an encoded stale utm_ key is stripped, not left to out-rank ours", func(t *testing.T) {
		got := Apply("https://lf.dev/e?utm%5Fsource=facebook&a=1", p, "")
		assert.NotContains(t, got, "facebook", "the stale encoded utm_source must be removed")
		assert.NotContains(t, got, "utm%5Fsource", "the encoded key form must not survive either")
		assert.Contains(t, got, "a=1", "a non-utm sibling is kept")
		assert.Contains(t, got, "utm_source=s")
	})
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

// TestApply_PreservesTheOriginalQueryVerbatim pins that tagging APPENDS rather than rebuilds.
//
// Rebuilding via url.Values.Encode() broke three things at once: it reordered keys (so a token
// restored by value could land on the wrong occurrence), dropped anything it could not
// round-trip (semicolon-separated params vanished entirely), and percent-encoded {{...}}
// tokens. Appending leaves every original byte of the query untouched.
func TestApply_PreservesTheOriginalQueryVerbatim(t *testing.T) {
	t.Run("semicolon params survive", func(t *testing.T) {
		// url.Values cannot round-trip these: they parsed to nothing and the pairs disappeared.
		got := Apply("https://events.lfx.dev/x?a=1;b=2", testParams(), "")
		assert.Contains(t, got, "a=1;b=2", "a param url.Values cannot parse must not be dropped")
		assert.Contains(t, got, "utm_campaign=kubecon-korea-2026")
	})

	t.Run("key order is preserved", func(t *testing.T) {
		got := Apply("https://events.lfx.dev/x?z=1&a=2", testParams(), "")
		assert.Less(t, strings.Index(got, "z=1"), strings.Index(got, "a=2"),
			"the original order must survive; Encode() would sort them")
	})

	t.Run("a live token and a pre-encoded literal stay distinct", func(t *testing.T) {
		// Restoring by VALUE across the whole URL could not tell these two apart, so the
		// encoded literal under `a` was revived as a live token while `z` stayed encoded.
		got := Apply("https://events.lfx.dev/x?z={{contact.id}}&a=%7B%7Bcontact.id%7D%7D", testParams(), "")
		assert.Contains(t, got, "z={{contact.id}}", "the live token stays live")
		assert.Contains(t, got, "a=%7B%7Bcontact.id%7D%7D", "the encoded literal stays encoded")
	})
}

// TestStripUTM pins that EVERY existing utm_* pair is removed before the fresh ones are
// appended. Appending alone leaves the originals first, and url.Values.Get — what most readers
// use — returns the first occurrence, so a stale `utm_source=facebook` would out-rank the
// appended `email` and the link would attribute to the wrong channel while looking tagged.
func TestStripUTM(t *testing.T) {
	assert.Equal(t, "", stripUTM(""))
	assert.Equal(t, "a=1", stripUTM("a=1&utm_campaign="))
	assert.Equal(t, "a=1&b=2", stripUTM("a=1&utm_source=&b=2&utm_medium="))
	// Non-blank utm_* values are removed too — that is the whole point.
	assert.Equal(t, "", stripUTM("utm_source=facebook&utm_medium=cpc"))
	assert.Equal(t, "a=1", stripUTM("utm_source=facebook&a=1&utm_content=x"))
	// Non-utm params are never removed, blank or not.
	assert.Equal(t, "a=&b=2", stripUTM("a=&b=2"))
	// A param merely CONTAINING "utm_" is not a utm param.
	assert.Equal(t, "xutm_source=1", stripUTM("xutm_source=1"))
}

// TestStripUTM_PreservesOriginalSeparators pins that a kept part is re-emitted with the byte that
// ORIGINALLY followed it, so only the removed utm_ pairs change the query.
//
// splitQuery splits on both "&" and ";" (a utm_ pair hidden behind a semicolon must be visible to
// the strip). Re-joining everything with "&" then SHREDDED any non-utm VALUE containing a
// semicolon: "sig=a;b;c&utm_term=old" came back as "sig=a&b&c", turning one signature into three
// empty-valued parameters. The link still resolves, so nothing fails loudly — the destination just
// receives a truncated signature. Signatures, base64 payloads, redirect= targets and ad-tracker
// macros all routinely carry unencoded semicolons.
func TestStripUTM_PreservesOriginalSeparators(t *testing.T) {
	assert.Equal(t, "sig=a;b;c", stripUTM("sig=a;b;c&utm_term=old"),
		"a semicolon inside a non-utm value must not be treated as a separator on the way out")
	assert.Equal(t, "a=1;b=2", stripUTM("a=1;b=2&utm_source=fb"),
		"a genuinely semicolon-separated query keeps its semicolons")
	assert.Equal(t, "a=1&b=2", stripUTM("a=1&b=2&utm_source=fb"),
		"an ampersand-separated query keeps its ampersands")
	assert.Equal(t, "a=1;b=2", stripUTM("utm_source=fb;a=1;b=2"),
		"dropping the leading pair must not promote its separator onto the survivors")
	assert.Equal(t, "redirect=https://x.dev/p?s=1;t=2", stripUTM("redirect=https://x.dev/p?s=1;t=2&utm_medium=cpc"),
		"an embedded redirect target survives whole")
}

// TestApply_SemicolonInsideANonUTMValueSurvives is the end-to-end form of the same bug: the URL
// that ships must differ from the input only by the utm_ pairs.
func TestApply_SemicolonInsideANonUTMValueSurvives(t *testing.T) {
	p := Params{Source: "s", Medium: "m", Campaign: "NEW"}

	got := Apply("https://lf.dev/p?sig=a;b;c&utm_term=old", p, "")
	assert.Equal(t, "https://lf.dev/p?sig=a;b;c&utm_source=s&utm_medium=m&utm_campaign=NEW", got,
		"sig=a;b;c must survive as ONE parameter; splitting it truncates the signature silently")
	assert.NotContains(t, got, "sig=a&b&c", "the value must not be shredded into separate params")
	assert.NotContains(t, got, "utm_term=old", "the stale utm_ pair is still stripped")
}

// TestApply_ReplacesStaleUTMValues pins the end-to-end effect: no duplicate utm keys survive, so
// a first-occurrence reader sees this service's values.
func TestApply_ReplacesStaleUTMValues(t *testing.T) {
	got := Apply("https://events.lfx.dev/x?utm_source=facebook&utm_medium=cpc", testParams(), "")

	assert.NotContains(t, got, "facebook", "a stale utm_source must not survive ahead of ours")
	assert.NotContains(t, got, "cpc")
	assert.Contains(t, got, "utm_source=email")
	assert.Contains(t, got, "utm_medium=LF-Events")
	assert.Equal(t, 1, strings.Count(got, "utm_source="), "no duplicate utm keys")
	assert.Equal(t, 1, strings.Count(got, "utm_medium="))
}

// TestApply_PathTokenOccurrencesKeepTheirIdentity pins the occurrence-identity fix.
//
// URL.String() encodes an already-encoded literal and a live token IDENTICALLY, so a
// replace-by-value restore matched the wrong needle and SWAPPED which occurrence was live:
// `/%7B%7Bcontact.id%7D%7D/{{contact.id}}` came back with the halves reversed. Splicing the
// original path back wholesale is exact by construction.
func TestApply_PathTokenOccurrencesKeepTheirIdentity(t *testing.T) {
	raw := "https://events.lfx.dev/%7B%7Bcontact.id%7D%7D/{{contact.id}}"
	got := Apply(raw, testParams(), "")

	path, _, _ := strings.Cut(got, "?")
	assert.Equal(t, "https://events.lfx.dev/%7B%7Bcontact.id%7D%7D/{{contact.id}}", path,
		"the pre-encoded literal must stay encoded and the live token must stay live")
	assert.Contains(t, got, "utm_campaign=kubecon-korea-2026", "and the link is still tagged")
}

// TestApply_PathTokensSurviveFragments pins the fragment case. Splitting the original on "?"
// alone left "#agenda" inside the "path" when there was no query, so the unescape comparison
// never matched, the restore silently skipped, and the token stayed percent-encoded — HubSpot
// would not substitute it and the personalized link would break at send time.
func TestApply_PathTokensSurviveFragments(t *testing.T) {
	cases := map[string]string{
		"fragment, no query": "https://events.lfx.dev/{{contact.id}}/r#agenda",
		"fragment and query": "https://events.lfx.dev/{{contact.id}}/r?a=1#agenda",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got := Apply(raw, testParams(), "")

			assert.Contains(t, got, "{{contact.id}}", "the token must stay live")
			assert.NotContains(t, got, "%7B", "no brace may remain percent-encoded")
			assert.True(t, strings.HasSuffix(got, "#agenda"), "the fragment must survive, and stay last: %s", got)
			assert.Contains(t, got, "utm_campaign=kubecon-korea-2026")
		})
	}

	// A fragment with no token still round-trips untouched.
	got := Apply("https://events.lfx.dev/r#agenda", testParams(), "")
	assert.True(t, strings.HasSuffix(got, "#agenda"))
}

// TestApply_UppercaseSchemePreservesTokens covers a template written with a non-lower-case
// scheme. url.Parse normalizes the scheme, so the tagged url no longer matches the original
// byte-for-byte; a strict comparison skipped the path restore and shipped the token as
// %7B%7Bcontact.id%7D%7D — which HubSpot does not expand, so every recipient gets a dead link.
//
// The scheme is also expected to come back NORMALIZED: only the token restore is at stake here,
// and re-introducing "HTTPS://" would undo a canonicalization that is otherwise correct.
func TestApply_UppercaseSchemePreservesTokens(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "uppercase scheme, no query",
			in:   "HTTPS://lf.dev/e/{{contact.id}}",
			want: "https://lf.dev/e/{{contact.id}}?utm_source=s&utm_medium=m&utm_campaign=c",
		},
		{
			name: "mixed-case scheme with an existing query",
			in:   "Https://lf.dev/e/{{contact.id}}?x=1",
			want: "https://lf.dev/e/{{contact.id}}?x=1&utm_source=s&utm_medium=m&utm_campaign=c",
		},
		{
			name: "mixed-case scheme with a fragment",
			in:   "HttpS://lf.dev/e/{{contact.id}}#agenda",
			want: "https://lf.dev/e/{{contact.id}}?utm_source=s&utm_medium=m&utm_campaign=c#agenda",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Apply(tc.in, Params{Source: "s", Medium: "m", Campaign: "c"}, "")
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, "%7B", "the token must not ship percent-encoded")
		})
	}
}

// TestSameExceptSchemeCase_OnlyFoldsTheScheme guards the narrow scope of the fold. Host and path
// case are meaningful (a path is case-sensitive, and a differing host is a different link), so
// only the scheme segment may be compared case-insensitively.
func TestSameExceptSchemeCase_OnlyFoldsTheScheme(t *testing.T) {
	assert.True(t, sameExceptSchemeCase("https://lf.dev/a", "HTTPS://lf.dev/a"))
	assert.False(t, sameExceptSchemeCase("https://lf.dev/a", "https://lf.dev/A"),
		"path case is significant and must not be folded")
	assert.False(t, sameExceptSchemeCase("https://lf.dev/a", "https://LF.dev/a"),
		"host case must not be folded here: only the scheme is normalized by url.Parse")
	assert.True(t, sameExceptSchemeCase("/relative/a", "/relative/a"), "schemeless urls still compare")
}

// TestApply_SemicolonQueriesDoNotBypassTheNeverRetagGuard covers the interaction between the
// never-retag guard and Go's query parsing.
//
// Go 1.17+ dropped ";" as a query separator, so url.Query() silently DISCARDS semicolon-delimited
// pairs. A guard built on Values therefore saw no campaign in "?utm_campaign=hand-picked;a=1"
// and happily retagged — replacing an author's deliberate campaign and dropping "a=1" with it.
// In the other ordering the stale pair survived the strip and the URL shipped with TWO
// conflicting utm_campaign values.
func TestApply_SemicolonQueriesDoNotBypassTheNeverRetagGuard(t *testing.T) {
	p := Params{Source: "s", Medium: "m", Campaign: "NEW"}

	t.Run("a hand-picked campaign is never overwritten", func(t *testing.T) {
		// Both orderings: the utm_ pair may be the first field or hidden after a semicolon.
		for _, raw := range []string{
			"https://lf.dev/e?utm_campaign=hand-picked;a=1",
			"https://lf.dev/e?a=1;utm_campaign=hand-picked",
		} {
			got := Apply(raw, p, "")
			assert.Equal(t, raw, got, "a url with a campaign must come back byte-identical")
			assert.NotContains(t, got, "NEW", "the author's campaign must not be replaced")
			assert.Contains(t, got, "a=1", "a sibling param must not be dropped")
			assert.Equal(t, 1, strings.Count(got, "utm_campaign="),
				"a second utm_campaign would leave analytics two conflicting values")
		}
	})

	t.Run("a semicolon query with no campaign is still tagged, separators intact", func(t *testing.T) {
		got := Apply("https://lf.dev/e?a=1;b=2", p, "")
		assert.Equal(t, "https://lf.dev/e?a=1;b=2&utm_source=s&utm_medium=m&utm_campaign=NEW", got,
			"an untagged query is appended to, never reshaped")
	})

	t.Run("an empty campaign is replaced without losing its siblings", func(t *testing.T) {
		got := Apply("https://lf.dev/e?utm_campaign=;a=1", p, "")
		assert.Contains(t, got, "utm_campaign=NEW", "a blank campaign is not a deliberate one")
		assert.Contains(t, got, "a=1", "stripping the utm_ pair must keep the rest")
		assert.Equal(t, 1, strings.Count(got, "utm_campaign="))
	})
}

// TestRawQueryValues_SeesWhatValuesCannot pins the helper directly, including the percent-decode:
// a campaign that arrives encoded is still deliberate, and treating it as empty would defeat the
// guard it backs.
func TestRawQueryValues_SeesWhatValuesCannot(t *testing.T) {
	assert.Equal(t, []string{"hand-picked"}, rawQueryValues("utm_campaign=hand-picked;a=1", "utm_campaign"))
	assert.Equal(t, []string{"", "hand-picked"}, rawQueryValues("utm_campaign=&utm_campaign=hand-picked", "utm_campaign"),
		"every value, not just the first: a leading blank must not mask a real one")
	assert.Equal(t, []string{" "}, rawQueryValues("utm_campaign=%20", "utm_campaign"),
		"values are decoded before the caller trims them")
	assert.Empty(t, rawQueryValues("a=1;b=2", "utm_campaign"))
}

// TestApply_RestoresFragmentTokens covers personalization tokens in the FRAGMENT.
//
// URL.String() escapes the fragment just as it escapes the path, so "#{{contact.id}}" shipped as
// "#%7B%7Bcontact.id%7D%7D" — which HubSpot never expands, so the link arrives broken. Path and
// fragment are restored independently: a url may personalize either alone, and gating one on the
// other would leave the second broken.
func TestApply_RestoresFragmentTokens(t *testing.T) {
	p := Params{Source: "s", Medium: "m", Campaign: "c"}
	cases := []struct{ name, in, want string }{
		{
			name: "fragment token only",
			in:   "https://lf.dev/e#{{contact.id}}",
			want: "https://lf.dev/e?utm_source=s&utm_medium=m&utm_campaign=c#{{contact.id}}",
		},
		{
			name: "tokens in both path and fragment",
			in:   "https://lf.dev/e/{{contact.id}}#{{contact.email}}",
			want: "https://lf.dev/e/{{contact.id}}?utm_source=s&utm_medium=m&utm_campaign=c#{{contact.email}}",
		},
		{
			name: "path token with a plain fragment",
			in:   "https://lf.dev/e/{{contact.id}}#agenda",
			want: "https://lf.dev/e/{{contact.id}}?utm_source=s&utm_medium=m&utm_campaign=c#agenda",
		},
		{
			name: "existing query and a fragment token",
			in:   "https://lf.dev/e?x=1#{{contact.id}}",
			want: "https://lf.dev/e?x=1&utm_source=s&utm_medium=m&utm_campaign=c#{{contact.id}}",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Apply(tc.in, p, "")
			assert.Equal(t, tc.want, got)
			assert.NotContains(t, got, "%7B", "no token may ship percent-encoded")
		})
	}
}
