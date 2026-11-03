// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// ---- pure-logic helpers ----------------------------------------------------

func TestTruncateRunes(t *testing.T) {
	cases := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"under limit", "hello", 10, "hello"},
		{"exact limit", "hello", 5, "hello"},
		{"over limit ascii", "hello world", 5, "hello"},
		{"multibyte not split", "日本語テスト", 3, "日本語"},
		{"empty", "", 5, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateRunes(tc.s, tc.n); got != tc.want {
				t.Errorf("truncateRunes(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
			}
		})
	}
}

func TestTruncateWeighted(t *testing.T) {
	cases := []struct {
		name      string
		s         string
		maxWeight int
		want      string
	}{
		{"ascii under limit counts 1 per rune", "hello", 10, "hello"},
		{"ascii over limit truncates by rune count", "hello world", 5, "hello"},
		{"CJK counts 2 per rune, not 1", "日本語テスト", 6, "日本語"},
		{"CJK never exceeds maxWeight, partial rune dropped entirely", "日本語", 5, "日本"},
		{"fullwidth forms count 2 like CJK", "ＡＢＣ", 4, "ＡＢ"},
		{"empty", "", 5, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateWeighted(tc.s, tc.maxWeight)
			if got != tc.want {
				t.Errorf("truncateWeighted(%q, %d) = %q, want %q", tc.s, tc.maxWeight, got, tc.want)
			}
		})
	}
}

// TestComposeAdCopy_CJKHeadlineStaysUnderEffectiveWidth pins the actual bug: a CJK-heavy
// eventName must not survive composeAdCopy with a headline whose double-width Google Ads weight
// exceeds maxHeadlineWeight (30), even though its plain rune count is far under 30. A caller that
// only rune-counts before dispatch would ship a headline Google Ads rejects at mutate time with
// LINE_TOO_WIDE, orphaning the ad group/ad tree GA-3b builds around it.
func TestComposeAdCopy_CJKHeadlineStaysUnderEffectiveWidth(t *testing.T) {
	eventName := strings.Repeat("日本語テスト", 4) // 24 runes, weight 48 — below 30 by rune count but
	// above 30 by Google's double-width weight (48 > 30).
	ad, err := composeAdCopy(nil, nil, eventName, "Project")
	if err != nil {
		t.Fatalf("composeAdCopy: %v", err)
	}
	for _, h := range ad.headlines {
		weight := 0
		for _, r := range h {
			weight += googleAdsCharWeight(r)
		}
		if weight > maxHeadlineWeight {
			t.Errorf("headline %q has Google Ads weight %d, want <= %d (LINE_TOO_WIDE risk)", h, weight, maxHeadlineWeight)
		}
	}
}

// callerCopy trims and skips blanks, refuses what Google would refuse, and drops (with a note)
// an exact duplicate. Caller text is never cut (#441).
func TestCallerCopy(t *testing.T) {
	cases := []struct {
		name       string
		candidates []string
		maxWeight  int
		maxCount   int
		want       []string
		notes      int
		refused    string
	}{
		{"trims and skips blanks without a note", []string{"  a  ", "", "   ", "b"}, 10, 10, []string{"a", "b"}, 0, ""},
		{"drops a duplicate with a note", []string{"abc", "abc", "d"}, 10, 10, []string{"abc", "d"}, 1, ""},
		{"refuses a line over the width, never cuts it", []string{"abcdef"}, 3, 10, nil, 0, "display width of 6, exceeding the 3 limit"},
		{"counts CJK double", []string{"日本語"}, 5, 10, nil, 0, "display width of 6"},
		{"drops lines past the cap with one note", []string{"a", "b", "c", "d"}, 10, 2, []string{"a", "b"}, 1, ""},
		{"no note when the list ends at the cap", []string{"a", "b"}, 10, 2, []string{"a", "b"}, 0, ""},
		{"blank rows past the cap are counted, not searched", []string{"a", "b", "", "  ", "c"}, 10, 2, []string{"a", "b"}, 1, ""},
		{"a line past the cap is not measured", []string{"a", "b", "far too wide to send"}, 3, 2, []string{"a", "b"}, 1, ""},
		{"a duplicate does not count toward the cap", []string{"a", "a", "b"}, 10, 2, []string{"a", "b"}, 1, ""},
		{"nil input", nil, 10, 10, nil, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var notes []string
			got, err := callerCopy("headline", tc.candidates, tc.maxWeight, tc.maxCount, &notes)
			if tc.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("err = %v, want a refusal containing %q", err, tc.refused)
				}
				return
			}
			if err != nil {
				t.Fatalf("callerCopy: %v", err)
			}
			if !strSliceEqual(got, tc.want) {
				t.Errorf("callerCopy(%v) = %v, want %v", tc.candidates, got, tc.want)
			}
			if len(notes) != tc.notes {
				t.Errorf("notes = %v, want %d", notes, tc.notes)
			}
		})
	}
}

func TestPadUnique(t *testing.T) {
	cases := []struct {
		name     string
		base     []string
		fallback []string
		maxRunes int
		min      int
		max      int
		want     []string
	}{
		{"already at min, no padding", []string{"a", "b", "c"}, []string{"d"}, 10, 3, 5, []string{"a", "b", "c"}},
		{"pads until min", []string{"a"}, []string{"b", "c", "d"}, 10, 3, 5, []string{"a", "b", "c"}},
		{"skips duplicates in fallback", []string{"a"}, []string{"a", "b"}, 10, 2, 5, []string{"a", "b"}},
		{"stops at max even if below min", []string{}, []string{"a", "b", "c"}, 10, 5, 2, []string{"a", "b"}},
		{"empty fallback entries skipped", []string{"a"}, []string{"  ", "b"}, 10, 2, 5, []string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var notes []string
			got := padUnique("headline", tc.base, tc.fallback, tc.maxRunes, tc.min, tc.max, &notes)
			if !strSliceEqual(got, tc.want) {
				t.Errorf("padUnique(%v, %v, %d, %d, %d) = %v, want %v", tc.base, tc.fallback, tc.maxRunes, tc.min, tc.max, got, tc.want)
			}
			// One note per line added, so an operator sees every line they did not write.
			if added := len(got) - len(tc.base); len(notes) != added {
				t.Errorf("notes = %v, want one per added line (%d)", notes, added)
			}
		})
	}
}

func TestDefaultHeadlinesAndDescriptions(t *testing.T) {
	if got := defaultHeadlines(""); got != nil {
		t.Errorf("defaultHeadlines(\"\") = %v, want nil", got)
	}
	if got := defaultHeadlines("  "); got != nil {
		t.Errorf("defaultHeadlines(whitespace) = %v, want nil", got)
	}
	headlines := defaultHeadlines("KubeCon")
	if len(headlines) == 0 || headlines[0] != "KubeCon" {
		t.Errorf("defaultHeadlines(%q) = %v, want a non-empty slice starting with the event name", "KubeCon", headlines)
	}

	if got := defaultDescriptions("", "CNCF"); got != nil {
		t.Errorf("defaultDescriptions(empty event) = %v, want nil", got)
	}
	withProject := defaultDescriptions("KubeCon", "CNCF")
	if len(withProject) == 0 {
		t.Fatalf("defaultDescriptions with a project must return at least one entry, got empty slice")
	}
	if !strings.Contains(withProject[0], "CNCF") {
		t.Errorf("defaultDescriptions with a project must mention it in the first entry, got %v", withProject)
	}
	withoutProject := defaultDescriptions("KubeCon", "")
	if len(withoutProject) == 0 {
		t.Errorf("defaultDescriptions without a project must still return entries")
	}
	if len(withoutProject) >= len(withProject) || withProject[0] == withoutProject[0] {
		t.Errorf("omitting the project should drop the project-specific description")
	}
}

// The overflow note counts every row past the cap and names the first one when it has text.
func TestCallerCopy_OverflowNoteCountsTheRowsAfterTheCap(t *testing.T) {
	for _, tc := range []struct {
		name       string
		candidates []string
		want       string
	}{
		{"names the first row", []string{"a", "b", "c", "d"}, `did not send the 2 headline row(s) after the 2 Google takes, from "c" on`},
		{"a blank first row is counted but not named", []string{"a", "b", "", "c"}, "did not send the 2 headline row(s) after the 2 Google takes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var notes []string
			if _, err := callerCopy("headline", tc.candidates, 10, 2, &notes); err != nil {
				t.Fatalf("callerCopy: %v", err)
			}
			if len(notes) != 1 || notes[0] != tc.want {
				t.Errorf("notes = %q, want [%q]", notes, tc.want)
			}
		})
	}
}

// However long the caller's list, the notes stay bounded: one for the duplicates, one for
// the overflow. Each note becomes a persisted step, so a per-line note would let one
// request write thousands of them.
func TestCallerCopy_NotesStayBoundedForALongList(t *testing.T) {
	var lines []string
	for i := 0; i < 1000; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i), fmt.Sprintf("line %d", i))
	}
	var notes []string
	got, err := callerCopy("headline", lines, maxHeadlineWeight, maxHeadlines, &notes)
	if err != nil {
		t.Fatalf("callerCopy: %v", err)
	}
	if len(got) != maxHeadlines {
		t.Errorf("kept %d headlines, want %d", len(got), maxHeadlines)
	}
	if len(notes) > 2 {
		t.Errorf("got %d notes for one long list, want at most 2: %v", len(notes), notes)
	}
}

func TestComposeAdCopy(t *testing.T) {
	t.Run("caller copy used verbatim when sufficient, with no notes", func(t *testing.T) {
		headlines := []string{"H1", "H2", "H3"}
		descriptions := []string{"D1", "D2"}
		ad, err := composeAdCopy(headlines, descriptions, "KubeCon", "CNCF")
		if err != nil {
			t.Fatalf("composeAdCopy: %v", err)
		}
		if !strSliceEqual(ad.headlines, headlines) || !strSliceEqual(ad.descriptions, descriptions) {
			t.Errorf("ad = %+v, want the caller's copy unchanged", ad)
		}
		if len(ad.copyNotes) != 0 {
			t.Errorf("notes = %v, want none for copy that was not changed", ad.copyNotes)
		}
	})

	t.Run("pads short caller copy and notes every added line", func(t *testing.T) {
		ad, err := composeAdCopy([]string{"Only One"}, nil, "KubeCon", "CNCF")
		if err != nil {
			t.Fatalf("composeAdCopy: %v", err)
		}
		if len(ad.headlines) != minHeadlines || ad.headlines[0] != "Only One" {
			t.Errorf("headlines = %v, want the caller's first, padded to %d", ad.headlines, minHeadlines)
		}
		if len(ad.descriptions) != minDescriptions {
			t.Errorf("descriptions = %v, want %d", ad.descriptions, minDescriptions)
		}
		added := (len(ad.headlines) - 1) + len(ad.descriptions)
		if len(ad.copyNotes) != added {
			t.Fatalf("notes = %v, want one per added line (%d)", ad.copyNotes, added)
		}
		for _, h := range ad.headlines[1:] {
			if !strings.Contains(strings.Join(ad.copyNotes, "\n"), `added default headline "`+h+`"`) {
				t.Errorf("notes %v do not name the added headline %q", ad.copyNotes, h)
			}
		}
	})

	t.Run("refuses over-wide caller copy instead of cutting it", func(t *testing.T) {
		long := strings.Repeat("x", maxHeadlineWeight+1)
		if _, err := composeAdCopy([]string{long, "H2", "H3"}, []string{"D1", "D2"}, "KubeCon", "CNCF"); err == nil {
			t.Fatal("accepted a headline over Google's width, which used to be cut mid-word")
		}
		longD := strings.Repeat("y", maxDescriptionWeight+1)
		if _, err := composeAdCopy([]string{"H1", "H2", "H3"}, []string{longD, "D2"}, "KubeCon", "CNCF"); err == nil {
			t.Fatal("accepted a description over Google's width")
		}
	})

	t.Run("no caller copy, no event name is a hard error", func(t *testing.T) {
		if _, err := composeAdCopy(nil, nil, "", ""); err == nil {
			t.Error("expected an error when there is no usable copy at all")
		}
	})

	t.Run("empty caller descriptions still padded even with headlines present", func(t *testing.T) {
		ad, err := composeAdCopy([]string{"H1", "H2", "H3"}, nil, "KubeCon", "CNCF")
		if err != nil {
			t.Fatalf("composeAdCopy: %v", err)
		}
		if len(ad.descriptions) < minDescriptions {
			t.Errorf("descriptions = %v, want at least %d", ad.descriptions, minDescriptions)
		}
	})
}

func strSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---- buildAdFinalURL / setIfAbsent -----------------------------------------

func TestBuildAdFinalURL(t *testing.T) {
	t.Run("empty registration URL is an error", func(t *testing.T) {
		if _, err := buildAdFinalURL(utmMediumSearch, "", "slug", "Event", "Proj", "suffix"); err == nil {
			t.Error("expected an error for an empty registration URL")
		}
	})

	t.Run("invalid scheme is rejected", func(t *testing.T) {
		if _, err := buildAdFinalURL(utmMediumSearch, "ftp://example.com/register", "slug", "Event", "Proj", "suffix"); err == nil {
			t.Error("expected an error for a non-http(s) scheme")
		}
	})

	t.Run("no host is rejected", func(t *testing.T) {
		if _, err := buildAdFinalURL(utmMediumSearch, "https:///register", "slug", "Event", "Proj", "suffix"); err == nil {
			t.Error("expected an error for a URL with no host")
		}
	})

	t.Run("tags utm params using the event slug", func(t *testing.T) {
		got, err := buildAdFinalURL(utmMediumSearch, "https://example.com/register", "kubecon-na-2026", "KubeCon NA", "CNCF", "brief-1")
		if err != nil {
			t.Fatalf("buildAdFinalURL: %v", err)
		}
		u, _ := url.Parse(got)
		q := u.Query()
		if q.Get("utm_source") != "google" {
			t.Errorf("utm_source = %q, want google", q.Get("utm_source"))
		}
		if q.Get("utm_medium") != utmMediumSearch {
			t.Errorf("utm_medium = %q, want the medium passed in (%s)", q.Get("utm_medium"), utmMediumSearch)
		}
		if q.Get("utm_campaign") != "kubecon-na-2026" {
			t.Errorf("utm_campaign = %q, want the event slug", q.Get("utm_campaign"))
		}
		if q.Get("utm_content") != "CNCF" {
			t.Errorf("utm_content = %q, want the project", q.Get("utm_content"))
		}
	})

	t.Run("falls back from slug to event name to name suffix", func(t *testing.T) {
		got, err := buildAdFinalURL(utmMediumSearch, "https://example.com/register", "", "KubeCon NA", "CNCF", "brief-1")
		if err != nil {
			t.Fatalf("buildAdFinalURL: %v", err)
		}
		u, _ := url.Parse(got)
		if u.Query().Get("utm_campaign") == "" {
			t.Error("utm_campaign should fall back to a sanitized event name")
		}

		got, err = buildAdFinalURL(utmMediumSearch, "https://example.com/register", "", "", "CNCF", "brief-1")
		if err != nil {
			t.Fatalf("buildAdFinalURL: %v", err)
		}
		u, _ = url.Parse(got)
		if u.Query().Get("utm_campaign") == "" {
			t.Error("utm_campaign should fall back to the name suffix when slug and event name are both empty")
		}
	})

	t.Run("overwrites utm_source and utm_medium but preserves other params", func(t *testing.T) {
		got, err := buildAdFinalURL(utmMediumSearch, "https://example.com/register?utm_source=newsletter&utm_medium=email&ref=abc", "slug", "Event", "Proj", "suffix")
		if err != nil {
			t.Fatalf("buildAdFinalURL: %v", err)
		}
		u, _ := url.Parse(got)
		q := u.Query()
		if q.Get("utm_source") != "google" {
			t.Errorf("utm_source = %q, want google (overwrite pre-existing for accurate attribution)", q.Get("utm_source"))
		}
		if q.Get("utm_medium") != utmMediumSearch {
			t.Errorf("utm_medium = %q, want %s (overwrite pre-existing for accurate attribution)", q.Get("utm_medium"), utmMediumSearch)
		}
		if q.Get("ref") != "abc" {
			t.Errorf("ref = %q, want the pre-existing non-utm param preserved", q.Get("ref"))
		}
		if q.Get("utm_campaign") != "slug" {
			t.Errorf("utm_campaign = %q, want slug", q.Get("utm_campaign"))
		}
	})
}

func TestRedactURLForError(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"strips query and fragment", "https://example.com/register?token=secret&utm_source=x#frag", "https://example.com/register"},
		{"strips userinfo", "https://user:pass@example.com/register", "https://example.com/register"}, // secretlint-disable-line -- fixture asserting userinfo is stripped, not a real credential
		{"preserves port", "https://example.com:8443/register", "https://example.com:8443/register"},
		{"unparseable input redacted", "https://[::1", "(redacted)"},
		{"empty input redacted", "", "(redacted)"},
		{"whitespace-only input redacted", "   ", "(redacted)"},
		{"relative path redacted", "/register?token=secret", "(redacted)"},
		{"scheme with no host is redacted", "mailto:", "(redacted)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactURLForError(tc.raw); got != tc.want {
				t.Errorf("redactURLForError(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestBuildAdFinalURL_RejectsUserinfoAndMalformedQuery(t *testing.T) {
	t.Run("embedded userinfo is rejected", func(t *testing.T) {
		_, err := buildAdFinalURL(utmMediumSearch, "https://user:pass@example.com/register", "slug", "Event", "Proj", "suffix") // secretlint-disable-line -- fixture asserting embedded userinfo is rejected, not a real credential
		if err == nil {
			t.Fatal("expected an error for a registration URL with embedded userinfo")
		}
		if !strings.Contains(err.Error(), "userinfo") {
			t.Errorf("error = %v, want it to mention userinfo", err)
		}
		if strings.Contains(err.Error(), "user:pass") {
			t.Errorf("error = %v, must not echo the embedded credentials", err)
		}
	})

	t.Run("malformed query is rejected rather than silently dropped", func(t *testing.T) {
		_, err := buildAdFinalURL(utmMediumSearch, "https://example.com/register?%zz", "slug", "Event", "Proj", "suffix")
		if err == nil {
			t.Fatal("expected an error for a malformed query string")
		}
		if !strings.Contains(err.Error(), "malformed query") {
			t.Errorf("error = %v, want it to mention the malformed query", err)
		}
	})
}

func TestSetIfAbsent(t *testing.T) {
	q := url.Values{"utm_source": []string{"existing"}}
	setIfAbsent(q, "utm_source", "new")
	if q.Get("utm_source") != "existing" {
		t.Errorf("setIfAbsent must not overwrite an existing key, got %q", q.Get("utm_source"))
	}
	setIfAbsent(q, "utm_medium", "cpc")
	if q.Get("utm_medium") != "cpc" {
		t.Errorf("setIfAbsent must set an absent key, got %q", q.Get("utm_medium"))
	}
}

// *url.Error renders the full request URL, query included, so wrapping one with %w
// puts a caller's signed URL into a persisted error however carefully the format
// arguments were redacted. redactedCause renders the operation and the inner cause
// and nothing else — while leaving the chain intact for errors.Is/As, which is what
// the create-path ambiguity classification keys on.
func TestRedactedCause_DropsTheURLAndKeepsTheChain(t *testing.T) {
	const secret = "SECRETSIGNATURE"
	inner := context.DeadlineExceeded
	ue := &url.Error{
		Op:  "Get",
		URL: "https://cdn.example.org/a.png?sig=" + secret,
		Err: inner,
	}
	if !strings.Contains(ue.Error(), secret) {
		t.Fatal("precondition: *url.Error is expected to render the query string")
	}

	got := redactedCause{ue}
	if strings.Contains(got.Error(), secret) {
		t.Errorf("the redacted cause still carries the query string: %v", got)
	}
	if !strings.Contains(got.Error(), "Get") {
		t.Errorf("the operation is load-bearing for diagnosis and must survive: %v", got)
	}
	if !errors.Is(got, inner) {
		t.Error("the original chain must stay reachable through Unwrap")
	}

	if s := (redactedCause{nil}).Error(); s != "(redacted)" {
		t.Errorf("a nil cause must render as a placeholder, got %q", s)
	}
}
