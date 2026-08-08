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

// TestJSONLDTextIsDepthBounded pins the array recursion's cap.
//
// The assertion is on the RESULT, not on any observable of the walk, because the cap has
// no other effect: a value nested past it is reported absent, exactly like a property the
// parser could not resolve for any other reason. The shallow case is in the same test on
// purpose — a cap that also swallowed ordinary one-level arrays would satisfy the deep
// assertion perfectly, and `location` arriving as a single-element array is the common
// real-world shape, not an edge case.
func TestJSONLDTextIsDepthBounded(t *testing.T) {
	deep := interface{}("Moscone Center")
	for range maxJSONLDDepth + 2 {
		deep = []interface{}{deep}
	}
	if got := jsonLDText(deep, "name"); got != "" {
		t.Errorf("value nested past the cap resolved to %q, want %q", got, "")
	}

	shallow := interface{}([]interface{}{map[string]interface{}{"name": "Moscone Center"}})
	if got := jsonLDText(shallow, "name"); got != "Moscone Center" {
		t.Errorf("ordinary single-element array resolved to %q, want %q", got, "Moscone Center")
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

// TestJSONLDTypeIsAllowListed pins that @type is matched against schema.org's Event
// hierarchy and not by substring. A substring test for "event" claims EventVenue (a
// Place), EventReservation (a Reservation) and the Event*Type enumerations, and an
// `@graph` conventionally lists the venue BEFORE the event it hosts — so the wrong node
// was routinely the first one matched.
func TestJSONLDTypeIsAllowListed(t *testing.T) {
	notEvents := []string{"EventVenue", "EventReservation", "EventStatusType", "EventAttendanceModeEnumeration"}
	for _, typ := range notEvents {
		t.Run(typ, func(t *testing.T) {
			page := `<html><head><title>Real Title</title><script type="application/ld+json">` +
				`{"@type":"` + typ + `","name":"Moscone Center"}` +
				`</script></head><body></body></html>`
			got := NewParser().Parse([]byte(page))
			if got.Name == "Moscone Center" {
				t.Errorf("@type %q was treated as an Event; name came from the venue node", typ)
			}
			if got.ExtractedFrom != "fallback" {
				t.Errorf("ExtractedFrom = %q, want fallback (no Event node on the page)", got.ExtractedFrom)
			}
		})
	}

	// The subtypes and the IRI/CURIE spellings must still be admitted — over-rejecting
	// is a silent quality regression in the other direction.
	events := []string{"Event", "BusinessEvent", "EducationEvent", "Hackathon",
		"https://schema.org/Event", "schema:MusicEvent"}
	for _, typ := range events {
		t.Run(typ, func(t *testing.T) {
			page := `<html><head><script type="application/ld+json">` +
				`{"@type":"` + typ + `","name":"KubeCon"}` +
				`</script></head><body></body></html>`
			if got := NewParser().Parse([]byte(page)); got.Name != "KubeCon" {
				t.Errorf("@type %q was rejected: Name = %q, want KubeCon", typ, got.Name)
			}
		})
	}
}

// TestJSONLDLocationResolvesPostalAddress pins the documented fallback. `location` is a
// Place whose venue is in `name`, and when `name` is absent the fallback is `address` —
// which schema.org emits as a PostalAddress NODE, not a string. Reading only string
// sub-keys made that fallback resolve to empty for the exact shape it exists to serve.
func TestJSONLDLocationResolvesPostalAddress(t *testing.T) {
	page := `<html><head><script type="application/ld+json">{"@type":"Event","name":"KubeCon",
		"location":{"@type":"Place","address":{"@type":"PostalAddress",
		"streetAddress":"747 Howard St","addressLocality":"San Francisco",
		"addressRegion":"CA","addressCountry":{"@type":"Country","name":"US"}}}}
		</script></head><body></body></html>`
	got := NewParser().Parse([]byte(page))
	want := "747 Howard St, San Francisco, CA, US"
	if got.Location != want {
		t.Errorf("Location = %q, want %q", got.Location, want)
	}
}

// TestJSONLDDoesNotMergeDistinctEvents pins that ONE node supplies every field. Merging
// per-field across nodes composes an event that exists on no part of the page: the name
// of one event beside the dates of another, with nothing downstream able to tell.
func TestJSONLDDoesNotMergeDistinctEvents(t *testing.T) {
	page := `<html><head><script type="application/ld+json">[
		{"@type":"Event","name":"KubeCon EU","startDate":"2026-03-01"},
		{"@type":"Event","description":"A different conference","endDate":"2026-09-30"}
	]</script></head><body></body></html>`
	got := NewParser().Parse([]byte(page))

	if got.Name != "KubeCon EU" || got.StartDate != "2026-03-01" {
		t.Fatalf("first Event not selected: Name=%q StartDate=%q", got.Name, got.StartDate)
	}
	if got.Description != "" {
		t.Errorf("Description = %q, want empty: it belongs to the SECOND Event node", got.Description)
	}
	if got.EndDate != "" {
		t.Errorf("EndDate = %q, want empty: it belongs to the SECOND Event node", got.EndDate)
	}
}

// TestClampSanitizesInvalidUTF8 pins that invalid UTF-8 arriving IN the fetched bytes is
// replaced, not merely left alone by a truncation that happens to cut cleanly. html.Parse
// does not validate encoding, so a page declaring UTF-8 while emitting Latin-1 hands us a
// string Postgres will refuse. A NUL is valid UTF-8 and refused too, so it is stripped.
func TestClampSanitizesInvalidUTF8(t *testing.T) {
	page := append([]byte("<html><head><title>Caf"), 0xE9) // lone Latin-1 é
	page = append(page, []byte(" Event</title></head></html>")...)

	got := NewParser().Parse(page)
	if !utf8.ValidString(got.Name) {
		t.Errorf("Name = %q is not valid UTF-8", got.Name)
	}
	if !strings.Contains(got.Name, "Caf") || !strings.Contains(got.Name, " Event") {
		t.Errorf("Name = %q lost the surrounding valid text", got.Name)
	}

	// The NUL pass needs its own input. html.Parse already replaces a raw NUL with U+FFFD
	// per the HTML spec, so the markup path never reaches it. JSON-LD does: encoding/json
	// decodes the six-character escape below into a real NUL, which Postgres then refuses.
	jsonLD := `<html><head><script type="application/ld+json">` +
		`{"@type":"Event","name":"Kube\u0000Con"}</script></head><body></body></html>`
	if n := NewParser().Parse([]byte(jsonLD)).Name; strings.ContainsRune(n, 0) {
		t.Errorf("Name = %q still carries a NUL byte", n)
	} else if n != "KubeCon" {
		t.Errorf("Name = %q, want KubeCon", n)
	}
}

// TestJSONLDNodesBoundsScheduledValuesNotJustNodes pins the bound that maxJSONLDNodes cannot
// provide on its own.
//
// maxJSONLDNodes counts what lands in `out`, and only maps land there. A document made mostly
// of non-maps therefore never trips it: the walk below queues every element of a top-level
// array before the loop can re-check the node cap even once, so `out` stays empty while the
// stack grows with the array. 10 MiB of `[1,1,1,...]` -- comfortably inside the fetcher's body
// cap -- is millions of frames, and the comment on jsonLDNodes claims a bound that would not
// hold. maxJSONLDScheduled bounds the traversal by what it SCHEDULES, which is a property of
// the walk rather than of the document's shape.
//
// The trailing Event is the assertion's whole point: it sits past the budget, so it must NOT
// be reached. Losing the tail of an oversized array is the deliberate trade -- see the
// truncation comment in jsonLDNodes for why the tail and not the head.
func TestJSONLDNodesBoundsScheduledValuesNotJustNodes(t *testing.T) {
	root := make([]interface{}, 0, maxJSONLDScheduled+1)
	for i := 0; i < maxJSONLDScheduled; i++ {
		root = append(root, float64(i))
	}
	root = append(root, map[string]interface{}{"@type": "Event", "name": "Past the budget"})

	if got := jsonLDNodes(root); len(got) != 0 {
		t.Errorf("jsonLDNodes returned %d nodes, want 0: every scalar ahead of the trailing "+
			"Event was scheduled, which means the traversal walked the whole array and "+
			"maxJSONLDNodes never bounded it", len(got))
	}

	// The retained prefix keeps document order, which is what parseJSONLD's "first named
	// Event wins" rule rests on. A budget that dropped from the head would silently change
	// which event a large page resolves to.
	head := []interface{}{
		map[string]interface{}{"@type": "Event", "name": "First"},
		map[string]interface{}{"@type": "Event", "name": "Second"},
	}
	nodes := jsonLDNodes(head)
	if len(nodes) != 2 || nodes[0]["name"] != "First" {
		t.Errorf("nodes = %v, want First then Second in document order", nodes)
	}
}

// TestParseJSONLDNameIsJudgedAfterSanitizing pins that a name which cannot survive storage is
// not treated as a name.
//
// sanitize strips NUL bytes, so a name of nothing but NULs is non-empty when the node is
// selected and empty by the time the record is returned. Judging usability before that ran let
// the poisoned node win its strategy outright: parseJSONLD committed to it, Parse stamped
// ExtractedFrom="jsonld", and the caller got an empty record -- with the page's second, valid
// Event node and its OpenGraph title both sitting unread. An attacker-controlled page could
// therefore make this service report "no event details here" about a page that plainly has
// them, which is the fail-closed shape that matters: the caller acts on the absence.
func TestParseJSONLDNameIsJudgedAfterSanitizing(t *testing.T) {
	page := `<html><head>` +
		`<script type="application/ld+json">{"@type":"Event","name":"\u0000\u0000"}</script>` +
		`<script type="application/ld+json">{"@type":"Event","name":"KubeCon EU"}</script>` +
		`<meta property="og:title" content="OpenGraph fallback">` +
		`</head><body></body></html>`

	got := NewParser().Parse([]byte(page))
	if got.Name != "KubeCon EU" {
		t.Errorf("Name = %q, want %q -- a name of NUL bytes alone is empty once stored, so it "+
			"cannot be what makes a node the event this page is about", got.Name, "KubeCon EU")
	}
	if got.ExtractedFrom != "jsonld" {
		t.Errorf("ExtractedFrom = %q, want jsonld", got.ExtractedFrom)
	}
}
