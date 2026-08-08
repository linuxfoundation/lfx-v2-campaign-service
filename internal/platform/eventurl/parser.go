// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package eventurl

import (
	"bytes"
	"encoding/json"
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
)

// EventDetails holds parsed event metadata extracted from an event page.
//
// The JSON keys are the ones a brief's stored `event_details` blob already uses, NOT a
// spelling chosen for this struct. The endpoint that returns this tells callers to store
// the result with create-brief, and the existing consumers of that blob read `eventName`
// specifically — internal/dispatch/reddit.go's briefFields and internal/service's
// briefEventDetails both reject a brief without it. Serializing the name as `name` would
// have produced a result that round-trips through create-brief and then fails campaign
// dispatch, with nothing between the two steps saying why. camelCase throughout matches
// that blob's existing keys (`registrationUrl`, `conversionPixelId`).
type EventDetails struct {
	Name        string `json:"eventName,omitempty"`
	Description string `json:"description,omitempty"`
	Location    string `json:"location,omitempty"`
	StartDate   string `json:"startDate,omitempty"`
	EndDate     string `json:"endDate,omitempty"`
	Image       string `json:"image,omitempty"`
	// URL is the page that was fetched. It is deliberately NOT emitted as
	// `registrationUrl`: the dispatchers treat that as the link an ad sends a human to,
	// and an event's landing page is often not its registration form. A caller that
	// wants the two to be the same says so explicitly.
	URL           string `json:"url,omitempty"`
	ExtractedFrom string `json:"extractedFrom,omitempty"` // "jsonld", "opengraph", or "fallback"
}

// clamp truncates s to at most maxBytes, cutting on a RUNE boundary: a byte slice of
// UTF-8 can end mid-sequence, and Postgres rejects the invalid string on insert —
// turning an oversized field into a failed request rather than a short one.
func clamp(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

// clampFields bounds every field in one place, so a new extraction strategy cannot
// introduce an unbounded one by forgetting to call it.
func (d *EventDetails) clampFields() {
	d.Name = clamp(d.Name, maxFieldBytes)
	d.Description = clamp(d.Description, maxDescriptionBytes)
	d.Location = clamp(d.Location, maxFieldBytes)
	d.StartDate = clamp(d.StartDate, maxFieldBytes)
	d.EndDate = clamp(d.EndDate, maxFieldBytes)
	d.Image = clamp(d.Image, maxFieldBytes)
	d.URL = clamp(d.URL, maxFieldBytes)
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
	details := EventDetails{}

	// One parse, three passes. The tree is only read, so parsing per strategy paid
	// html.Parse — the most expensive step — three times per fetch on a body that may
	// be 10MiB. bytes.NewReader also avoids copying the body to a string.
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return details
	}

	if p.parseJSONLD(doc, &details) && details.Name != "" {
		details.ExtractedFrom = "jsonld"
		details.clampFields()
		return details
	}

	if p.parseOpenGraph(doc, &details) && details.Name != "" {
		details.ExtractedFrom = "opengraph"
		details.clampFields()
		return details
	}

	p.parseFallback(doc, &details)
	if details.Name != "" {
		details.ExtractedFrom = "fallback"
	}
	details.clampFields()
	return details
}

// maxJSONLDNodes and maxJSONLDDepth bound one JSON-LD block's traversal. encoding/json's
// 10000-level nesting limit is not a usable bound here: it caps what DECODES, and a
// document that decodes fine can still describe far more graph nodes than any real page.
// A page emitting more than a few hundred nodes, or nesting `@graph` past a handful of
// levels, is not a page this parser is for.
const (
	maxJSONLDNodes = 512
	maxJSONLDDepth = 16
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
	for len(stack) > 0 && len(out) < maxJSONLDNodes {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if f.depth > maxJSONLDDepth {
			continue
		}
		switch t := f.v.(type) {
		case map[string]interface{}:
			out = append(out, t)
			if g, ok := t["@graph"]; ok {
				stack = append(stack, frame{v: g, depth: f.depth + 1})
			}
		case []interface{}:
			// Pushed in reverse so the stack pops them in document order, which is what
			// makes setIfEmpty's "first node to supply a field wins" rule deterministic.
			for i := len(t) - 1; i >= 0; i-- {
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
func jsonLDText(v interface{}, keys ...string) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]interface{}:
		for _, k := range keys {
			if s, ok := t[k].(string); ok && s != "" {
				return s
			}
		}
	case []interface{}:
		for _, e := range t {
			if s := jsonLDText(e, keys...); s != "" {
				return s
			}
		}
	}
	return ""
}

// parseJSONLD extracts event data from <script type="application/ld+json"> blocks.
func (p *Parser) parseJSONLD(doc *html.Node, details *EventDetails) bool {
	found := false
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "script" && isJSONLDScript(n) &&
			n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
			var doc interface{}
			if err := json.Unmarshal([]byte(n.FirstChild.Data), &doc); err == nil {
				for _, node := range jsonLDNodes(doc) {
					if p.extractFromJSONLD(node, details) {
						found = true
					}
				}
			}
		}
		// Keep walking: a page carries several blocks and the Event is rarely first.
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return found
}

func isJSONLDScript(n *html.Node) bool {
	for _, attr := range n.Attr {
		// The media type is case-insensitive (RFC 9110 §8.3.1).
		if attr.Key == "type" && strings.EqualFold(strings.TrimSpace(attr.Val), "application/ld+json") {
			return true
		}
	}
	return false
}

// extractFromJSONLD extracts event fields from one JSON-LD node object.
func (p *Parser) extractFromJSONLD(ld map[string]interface{}, details *EventDetails) bool {
	// @type may be a string or an array of them; "event" matches the subtypes too
	// (BusinessEvent, EducationEvent, …), which is what most conference pages emit.
	var isEvent bool
	switch t := ld["@type"].(type) {
	case string:
		isEvent = strings.Contains(strings.ToLower(t), "event")
	case []interface{}:
		for _, tv := range t {
			if s, ok := tv.(string); ok && strings.Contains(strings.ToLower(s), "event") {
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
	setIfEmpty(&details.Location, jsonLDText(ld["location"], "name", "address"))
	setIfEmpty(&details.Image, jsonLDText(ld["image"], "url", "contentUrl"))

	return details.Name != ""
}

// setIfEmpty fills dst only when still empty, so the FIRST node to supply a field wins
// and a later Organization block cannot overwrite the Event's own name.
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
