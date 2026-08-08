// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package eventurl

import (
	"testing"
)

func TestParserJSONLD(t *testing.T) {
	html := `<html><head>
<script type="application/ld+json">
{
  "@context": "https://schema.org",
  "@type": "Event",
  "name": "KubeCon EU 2026",
  "description": "Cloud Native Conference",
  "startDate": "2026-04-13",
  "endDate": "2026-04-16",
  "location": {"name": "Amsterdam"},
  "image": "https://example.com/image.jpg",
  "url": "https://example.com/kubecon"
}
</script>
</head></html>`

	p := NewParser()
	details := p.Parse([]byte(html))

	if details.Name != "KubeCon EU 2026" {
		t.Errorf("Name mismatch: got %q", details.Name)
	}
	if details.Description != "Cloud Native Conference" {
		t.Errorf("Description mismatch: got %q", details.Description)
	}
	if details.Location != "Amsterdam" {
		t.Errorf("Location mismatch: got %q", details.Location)
	}
	if details.StartDate != "2026-04-13" {
		t.Errorf("StartDate mismatch: got %q", details.StartDate)
	}
	if details.ExtractedFrom != "jsonld" {
		t.Errorf("ExtractedFrom should be jsonld, got %q", details.ExtractedFrom)
	}
}

func TestParserOpenGraph(t *testing.T) {
	html := `<html><head>
<meta property="og:title" content="Amazing Event 2026">
<meta property="og:description" content="Join us for an amazing event">
<meta property="og:image" content="https://example.com/og-image.jpg">
<meta property="og:url" content="https://example.com/event">
</head></html>`

	p := NewParser()
	details := p.Parse([]byte(html))

	if details.Name != "Amazing Event 2026" {
		t.Errorf("Name mismatch: got %q", details.Name)
	}
	if details.Description != "Join us for an amazing event" {
		t.Errorf("Description mismatch: got %q", details.Description)
	}
	if details.Image != "https://example.com/og-image.jpg" {
		t.Errorf("Image mismatch: got %q", details.Image)
	}
	if details.ExtractedFrom != "opengraph" {
		t.Errorf("ExtractedFrom should be opengraph, got %q", details.ExtractedFrom)
	}
}

func TestParserFallback(t *testing.T) {
	html := `<html><head>
<title>Simple Event Title</title>
<meta name="description" content="This is a simple event page">
</head></html>`

	p := NewParser()
	details := p.Parse([]byte(html))

	if details.Name != "Simple Event Title" {
		t.Errorf("Name mismatch: got %q", details.Name)
	}
	if details.Description != "This is a simple event page" {
		t.Errorf("Description mismatch: got %q", details.Description)
	}
	if details.ExtractedFrom != "fallback" {
		t.Errorf("ExtractedFrom should be fallback, got %q", details.ExtractedFrom)
	}
}

func TestParserEmptyHTML(t *testing.T) {
	html := `<html><head></head><body></body></html>`

	p := NewParser()
	details := p.Parse([]byte(html))

	if details.Name != "" {
		t.Errorf("Should extract nothing, got name: %q", details.Name)
	}
	if details.ExtractedFrom != "" {
		t.Errorf("ExtractedFrom should be empty for no data")
	}
}

func TestParserJSONLDPriority(t *testing.T) {
	// JSON-LD should take priority over OpenGraph.
	html := `<html><head>
<script type="application/ld+json">
{
  "@type": "Event",
  "name": "JSON-LD Event"
}
</script>
<meta property="og:title" content="OpenGraph Event">
</head></html>`

	p := NewParser()
	details := p.Parse([]byte(html))

	if details.Name != "JSON-LD Event" {
		t.Errorf("JSON-LD should take priority, got %q", details.Name)
	}
	if details.ExtractedFrom != "jsonld" {
		t.Errorf("Should be extracted from jsonld, got %q", details.ExtractedFrom)
	}
}

func TestParserInvalidJSON(t *testing.T) {
	html := `<html><head>
<script type="application/ld+json">
{ invalid json }
</script>
<title>Fallback Title</title>
</head></html>`

	p := NewParser()
	details := p.Parse([]byte(html))

	// Should fall back to title when JSON-LD is invalid.
	if details.Name != "Fallback Title" {
		t.Errorf("Should fall back to title, got %q", details.Name)
	}
}

func TestParserImageArray(t *testing.T) {
	html := `<html><head>
<script type="application/ld+json">
{
  "@type": "Event",
  "name": "Event with Images",
  "image": ["https://example.com/img1.jpg", "https://example.com/img2.jpg"]
}
</script>
</head></html>`

	p := NewParser()
	details := p.Parse([]byte(html))

	if details.Image != "https://example.com/img1.jpg" {
		t.Errorf("Should extract first image from array, got %q", details.Image)
	}
}

func TestParserLocationObject(t *testing.T) {
	html := `<html><head>
<script type="application/ld+json">
{
  "@type": "Event",
  "name": "Event in Paris",
  "location": {
    "@type": "Place",
    "name": "Paris, France"
  }
}
</script>
</head></html>`

	p := NewParser()
	details := p.Parse([]byte(html))

	if details.Location != "Paris, France" {
		t.Errorf("Should extract location name from object, got %q", details.Location)
	}
}

func TestParserMalformedHTML(t *testing.T) {
	html := `not valid html at all <><title>Title</title>stuff`

	p := NewParser()
	// Should not crash, might extract title if possible.
	details := p.Parse([]byte(html))
	_ = details // Just verify no panic.
}
