// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package eventurl

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestParse covers the three strategies, their precedence, and the shapes schema.org
// permits for the SAME property that a plain type assertion drops silently: a top-level
// array (a page emits its Organization and its Event as one list), an @graph wrapper, an
// ImageObject instead of a URL string, and an array of Place for location. Each of those
// is common enough that dropping it returns an empty field for a real event page.
func TestParse(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       EventDetails
	}{
		{"jsonld", `<html><head><script type="application/ld+json">
		 {"@context":"https://schema.org","@type":"Event","name":"KubeCon EU 2026",
		  "description":"Cloud Native Conference","startDate":"2026-04-13",
		  "endDate":"2026-04-16","location":{"name":"Amsterdam"},
		  "image":"https://e.com/i.jpg","url":"https://e.com/kubecon"}
		 </script></head></html>`,
			EventDetails{Name: "KubeCon EU 2026", Description: "Cloud Native Conference",
				Location: "Amsterdam", StartDate: "2026-04-13", EndDate: "2026-04-16",
				Image: "https://e.com/i.jpg", URL: "https://e.com/kubecon", ExtractedFrom: "jsonld"}},

		{"opengraph", `<html><head><meta property="og:title" content="Amazing Event">
		 <meta property="og:description" content="Join us"><meta property="og:image" content="https://e.com/i.jpg">
		 <meta property="og:url" content="https://e.com/e"></head></html>`,
			EventDetails{Name: "Amazing Event", Description: "Join us", Image: "https://e.com/i.jpg",
				URL: "https://e.com/e", ExtractedFrom: "opengraph"}},

		// x/net/html lowercases attribute NAMES but not values, and the OpenGraph
		// property is a value — "OG:TITLE" is not rare.
		{"opengraph uppercase property", `<html><head><meta property="OG:TITLE" content="Cased"></head></html>`,
			EventDetails{Name: "Cased", ExtractedFrom: "opengraph"}},

		{"fallback title", `<html><head><title>Simple Event</title>
		 <meta name="description" content="A simple page"></head></html>`,
			EventDetails{Name: "Simple Event", Description: "A simple page", ExtractedFrom: "fallback"}},

		{"nothing extractable", `<html><head></head><body></body></html>`, EventDetails{}},

		{"jsonld outranks opengraph", `<html><head><script type="application/ld+json">
		 {"@type":"Event","name":"JSON-LD Event"}</script>
		 <meta property="og:title" content="OpenGraph Event"></head></html>`,
			EventDetails{Name: "JSON-LD Event", ExtractedFrom: "jsonld"}},

		// Invalid JSON must not abort extraction — the page still has a usable title.
		{"invalid json falls through", `<html><head><script type="application/ld+json">{ nope }
		 </script><title>Fallback Title</title></head></html>`,
			EventDetails{Name: "Fallback Title", ExtractedFrom: "fallback"}},

		{"top-level array, ImageObject, location array", `<html><head>
		 <script type="application/LD+JSON">[{"@type":"Organization","name":"Org"},
		 {"@type":"Event","name":"Shaped Event",
		  "image":{"@type":"ImageObject","url":"https://e.com/i.jpg"},
		  "location":[{"@type":"Place","name":"Seoul"}]}]</script></head></html>`,
			EventDetails{Name: "Shaped Event", Location: "Seoul",
				Image: "https://e.com/i.jpg", ExtractedFrom: "jsonld"}},

		{"@graph wrapper, contentUrl, image array", `<html><head>
		 <script type="application/ld+json">{"@context":"https://schema.org","@graph":[
		 {"@type":"WebPage"},{"@type":"BusinessEvent","name":"Shaped Event",
		  "image":[{"@type":"ImageObject","contentUrl":"https://e.com/i.jpg"}],
		  "location":{"@type":"Place","name":"Seoul"}}]}</script></head></html>`,
			EventDetails{Name: "Shaped Event", Location: "Seoul",
				Image: "https://e.com/i.jpg", ExtractedFrom: "jsonld"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewParser().Parse([]byte(tc.body)); got != tc.want {
				t.Errorf("Parse() = %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// TestParseClampsFields pins the field bound. The body limit is 10MiB and every field
// is attacker-controlled, so without it one <title> lands megabytes in Postgres.
func TestParseClampsFields(t *testing.T) {
	// Multi-byte runes straddling the cut prove the truncation lands on a rune
	// boundary: an invalid UTF-8 tail is rejected on insert, turning an oversized
	// field into a FAILED request rather than a short one.
	d := NewParser().Parse([]byte(`<html><head><title>` + strings.Repeat("é", maxFieldBytes) +
		`</title><meta name="description" content="` +
		strings.Repeat("x", maxDescriptionBytes+10) + `"></head></html>`))

	if len(d.Name) > maxFieldBytes {
		t.Errorf("Name is %d bytes, want <= %d", len(d.Name), maxFieldBytes)
	}
	if !utf8.ValidString(d.Name) {
		t.Error("Name was cut mid-rune and is not valid UTF-8")
	}
	if len(d.Description) != maxDescriptionBytes {
		t.Errorf("Description is %d bytes, want %d", len(d.Description), maxDescriptionBytes)
	}
}

func TestParseMalformedHTML(t *testing.T) {
	// Not an assertion about the result — only that hostile markup cannot panic the
	// walk, since every input here is a page somebody else controls.
	_ = NewParser().Parse([]byte(`not valid html at all <><title>Title</title>stuff`))
}
