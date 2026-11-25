// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"encoding/json"
	"testing"
)

func decodeContent(t *testing.T, doc string) emailContent {
	t.Helper()
	var ec emailContent
	if err := json.Unmarshal([]byte(doc), &ec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return ec
}

// TestLeadingHeaderImage pins which image counts as the header: the first image ABOVE the first
// rich-text block, in layout order, and none when text comes first.
func TestLeadingHeaderImage(t *testing.T) {
	layout := `"flexAreas":{"main":{"sections":[{"columns":[{"widgets":["logo"]}]},{"columns":[{"widgets":["body","photo"]}]}]}}`
	withLogo := decodeContent(t, `{"content":{"widgets":{
		"logo":{"body":{"img":{"src":"https://f.hubspot.example/logo.png","alt":"OpenSSF"},"link":"https://openssf.org"}},
		"body":{"body":{"html":"<p>October news</p>"}},
		"photo":{"body":{"img":{"src":"https://f.hubspot.example/photo.png"}}}},`+layout+`}}`)
	img, ok := withLogo.leadingHeaderImage()
	if !ok || img.Src != "https://f.hubspot.example/logo.png" || img.Alt != "OpenSSF" || img.Link != "https://openssf.org" {
		t.Fatalf("leadingHeaderImage = %+v, %v; want the logo above the body", img, ok)
	}

	textFirst := decodeContent(t, `{"content":{"widgets":{
		"logo":{"body":{"html":"<h1>Title</h1>"}},
		"body":{"body":{"html":"<p>x</p>"}},
		"photo":{"body":{"img":{"src":"https://f.hubspot.example/photo.png"}}}},`+layout+`}}`)
	if img, ok := textFirst.leadingHeaderImage(); ok {
		t.Fatalf("an image below the first text block is last issue's content, not a header: got %+v", img)
	}
}
