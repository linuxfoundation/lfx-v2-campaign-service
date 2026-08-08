// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package eventurl

import (
	"encoding/json"
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
// TestParseDoesNotMergeAcrossStrategies pins provenance as all-or-nothing.
//
// The page carries JSON-LD with a description but NO name, so the JSON-LD strategy loses,
// and OpenGraph tags that supply a name and a different description. With one shared
// EventDetails across the three strategies, the JSON-LD description survived into the
// OpenGraph result: the record then claimed ExtractedFrom="opengraph" while holding a
// description no OpenGraph tag on the page contains. ExtractedFrom exists so a human can
// judge how much to trust the rest of the record, so a label that is true of only some
// fields is worse than no label.
func TestParseDoesNotMergeAcrossStrategies(t *testing.T) {
	body := []byte(`<html><head>
<script type="application/ld+json">{"@type":"Event","description":"jsonld description"}</script>
<meta property="og:title" content="OG Event">
<meta property="og:description" content="og description">
</head><body></body></html>`)

	got := NewParser().Parse(body)

	if got.ExtractedFrom != "opengraph" {
		t.Fatalf("ExtractedFrom = %q, want opengraph", got.ExtractedFrom)
	}
	if got.Name != "OG Event" {
		t.Errorf("Name = %q, want OG Event", got.Name)
	}
	if got.Description != "og description" {
		t.Errorf("Description = %q, want the OpenGraph description only — a losing strategy's field leaked into the winner's result", got.Description)
	}
}

// TestParseJSONLDMediaTypeParameters: `type` is a media type, and RFC 2045 lets it carry
// parameters. `application/ld+json;profile=...` is the form the JSON-LD spec defines, so a
// whole-value comparison skips a perfectly valid block and falls through to weaker
// metadata — a silent quality regression, since nothing errors.
func TestParseJSONLDMediaTypeParameters(t *testing.T) {
	for _, typ := range []string{
		`application/ld+json`,
		`application/ld+json;profile="http://www.w3.org/ns/json-ld#compacted"`,
		`application/ld+json; charset=utf-8`,
		`  APPLICATION/LD+JSON  `,
	} {
		t.Run(typ, func(t *testing.T) {
			// The attribute is SINGLE-quoted: a media-type parameter value may itself be
			// a quoted string, and inside a double-quoted attribute that quote would
			// terminate the attribute rather than reach the parser.
			body := []byte(`<html><head><script type='` + typ +
				`'>{"@type":"Event","name":"Parameterised"}</script></head><body></body></html>`)
			got := NewParser().Parse(body)
			if got.ExtractedFrom != "jsonld" {
				t.Fatalf("ExtractedFrom = %q, want jsonld", got.ExtractedFrom)
			}
			if got.Name != "Parameterised" {
				t.Errorf("Name = %q, want Parameterised", got.Name)
			}
		})
	}
}

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

// TestJSONLDNodesIsBounded covers the two limits on graph traversal. The decoder's own
// 10000-level nesting limit bounds what DECODES, not how many nodes a decoded document
// describes, so a document that is perfectly well-formed can still ask this parser to
// walk far more of a graph than any real event page contains.
func TestJSONLDNodesIsBounded(t *testing.T) {
	// A nested @graph chain past the depth cap: the frames beyond it are dropped rather
	// than walked, and nothing recurses.
	deep := map[string]interface{}{"@type": "Organization"}
	for range maxJSONLDDepth * 4 {
		deep = map[string]interface{}{"@graph": deep}
	}
	if n := len(jsonLDNodes(deep)); n > maxJSONLDDepth+1 {
		t.Errorf("depth-capped traversal visited %d nodes, want at most %d", n, maxJSONLDDepth+1)
	}

	// A wide array past the node cap stops at the cap instead of materialising all of it.
	wide := make([]interface{}, maxJSONLDNodes*3)
	for i := range wide {
		wide[i] = map[string]interface{}{"@type": "Thing"}
	}
	if n := len(jsonLDNodes(wide)); n > maxJSONLDNodes {
		t.Errorf("node-capped traversal visited %d nodes, want at most %d", n, maxJSONLDNodes)
	}
}

// TestParseJSONLDGraphOrderIsDocumentOrder pins the iterative traversal's ordering. The
// stack pushes array elements in reverse precisely so they pop in document order; get
// that backwards and setIfEmpty's "first node wins" rule silently selects the LAST
// Event on a page that lists several.
func TestParseJSONLDGraphOrderIsDocumentOrder(t *testing.T) {
	body := []byte(`<html><head><script type="application/ld+json">
	{"@graph":[{"@type":"Event","name":"First"},{"@type":"Event","name":"Second"}]}
	</script></head><body></body></html>`)
	if got := NewParser().Parse(body); got.Name != "First" {
		t.Errorf("Name = %q, want %q — the graph was not walked in document order", got.Name, "First")
	}
}

// TestEventDetailsUsesStoredBlobKeys pins the JSON keys against the shape a brief's
// event_details blob already uses. The endpoint returning this tells callers to store the
// result, and the dispatchers reject a brief with no `eventName` — a rename here produces
// a result that stores cleanly and then fails campaign dispatch with nothing in between
// explaining why.
func TestEventDetailsUsesStoredBlobKeys(t *testing.T) {
	b, err := json.Marshal(EventDetails{Name: "KubeCon", Location: "Seoul", StartDate: "2026-08-10"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for k, want := range map[string]string{"eventName": "KubeCon", "location": "Seoul", "startDate": "2026-08-10"} {
		if got[k] != want {
			t.Errorf("key %q = %q, want %q (serialized as %s)", k, got[k], want, b)
		}
	}
}
