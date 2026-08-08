// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package eventurl

import (
	"encoding/json"
	"strings"

	"golang.org/x/net/html"
)

// EventDetails holds parsed event metadata extracted from an event page.
type EventDetails struct {
	Name          string `json:"name,omitempty"`
	Description   string `json:"description,omitempty"`
	Location      string `json:"location,omitempty"`
	StartDate     string `json:"start_date,omitempty"`
	EndDate       string `json:"end_date,omitempty"`
	Image         string `json:"image,omitempty"`
	URL           string `json:"url,omitempty"`
	ExtractedFrom string `json:"extracted_from,omitempty"` // "jsonld", "opengraph", or "fallback"
}

// Parser extracts event details from HTML using structured metadata.
type Parser struct{}

// NewParser constructs a Parser.
func NewParser() *Parser {
	return &Parser{}
}

// Parse extracts event details from HTML body. It tries, in order:
// 1. JSON-LD schema.org/Event
// 2. OpenGraph meta tags (og:title, og:description, og:image)
// 3. HTML title + meta description
//
// Returns an empty EventDetails if no usable metadata is found (the caller
// checks and returns ErrEventDetailsEmpty if name is empty after all attempts).
func (p *Parser) Parse(body []byte) EventDetails {
	details := EventDetails{}

	// Try JSON-LD first (most structured).
	if p.parseJSONLD(body, &details) {
		details.ExtractedFrom = "jsonld"
		if details.Name != "" {
			return details
		}
	}

	// Fall back to OpenGraph.
	if p.parseOpenGraph(body, &details) {
		details.ExtractedFrom = "opengraph"
		if details.Name != "" {
			return details
		}
	}

	// Final fallback to HTML title/description.
	p.parseFallback(body, &details)
	if details.Name != "" {
		details.ExtractedFrom = "fallback"
	}

	return details
}

// parseJSONLD extracts event data from JSON-LD <script type="application/ld+json"> tags.
// It looks for schema.org/Event types and extracts name, description, location, dates, and image.
func (p *Parser) parseJSONLD(body []byte, details *EventDetails) bool {
	// Find all <script type="application/ld+json"> tags.
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return false
	}

	found := false
	var parseNode func(*html.Node) bool
	parseNode = func(n *html.Node) bool {
		if n.Type == html.ElementNode && n.Data == "script" {
			isJSONLD := false
			for _, attr := range n.Attr {
				if attr.Key == "type" && attr.Val == "application/ld+json" {
					isJSONLD = true
					break
				}
			}
			if isJSONLD && n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
				// Parse the JSON content.
				var ld map[string]interface{}
				if err := json.Unmarshal([]byte(n.FirstChild.Data), &ld); err == nil {
					// Look for Event type (could be wrapped in @context).
					if p.extractFromJSONLD(ld, details) {
						found = true
					}
				}
			}
		}
		// Continue searching even after finding one (multiple JSON-LD blocks possible).
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if parseNode(c) {
				found = true
			}
		}
		return false
	}
	parseNode(doc)
	return found
}

// extractFromJSONLD extracts event fields from a parsed JSON-LD object.
func (p *Parser) extractFromJSONLD(ld map[string]interface{}, details *EventDetails) bool {
	// Check if this is an Event.
	typeVal := ld["@type"]
	var isEvent bool
	switch t := typeVal.(type) {
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

	// Extract known fields.
	if name, ok := ld["name"].(string); ok && name != "" {
		details.Name = name
	}
	if desc, ok := ld["description"].(string); ok && desc != "" {
		details.Description = desc
	}
	if url, ok := ld["url"].(string); ok && url != "" {
		details.URL = url
	}

	// Extract location (can be a string or an object).
	if locStr, ok := ld["location"].(string); ok && locStr != "" {
		details.Location = locStr
	} else if locObj, ok := ld["location"].(map[string]interface{}); ok {
		if name, ok := locObj["name"].(string); ok && name != "" {
			details.Location = name
		}
	}

	// Extract image (can be a string or an array).
	if imgStr, ok := ld["image"].(string); ok && imgStr != "" {
		details.Image = imgStr
	} else if imgs, ok := ld["image"].([]interface{}); ok && len(imgs) > 0 {
		if imgStr, ok := imgs[0].(string); ok && imgStr != "" {
			details.Image = imgStr
		}
	}

	// Extract start/end dates.
	if startDate, ok := ld["startDate"].(string); ok && startDate != "" {
		details.StartDate = startDate
	}
	if endDate, ok := ld["endDate"].(string); ok && endDate != "" {
		details.EndDate = endDate
	}

	return details.Name != ""
}

// parseOpenGraph extracts data from og: meta tags.
func (p *Parser) parseOpenGraph(body []byte, details *EventDetails) bool {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return false
	}

	found := false
	var traverse func(*html.Node)
	traverse = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "meta" {
			var property, content string
			for _, attr := range n.Attr {
				switch attr.Key {
				case "property":
					property = attr.Val
				case "content":
					content = attr.Val
				}
			}

			// Match og: tags.
			if strings.HasPrefix(property, "og:") && content != "" {
				switch property {
				case "og:title":
					if details.Name == "" {
						details.Name = content
						found = true
					}
				case "og:description":
					if details.Description == "" {
						details.Description = content
					}
				case "og:image":
					if details.Image == "" {
						details.Image = content
					}
				case "og:url":
					if details.URL == "" {
						details.URL = content
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			traverse(c)
		}
	}
	traverse(doc)
	return found
}

// parseFallback extracts title and meta description from HTML.
func (p *Parser) parseFallback(body []byte, details *EventDetails) {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return
	}

	var traverse func(*html.Node)
	traverse = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if n.Data == "title" && n.FirstChild != nil && n.FirstChild.Type == html.TextNode {
				if details.Name == "" {
					details.Name = strings.TrimSpace(n.FirstChild.Data)
				}
			} else if n.Data == "meta" {
				var name, content string
				for _, attr := range n.Attr {
					switch attr.Key {
					case "name":
						name = attr.Val
					case "content":
						content = attr.Val
					}
				}
				if name == "description" && content != "" && details.Description == "" {
					details.Description = content
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			traverse(c)
		}
	}
	traverse(doc)
}
