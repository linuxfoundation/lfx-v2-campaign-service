// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
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

func TestBoundedUniqueCopy(t *testing.T) {
	cases := []struct {
		name       string
		candidates []string
		maxRunes   int
		maxCount   int
		want       []string
	}{
		{"trims and drops empties", []string{"  a  ", "", "   ", "b"}, 10, 10, []string{"a", "b"}},
		{"dedupes after truncation", []string{"abcdef", "abcxyz"}, 3, 10, []string{"abc"}},
		{"caps at maxCount", []string{"a", "b", "c", "d"}, 10, 2, []string{"a", "b"}},
		{"nil input", nil, 10, 10, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := boundedUniqueCopy(tc.candidates, tc.maxRunes, tc.maxCount)
			if !strSliceEqual(got, tc.want) {
				t.Errorf("boundedUniqueCopy(%v, %d, %d) = %v, want %v", tc.candidates, tc.maxRunes, tc.maxCount, got, tc.want)
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
			got := padUnique(tc.base, tc.fallback, tc.maxRunes, tc.min, tc.max)
			if !strSliceEqual(got, tc.want) {
				t.Errorf("padUnique(%v, %v, %d, %d, %d) = %v, want %v", tc.base, tc.fallback, tc.maxRunes, tc.min, tc.max, got, tc.want)
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
	if len(withProject) == 0 || !strings.Contains(withProject[0], "CNCF") {
		t.Errorf("defaultDescriptions with a project must mention it, got %v", withProject)
	}
	withoutProject := defaultDescriptions("KubeCon", "")
	if len(withoutProject) == 0 {
		t.Errorf("defaultDescriptions without a project must still return entries")
	}
	if len(withoutProject) >= len(withProject) && withProject[0] == withoutProject[0] {
		t.Errorf("omitting the project should drop the project-specific description")
	}
}

func TestComposeAdCopy(t *testing.T) {
	t.Run("caller copy used verbatim when sufficient", func(t *testing.T) {
		headlines := []string{"H1", "H2", "H3"}
		descriptions := []string{"D1", "D2"}
		gotH, gotD, err := composeAdCopy(headlines, descriptions, "KubeCon", "CNCF")
		if err != nil {
			t.Fatalf("composeAdCopy: %v", err)
		}
		if !strSliceEqual(gotH, headlines) {
			t.Errorf("headlines = %v, want %v", gotH, headlines)
		}
		if !strSliceEqual(gotD, descriptions) {
			t.Errorf("descriptions = %v, want %v", gotD, descriptions)
		}
	})

	t.Run("pads short caller copy with placeholders", func(t *testing.T) {
		gotH, gotD, err := composeAdCopy([]string{"Only One"}, nil, "KubeCon", "CNCF")
		if err != nil {
			t.Fatalf("composeAdCopy: %v", err)
		}
		if len(gotH) < minHeadlines {
			t.Errorf("headlines = %v, want at least %d", gotH, minHeadlines)
		}
		if gotH[0] != "Only One" {
			t.Errorf("caller-supplied headline must be preserved first, got %v", gotH)
		}
		if len(gotD) < minDescriptions {
			t.Errorf("descriptions = %v, want at least %d", gotD, minDescriptions)
		}
	})

	t.Run("no caller copy, no event name is a hard error", func(t *testing.T) {
		_, _, err := composeAdCopy(nil, nil, "", "")
		if err == nil {
			t.Error("expected an error when there is no usable copy at all")
		}
	})

	t.Run("empty caller descriptions still padded even with headlines present", func(t *testing.T) {
		_, gotD, err := composeAdCopy([]string{"H1", "H2", "H3"}, nil, "KubeCon", "CNCF")
		if err != nil {
			t.Fatalf("composeAdCopy: %v", err)
		}
		if len(gotD) < minDescriptions {
			t.Errorf("descriptions = %v, want at least %d", gotD, minDescriptions)
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
		if _, err := buildAdFinalURL("", "slug", "Event", "Proj", "suffix"); err == nil {
			t.Error("expected an error for an empty registration URL")
		}
	})

	t.Run("invalid scheme is rejected", func(t *testing.T) {
		if _, err := buildAdFinalURL("ftp://example.com/register", "slug", "Event", "Proj", "suffix"); err == nil {
			t.Error("expected an error for a non-http(s) scheme")
		}
	})

	t.Run("no host is rejected", func(t *testing.T) {
		if _, err := buildAdFinalURL("https:///register", "slug", "Event", "Proj", "suffix"); err == nil {
			t.Error("expected an error for a URL with no host")
		}
	})

	t.Run("tags utm params using the event slug", func(t *testing.T) {
		got, err := buildAdFinalURL("https://example.com/register", "kubecon-na-2026", "KubeCon NA", "CNCF", "brief-1")
		if err != nil {
			t.Fatalf("buildAdFinalURL: %v", err)
		}
		u, _ := url.Parse(got)
		q := u.Query()
		if q.Get("utm_source") != "google" {
			t.Errorf("utm_source = %q, want google", q.Get("utm_source"))
		}
		if q.Get("utm_medium") != "cpc" {
			t.Errorf("utm_medium = %q, want cpc", q.Get("utm_medium"))
		}
		if q.Get("utm_campaign") != "kubecon-na-2026" {
			t.Errorf("utm_campaign = %q, want the event slug", q.Get("utm_campaign"))
		}
		if q.Get("utm_content") != "CNCF" {
			t.Errorf("utm_content = %q, want the project", q.Get("utm_content"))
		}
	})

	t.Run("falls back from slug to event name to name suffix", func(t *testing.T) {
		got, err := buildAdFinalURL("https://example.com/register", "", "KubeCon NA", "CNCF", "brief-1")
		if err != nil {
			t.Fatalf("buildAdFinalURL: %v", err)
		}
		u, _ := url.Parse(got)
		if u.Query().Get("utm_campaign") == "" {
			t.Error("utm_campaign should fall back to a sanitized event name")
		}

		got, err = buildAdFinalURL("https://example.com/register", "", "", "CNCF", "brief-1")
		if err != nil {
			t.Fatalf("buildAdFinalURL: %v", err)
		}
		u, _ = url.Parse(got)
		if u.Query().Get("utm_campaign") == "" {
			t.Error("utm_campaign should fall back to the name suffix when slug and event name are both empty")
		}
	})

	t.Run("preserves existing query params and does not overwrite an existing utm key", func(t *testing.T) {
		got, err := buildAdFinalURL("https://example.com/register?utm_source=newsletter&ref=abc", "slug", "Event", "Proj", "suffix")
		if err != nil {
			t.Fatalf("buildAdFinalURL: %v", err)
		}
		u, _ := url.Parse(got)
		q := u.Query()
		if q.Get("utm_source") != "newsletter" {
			t.Errorf("utm_source = %q, want the pre-existing value preserved", q.Get("utm_source"))
		}
		if q.Get("ref") != "abc" {
			t.Errorf("ref = %q, want the pre-existing param preserved", q.Get("ref"))
		}
		if q.Get("utm_medium") != "cpc" {
			t.Errorf("utm_medium = %q, want cpc added", q.Get("utm_medium"))
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
