// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package eventurl

import (
	"bytes"
	"encoding/json"
	"mime"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// Every extracted field comes from attacker-controlled markup and the body limit is
// 10MiB, so without these one <title> puts megabytes into a Postgres column and into
// every brief built from it. The description gets more room: it is legitimately longer
// and is the one field a human reads in full.
const (
	maxFieldBytes       = 1 << 10 // 1 KiB
	maxDescriptionBytes = 8 << 10 // 8 KiB
	// maxListEntries bounds the two LIST fields (speakers, sponsors) by count, which the
	// per-field byte bounds above cannot do. 64 is chosen to sit above any real event's
	// headline speaker or sponsor list while keeping the worst case a bounded multiple of
	// maxFieldBytes rather than a function of the page.
	maxListEntries = 64
)

// EventDetails holds parsed event metadata extracted from an event page.
//
// The JSON keys are the ones a brief's stored `event_details` blob already uses, NOT a
// spelling chosen for this struct. The endpoint that returns this tells callers to store
// the result with create-brief. `eventName` is the PREFERRED spelling: both consumers of
// that blob — internal/dispatch/reddit.go's briefFields and internal/service's
// briefEventDetails — read it first. Since LFXV2-3259 they also accept `name`, because
// that is what the UI writes and briefs stored that way were undispatchable; emitting
// `eventName` here keeps this producer on the unambiguous key rather than relying on the
// compatibility fallback. camelCase throughout matches
// that blob's existing keys (`registrationUrl`, `conversionPixelId`).
type EventDetails struct {
	Name        string `json:"eventName,omitempty"`
	Description string `json:"description,omitempty"`
	Location    string `json:"location,omitempty"`
	StartDate   string `json:"startDate,omitempty"`
	EndDate     string `json:"endDate,omitempty"`
	Image       string `json:"image,omitempty"`
	// URL is the event's own landing page AS THE PAGE DECLARES IT — JSON-LD `url` or
	// og:url — and NOT the URL that was fetched, which this package never sees a reason
	// to record. Callers commonly paste a link carrying tracking parameters; a declared
	// canonical is the better destination, and a caller wanting the fetched URL as a
	// fallback supplies it itself (see service.FetchEventURL).
	//
	// It is deliberately NOT emitted as
	// `registrationUrl`: the dispatchers treat that as the link an ad sends a human to,
	// and an event's landing page is often not its registration form. A caller that
	// wants the two to be the same says so explicitly.
	URL string `json:"url,omitempty"`

	// Speakers, Sponsors and RegistrationURL are the three fields the email-creation
	// wizard needs beyond the ones above, and they are populated from the JSON-LD tier
	// ONLY.
	//
	// That restriction is the point rather than an unfinished edge. The OpenGraph and
	// <title> tiers exist because almost every page carries og: tags or a title, so a
	// heuristic there is cheap and usually right. None of these three has any OpenGraph
	// equivalent: recovering a speaker list or a sponsor grid from arbitrary markup means
	// guessing at class names and heading text, and the failure mode is not an empty
	// field — it is a CONFIDENT wrong one, printed into a marketing email under a real
	// foundation's name. A page without JSON-LD simply leaves these empty, exactly as it
	// leaves every other field it does not declare, and the wizard's prompts are written
	// to work without them.
	//
	// They are `omitempty` and camelCase like the rest, so a brief's stored
	// `event_details` blob gains three keys when the page declares them and is byte-wise
	// unchanged when it does not.
	Speakers        []string     `json:"speakers,omitempty"`
	Sponsors        []SponsorRef `json:"sponsors,omitempty"`
	RegistrationURL string       `json:"registrationUrl,omitempty"`

	ExtractedFrom string `json:"extractedFrom,omitempty"` // "jsonld", "opengraph", or "fallback"
}

// SponsorRef is one sponsor named by an event page's JSON-LD.
//
// Every field is optional except Name: schema.org's `sponsor` is an Organization or
// Person, and the only property those two reliably share is a name. A sponsor with no
// name is not a sponsor anybody can render, so the extractor drops it rather than
// emitting a blank row.
type SponsorRef struct {
	Name string `json:"name"`
	Logo string `json:"logoUrl,omitempty"`
	URL  string `json:"url,omitempty"`
	// Tier is free text, taken from whatever the page calls the sponsorship level
	// ("Diamond", "tier1", "Community"). It is NOT normalised: there is no cross-event
	// vocabulary for sponsorship tiers, and mapping "Diamond" onto an invented scale
	// would assert a ranking the page never made.
	Tier string `json:"tier,omitempty"`
}

// sanitize makes s storable in a Postgres text column.
//
// Two distinct hazards, and clamping addresses NEITHER of them:
//
// Invalid UTF-8 does not have to be introduced by truncation — it can arrive that way.
// The fetched bytes are an arbitrary remote response, and `html.Parse` does not validate
// encoding: a page declaring UTF-8 while emitting Latin-1, or one deliberately serving
// malformed sequences, hands us a Go string that is not valid UTF-8 to begin with.
// Clamping cuts on a rune boundary, so it only avoids CREATING invalid UTF-8; it cannot
// notice what was already there. Postgres rejects the insert either way.
//
// A NUL byte is valid UTF-8, so ToValidUTF8 leaves it, and Postgres still refuses it in a
// text value ("unsupported Unicode escape sequence"). It gets its own pass.
//
// Both replacements happen BEFORE clamping, since either can change the byte length.
func sanitize(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if strings.IndexByte(s, 0) >= 0 {
		s = strings.ReplaceAll(s, "\x00", "")
	}
	return s
}

// clamp sanitizes s and truncates it to at most maxBytes, cutting on a RUNE boundary:
// a byte slice of UTF-8 can end mid-sequence, and Postgres rejects the invalid string on
// insert — turning an oversized field into a failed request rather than a short one.
func clamp(s string, maxBytes int) string {
	s = sanitize(s)
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

// clampFields bounds and sanitizes every field in one place, so a new extraction strategy
// cannot introduce an unbounded or unstorable one by forgetting to call it.
func (d *EventDetails) clampFields() {
	d.Name = clamp(d.Name, maxFieldBytes)
	d.Description = clamp(d.Description, maxDescriptionBytes)
	d.Location = clamp(d.Location, maxFieldBytes)
	d.StartDate = clamp(d.StartDate, maxFieldBytes)
	d.EndDate = clamp(d.EndDate, maxFieldBytes)
	d.Image = clamp(d.Image, maxFieldBytes)
	d.URL = clamp(d.URL, maxFieldBytes)
	d.RegistrationURL = clamp(d.RegistrationURL, maxFieldBytes)

	// The two LISTS need a count bound as well as a per-entry byte bound, which the
	// scalar fields did not. A single field can only be as long as the page makes one
	// value; a list is as long as the page makes it, and the whole struct is serialised
	// into a brief's event_details column, so an adversarial (or merely generated) page
	// declaring fifty thousand performers would be a row bounded by nothing. Cutting the
	// list is the right response rather than rejecting the page: the first entries are
	// still correct and useful, and no consumer treats these as exhaustive.
	if len(d.Speakers) > maxListEntries {
		d.Speakers = d.Speakers[:maxListEntries]
	}
	for i := range d.Speakers {
		d.Speakers[i] = clamp(d.Speakers[i], maxFieldBytes)
	}
	if len(d.Sponsors) > maxListEntries {
		d.Sponsors = d.Sponsors[:maxListEntries]
	}
	for i := range d.Sponsors {
		d.Sponsors[i].Name = clamp(d.Sponsors[i].Name, maxFieldBytes)
		d.Sponsors[i].Logo = clamp(d.Sponsors[i].Logo, maxFieldBytes)
		d.Sponsors[i].URL = clamp(d.Sponsors[i].URL, maxFieldBytes)
		d.Sponsors[i].Tier = clamp(d.Sponsors[i].Tier, maxFieldBytes)
	}
}

// Parser extracts event details from HTML using structured metadata.
type Parser struct{}

// NewParser constructs a Parser.
func NewParser() *Parser {
	return &Parser{}
}

// Parse extracts event details from an HTML body. It tries, in order:
//  1. JSON-LD schema.org/Event
//  2. OpenGraph meta tags
//  3. <title> plus meta[name=description]
//
// Returns an empty EventDetails if no usable metadata is found; the caller checks Name
// and answers ErrEventDetailsEmpty rather than letting an empty success through.
func (p *Parser) Parse(body []byte) EventDetails {
	// One parse, three passes. The tree is only read, so parsing per strategy paid
	// html.Parse — the most expensive step — three times per fetch on a body that may
	// be 10MiB. bytes.NewReader also avoids copying the body to a string.
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return EventDetails{}
	}

	// Each strategy fills a FRESH candidate, and only a candidate that wins is adopted.
	// Sharing one struct across the three would let a losing strategy's fields survive
	// into the winner's result: JSON-LD that yields a description but no name returns
	// false, OpenGraph then supplies the name, and the result is stamped
	// ExtractedFrom="opengraph" while carrying a description the OpenGraph tags never
	// contained. That is two pages' worth of metadata merged into one record with a
	// provenance label that is simply wrong — and ExtractedFrom exists precisely so a
	// human can judge how much to trust the rest. Provenance has to be all-or-nothing
	// to mean anything.
	for _, strategy := range []struct {
		name  string
		parse func(*html.Node, *EventDetails) bool
	}{
		{"jsonld", p.parseJSONLD},
		{"opengraph", p.parseOpenGraph},
	} {
		candidate := EventDetails{}
		if !strategy.parse(doc, &candidate) {
			continue
		}
		// Clamped BEFORE the name is judged usable, not after. sanitize strips NUL bytes
		// and replaces invalid UTF-8, so a name made only of those is non-empty here and
		// empty by the time it is returned: judging first meant a page whose first Event
		// carried such a name won the strategy outright, and the caller received an empty
		// record with ExtractedFrom="jsonld" while the page's OpenGraph title sat unread.
		// A field that cannot survive storage is not a field the page supplied.
		candidate.clampFields()
		if candidate.Name != "" {
			candidate.ExtractedFrom = strategy.name
			return candidate
		}
	}

	details := EventDetails{}
	p.parseFallback(doc, &details)
	details.clampFields()
	if details.Name != "" {
		details.ExtractedFrom = "fallback"
	}
	return details
}

// maxJSONLDNodes and maxJSONLDDepth bound one JSON-LD block's traversal. encoding/json's
// 10000-level nesting limit is not a usable bound here: it caps what DECODES, and a
// document that decodes fine can still describe far more graph nodes than any real page.
// A page emitting more than a few hundred nodes, or nesting `@graph` past a handful of
// levels, is not a page this parser is for.
//
// Neither constant bounds PEAK MEMORY, and it would be a mistake to read them that way:
// json.Unmarshal runs to completion and materializes the entire value before jsonLDNodes
// is handed anything, so by the time the node cap can apply, the allocation it might have
// prevented has already happened. What bounds that is upstream — the fetcher caps a
// response at maxResponseBytes (10 MiB) and REFUSES anything larger rather than parsing a
// truncated prefix, so the decoded value is bounded by what 10 MiB of JSON can describe.
// These two caps bound the TRAVERSAL that follows, which is a different cost.
//
// maxJSONLDScheduled is the third bound, and it is the one that makes the other two hold.
// maxJSONLDNodes counts what LANDS IN `out` — i.e. maps — so a document that is mostly not
// maps never trips it: a top-level array of scalars leaves `out` empty forever while every
// element is still queued and popped. 10 MiB of `[1,1,1,...]` is roughly five million such
// elements, several times that in bytes of frame, and the cap the comment above promises
// never fires once. Bounding SCHEDULED values instead bounds the traversal whatever the
// document is made of.
const (
	maxJSONLDNodes     = 512
	maxJSONLDDepth     = 16
	maxJSONLDScheduled = 4096
)

// jsonLDNodes flattens one JSON-LD document into the node objects worth inspecting.
// A top-level ARRAY is the common real-world shape — a page emits its Organization,
// BreadcrumbList and Event as one list — and unmarshalling into a map drops the whole
// block; `@graph` is the other standard wrapper.
//
// The traversal is ITERATIVE and explicitly bounded. The recursive form returned a fresh
// slice per frame and appended each child slice into its parent, so a nested `@graph`
// chain copied the accumulated result once per level — quadratic allocation driven by
// attacker-controlled nesting, on top of one goroutine stack frame per level. Here the
// stack is a heap slice, every node lands in one `out` that is never re-copied, and both
// the node count and the depth are capped: a malformed document yields the nodes found so
// far, which the caller treats exactly like any page whose Event it could not locate.
func jsonLDNodes(root interface{}) []map[string]interface{} {
	type frame struct {
		v     interface{}
		depth int
	}
	var out []map[string]interface{}
	stack := []frame{{v: root}}
	// budget is what remains of maxJSONLDScheduled. Every value put on the stack spends one,
	// including the root, so the stack can never hold more frames than the budget allowed.
	budget := maxJSONLDScheduled - 1
	for len(stack) > 0 && len(out) < maxJSONLDNodes {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if f.depth > maxJSONLDDepth {
			continue
		}
		switch t := f.v.(type) {
		case map[string]interface{}:
			out = append(out, t)
			if g, ok := t["@graph"]; ok && budget > 0 {
				budget--
				stack = append(stack, frame{v: g, depth: f.depth + 1})
			}
		case []interface{}:
			// The array is truncated to the budget BEFORE anything is pushed, and truncated
			// from the TAIL. Scheduling every element in one shot is what let a single large
			// array defeat maxJSONLDNodes (see the constant's comment); refusing mid-loop
			// instead would drop elements from the HEAD, because the push below runs in
			// reverse — and the head is exactly what parseJSONLD's "first named Event in
			// document order wins" rule depends on. Keeping the prefix keeps that rule
			// meaningful on a document too large to walk in full.
			n := len(t)
			if n > budget {
				n = budget
			}
			budget -= n
			// Pushed in reverse so the stack pops them in document order, which is what
			// makes parseJSONLD's "first named Event wins" rule deterministic.
			for i := n - 1; i >= 0; i-- {
				stack = append(stack, frame{v: t[i], depth: f.depth + 1})
			}
		}
	}
	return out
}

// jsonLDText resolves a JSON-LD property to a single string. schema.org lets the SAME
// property be a bare string, a node object, or an array of either — `location` is
// normally a Place and `image` an ImageObject — so a plain assertion to string drops
// the majority shape silently, and the field just comes back empty with nothing saying
// why. keys names the sub-properties to try on a node object, in preference order.
//
// The array case recurses, and the depth is bounded for the same reason jsonLDNodes
// bounds its traversal: both walk the SAME attacker-controlled document, and leaving one
// of them open is an asymmetry a reader has to explain rather than read.
//
// It is worth being exact about what the bound is NOT for, so nobody later "hardens" it
// against a threat it never faced. This was never a stack-overflow risk. encoding/json
// refuses to decode past 10000 levels of nesting ("exceeded max depth"), so an array
// arriving here is already capped at that, and 10000 frames of a function this small sit
// far inside a goroutine stack that grows to 1GB. The bound buys two smaller things: the
// traversal stops depending on an implementation detail of another package for its
// termination, and a value nested deeper than any real schema.org property costs a
// constant instead of a walk. A property nested past maxJSONLDDepth is treated as absent,
// which is what the caller does with every property it cannot resolve.
func jsonLDText(v interface{}, keys ...string) string {
	return jsonLDTextAt(v, 0, keys...)
}

func jsonLDTextAt(v interface{}, depth int, keys ...string) string {
	if depth > maxJSONLDDepth {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case map[string]interface{}:
		for _, k := range keys {
			// The sub-property is resolved RECURSIVELY rather than asserted to string.
			// schema.org applies the same string-or-node-or-array freedom at every level,
			// not only the top one: an ImageObject's `url` is usually a string but a
			// `name` may arrive as a one-element array. Asserting here would drop those
			// exactly as asserting at the top level dropped the node shape.
			if s := jsonLDTextAt(t[k], depth+1, keys...); s != "" {
				return s
			}
		}
	case []interface{}:
		for _, e := range t {
			if s := jsonLDTextAt(e, depth+1, keys...); s != "" {
				return s
			}
		}
	}
	return ""
}

// jsonLDLocation resolves schema.org `location` to a human-readable venue string.
//
// It is separate from jsonLDText because `location` is the one property whose fallback is
// not another string: a Place carries the venue in `name`, and when `name` is absent the
// fallback is `address`, which is normally a PostalAddress NODE. Passing "address" to
// jsonLDText's generic key list therefore resolved nothing for the exact shape the
// fallback exists to serve — the location came back empty and no error said why.
func jsonLDLocation(v interface{}) string {
	return jsonLDLocationAt(v, 0)
}

func jsonLDLocationAt(v interface{}, depth int) string {
	if depth > maxJSONLDDepth {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case []interface{}:
		for _, e := range t {
			if s := jsonLDLocationAt(e, depth+1); s != "" {
				return s
			}
		}
	case map[string]interface{}:
		if s := jsonLDTextAt(t["name"], depth+1, "name"); s != "" {
			return s
		}
		return jsonLDAddressAt(t["address"], depth+1)
	}
	return ""
}

// jsonLDAddressAt renders a PostalAddress. Its fields are JOINED rather than resolved
// first-wins: "San Francisco" alone is a materially worse answer than "San Francisco, CA,
// US" for a venue line, and unlike a name there is no single field that stands for the
// whole. Order is schema.org's own narrowest-to-widest.
func jsonLDAddressAt(v interface{}, depth int) string {
	if depth > maxJSONLDDepth {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case []interface{}:
		for _, e := range t {
			if s := jsonLDAddressAt(e, depth+1); s != "" {
				return s
			}
		}
	case map[string]interface{}:
		var parts []string
		for _, k := range []string{"streetAddress", "addressLocality", "addressRegion", "addressCountry"} {
			// addressCountry is frequently a Country node rather than a string, so each
			// component goes through the same string-or-node resolution as everything else.
			if s := jsonLDTextAt(t[k], depth+1, "name"); s != "" {
				parts = append(parts, s)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, ", ")
		}
		// A node that is not a PostalAddress at all still gets the ordinary name treatment.
		return jsonLDTextAt(t["name"], depth+1, "name")
	}
	return ""
}

// jsonLDNames resolves a property that may name SEVERAL entities to their names.
//
// It is the list counterpart of jsonLDText: `performer` is a Person, an Organization, an
// array of either, or a bare string, and jsonLDText's first-wins rule would return one
// speaker from a page declaring twelve. Entries whose name cannot be resolved are dropped
// rather than appended blank, so the length of the result means something.
//
// Collection stops at maxListEntries. clampFields would trim the slice anyway, but only
// AFTER it had been built: the cap belongs here too so that a page declaring fifty
// thousand performers costs a bounded walk and a bounded allocation rather than a large
// one that is then thrown away.
func jsonLDNames(v interface{}) []string {
	var out []string
	appendNamesAt(v, 0, &out)
	return out
}

func appendNamesAt(v interface{}, depth int, out *[]string) {
	if depth > maxJSONLDDepth || len(*out) >= maxListEntries {
		return
	}
	switch t := v.(type) {
	case string:
		if s := strings.TrimSpace(t); s != "" {
			*out = append(*out, s)
		}
	case map[string]interface{}:
		// "name" first, then the two halves a Person may carry instead — some generators
		// emit givenName/familyName with no name at all.
		if s := jsonLDTextAt(t["name"], depth+1, "name"); s != "" {
			*out = append(*out, s)
			return
		}
		given := jsonLDTextAt(t["givenName"], depth+1, "name")
		family := jsonLDTextAt(t["familyName"], depth+1, "name")
		if full := strings.TrimSpace(given + " " + family); full != "" {
			*out = append(*out, full)
		}
	case []interface{}:
		for _, e := range t {
			appendNamesAt(e, depth+1, out)
			if len(*out) >= maxListEntries {
				return
			}
		}
	}
}

// jsonLDSponsors resolves schema.org `sponsor` to the renderable sponsor rows.
//
// A sponsor is only kept when it has a NAME (see SponsorRef): logo-only entries cannot be
// captioned, attributed, or given alt text, and an email that shows an unlabelled image
// borrowed from a third party's CDN is worse than one that shows nothing. A bare string
// sponsor — the common minimal shape — becomes a name-only row.
//
// Tier has no schema.org property of its own, so the three spellings real pages use are
// tried in turn. A page that names none leaves Tier empty, which is honest; inventing one
// would assert a ranking the page never made.
func jsonLDSponsors(v interface{}) []SponsorRef {
	var out []SponsorRef
	appendSponsorsAt(v, 0, &out)
	return out
}

func appendSponsorsAt(v interface{}, depth int, out *[]SponsorRef) {
	if depth > maxJSONLDDepth || len(*out) >= maxListEntries {
		return
	}
	switch t := v.(type) {
	case string:
		if s := strings.TrimSpace(t); s != "" {
			*out = append(*out, SponsorRef{Name: s})
		}
	case map[string]interface{}:
		name := jsonLDTextAt(t["name"], depth+1, "name")
		if name == "" {
			return
		}
		s := SponsorRef{
			Name: name,
			Logo: jsonLDTextAt(t["logo"], depth+1, "url", "contentUrl"),
			URL:  jsonLDTextAt(t["url"], depth+1, "url"),
		}
		for _, k := range []string{"sponsorshipLevel", "tier", "level"} {
			if tier := jsonLDTextAt(t[k], depth+1, "name"); tier != "" {
				s.Tier = tier
				break
			}
		}
		*out = append(*out, s)
	case []interface{}:
		for _, e := range t {
			appendSponsorsAt(e, depth+1, out)
			if len(*out) >= maxListEntries {
				return
			}
		}
	}
}

// parseJSONLD extracts event data from <script type="application/ld+json"> blocks.
//
// ONE node supplies every field, and the node is the first NAMED Event in document order.
// The obvious alternative — let each Event node fill whatever the previous ones left empty —
// composes an event that exists on no part of the page: a page whose `@graph` lists Event A
// with only a name and Event B with only a description would yield A's name beside B's
// dates, and nothing downstream could tell that the two halves describe different events.
// A nameless Event node leaks its fields into the next named one the same way.
//
// This is the same all-or-nothing rule Parse applies BETWEEN strategies (JSON-LD vs
// OpenGraph vs fallback); applying it between strategies but not between nodes within one
// would leave the larger of the two mixing hazards open, since a single `@graph` routinely
// carries several events while a page rarely carries competing metadata blocks.
func (p *Parser) parseJSONLD(doc *html.Node, details *EventDetails) bool {
	var walk func(*html.Node) bool
	walk = func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "script" && isJSONLDScript(n) &&
			n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
			var doc interface{}
			if err := json.Unmarshal([]byte(n.FirstChild.Data), &doc); err == nil {
				for _, node := range jsonLDNodes(doc) {
					var candidate EventDetails
					if !p.extractFromJSONLD(node, &candidate) {
						continue
					}
					// A name is what makes a node usable: it is the one field the brief
					// cannot be built without, and a node lacking it cannot be the event
					// this page is about. Skipping it here (rather than merging it) is
					// what keeps its stray fields out of the node that IS the event.
					//
					// Clamped first, for the same reason Parse clamps before judging: a
					// name of NUL bytes alone is non-empty until sanitize runs, and would
					// otherwise win this loop and lock out the real Event node behind it.
					candidate.clampFields()
					if candidate.Name == "" {
						continue
					}
					*details = candidate
					return true
				}
			}
		}
		// Keep walking: a page carries several blocks and the Event is rarely first.
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if walk(c) {
				return true
			}
		}
		return false
	}
	return walk(doc)
}

func isJSONLDScript(n *html.Node) bool {
	for _, attr := range n.Attr {
		// The media type is case-insensitive (RFC 9110 §8.3.1) and is compared WITHOUT
		// its parameters. `type="application/ld+json"`
		// is a media type, and RFC 2045 lets one carry parameters — `application/ld+json;
		// profile="http://www.w3.org/ns/json-ld#compacted"` is the shape the JSON-LD spec
		// itself defines, and it is valid markup. Comparing the whole attribute skips such
		// a block and silently falls through to the weaker OpenGraph or <title> metadata,
		// which is a quality regression no error reports. mime.ParseMediaType also folds
		// case and trims surrounding whitespace, so it subsumes the previous handling; a
		// value it cannot parse is not a media type and is skipped.
		if attr.Key == "type" {
			mediaType, _, err := mime.ParseMediaType(attr.Val)
			if err != nil || mediaType != "application/ld+json" {
				continue
			}
			return true
		}
	}
	return false
}

// eventTypes is schema.org's Event plus its subtypes, keyed lower-case.
//
// This is an ALLOW-LIST rather than a substring test for "event" because the substring
// version claimed types that are not events and never were: EventVenue is a Place,
// EventReservation is a Reservation, and EventStatusType and EventAttendanceModeEnumeration
// are enumerations. That is not a theoretical mismatch — an `@graph` conventionally lists
// the venue before the event it hosts, so EventVenue was routinely the FIRST match, and the
// old first-wins merge then locked the event name to the venue's while the real Event node
// was still to come. The two defects compounded: either alone produced a wrong name.
//
// Omitting a real subtype costs a page that falls through to OpenGraph, which is a
// recoverable degradation; admitting a non-event costs a brief built from the wrong node.
var eventTypes = map[string]bool{
	"event": true,
	// Direct subtypes, schema.org v29.
	"businessevent": true, "childrensevent": true, "comedyevent": true,
	"courseinstance": true, "danceevent": true, "deliveryevent": true,
	"educationevent": true, "eventseries": true, "exhibitionevent": true,
	"festival": true, "foodevent": true, "hackathon": true,
	"literaryevent": true, "musicevent": true, "publicationevent": true,
	"saleevent": true, "screeningevent": true, "socialevent": true,
	"sportsevent": true, "theaterevent": true, "visualartsevent": true,
	// PublicationEvent's own subtypes.
	"broadcastevent": true, "ondemandevent": true,
}

// isEventType reports whether one @type token names an Event.
//
// The token is normalised first: schema.org types travel as a bare name ("Event"), a full
// IRI ("https://schema.org/Event"), or a compacted CURIE ("schema:Event") depending on
// which generator emitted the page, and all three mean the same type. Matching only the
// bare form would reject two shapes that are just as valid as the one we happen to expect.
func isEventType(s string) bool {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexAny(s, "/:#"); i >= 0 {
		s = s[i+1:]
	}
	return eventTypes[strings.ToLower(s)]
}

// extractFromJSONLD extracts event fields from one JSON-LD node object.
func (p *Parser) extractFromJSONLD(ld map[string]interface{}, details *EventDetails) bool {
	// @type may be a string or an array of them (a node can declare several types).
	var isEvent bool
	switch t := ld["@type"].(type) {
	case string:
		isEvent = isEventType(t)
	case []interface{}:
		for _, tv := range t {
			if s, ok := tv.(string); ok && isEventType(s) {
				isEvent = true
				break
			}
		}
	}
	if !isEvent {
		return false
	}

	setIfEmpty(&details.Name, jsonLDText(ld["name"]))
	setIfEmpty(&details.Description, jsonLDText(ld["description"]))
	setIfEmpty(&details.URL, jsonLDText(ld["url"]))
	setIfEmpty(&details.StartDate, jsonLDText(ld["startDate"]))
	setIfEmpty(&details.EndDate, jsonLDText(ld["endDate"]))
	// A Place carries the venue in name, with address (itself often a PostalAddress
	// node) as fallback; an ImageObject uses url, or contentUrl for a distribution.
	setIfEmpty(&details.Location, jsonLDLocation(ld["location"]))
	setIfEmpty(&details.Image, jsonLDText(ld["image"], "url", "contentUrl"))

	// Speakers come from `performer` ONLY, not from `organizer` as well. They are close
	// enough to conflate by accident and must not be: schema.org's organizer is whoever
	// PUTS ON the event — routinely a foundation, a sponsor, or a conference company — and
	// an email that introduces "The Linux Foundation" as a speaker is wrong in a way a
	// reader notices immediately. An event that declares no performer leaves the list
	// empty, which every consumer already handles. `performers` is accepted as well
	// because schema.org lists it as a supersededBy alias and real generators still emit
	// it.
	if speakers := jsonLDNames(ld["performer"]); len(speakers) > 0 {
		details.Speakers = speakers
	} else {
		details.Speakers = jsonLDNames(ld["performers"])
	}
	details.Sponsors = jsonLDSponsors(ld["sponsor"])
	// `offers` is an Offer (or a list of them) whose `url` is the page a human buys or
	// registers on. This is the only place RegistrationURL is ever set, and deliberately
	// NOT from the event's own `url`: EventDetails.URL documents why the landing page and
	// the registration form are different things, and defaulting one to the other here
	// would make that distinction silently untrue for every page with no offer.
	setIfEmpty(&details.RegistrationURL, jsonLDText(ld["offers"], "url"))

	return details.Name != ""
}

// setIfEmpty fills dst only when still empty, so the FIRST strategy to supply a field wins:
// JSON-LD is authored metadata and beats OpenGraph, which beats the <title> fallback.
// Within JSON-LD it no longer arbitrates between nodes — parseJSONLD commits to a single
// node before any field is written — so the only thing it orders here is that a node's own
// value cannot be overwritten by a weaker later strategy.
func setIfEmpty(dst *string, v string) {
	if *dst == "" && v != "" {
		*dst = v
	}
}

// parseOpenGraph extracts data from og: meta tags.
func (p *Parser) parseOpenGraph(doc *html.Node, details *EventDetails) bool {
	found := false
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "meta" {
			var property, content string
			for _, attr := range n.Attr {
				switch attr.Key {
				case "property":
					// x/net/html lowercases attribute NAMES but not values, and the
					// OpenGraph property is a value.
					property = strings.ToLower(strings.TrimSpace(attr.Val))
				case "content":
					content = attr.Val
				}
			}
			if content != "" {
				switch property {
				case "og:title":
					if details.Name == "" {
						details.Name = content
						found = true
					}
				case "og:description":
					setIfEmpty(&details.Description, content)
				case "og:image":
					setIfEmpty(&details.Image, content)
				case "og:url":
					setIfEmpty(&details.URL, content)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return found
}

// parseFallback extracts <title> and meta[name=description].
func (p *Parser) parseFallback(doc *html.Node, details *EventDetails) {
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "title":
				if n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
					setIfEmpty(&details.Name, strings.TrimSpace(n.FirstChild.Data))
				}
			case "meta":
				var name, content string
				for _, attr := range n.Attr {
					switch attr.Key {
					case "name":
						name = strings.ToLower(strings.TrimSpace(attr.Val))
					case "content":
						content = attr.Val
					}
				}
				if name == "description" {
					setIfEmpty(&details.Description, content)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
}
