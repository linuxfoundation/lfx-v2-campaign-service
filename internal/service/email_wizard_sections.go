// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"unicode/utf8"
)

// The wizard's email body is a LIST OF BLOCKS, not a blob of HTML, and this file is the one
// place that turns the blocks back into HTML.
//
// It is deterministic and makes no model call. That is the whole point of the
// update-sections endpoint: after the model has written the copy, an operator reorders and
// edits blocks in the UI, and pressing "preview" must produce the same HTML every time for
// the same blocks — a second model call would quietly rewrite prose the operator had already
// accepted. It also means the editing half of the wizard keeps working with the AI proxy
// unconfigured.
//
// The block vocabulary (rich_text / button / image / image_row / divider / social_icons) and
// its field names are fixed by the Angular wizard that already renders and edits them; they
// are wire contract, not a choice made here.

// wizardSection is one content block.
//
// Decoded from `any` rather than declared as a typed union because the wire shape is a
// heterogeneous per-type object and the UI round-trips blocks it may know more about than this
// service does. An unknown field must survive the round trip, so the decode is by field name
// into this all-optional struct and the ORIGINAL value is what gets persisted.
type wizardSection struct {
	Type string `json:"type"`

	// rich_text
	HTML string `json:"html,omitempty"`

	// button
	Text            string `json:"text,omitempty"`
	URL             string `json:"url,omitempty"`
	Destination     string `json:"destination,omitempty"`
	Color           string `json:"color,omitempty"`
	BackgroundColor string `json:"background_color,omitempty"`

	// image
	Src string `json:"src,omitempty"`
	Alt string `json:"alt,omitempty"`

	// image_row
	Images []wizardSectionImage `json:"images,omitempty"`

	// divider
	Style  string `json:"style,omitempty"`
	Height int    `json:"height,omitempty"`

	// social_icons
	Networks []string `json:"networks,omitempty"`
}

// wizardSectionImage is one logo inside an image_row block.
type wizardSectionImage struct {
	Src string `json:"src"`
	Alt string `json:"alt,omitempty"`
}

const (
	// maxWizardSections bounds how many blocks one email may carry.
	//
	// The bound is on the RENDERER, not only on the endpoint, because the same function
	// renders model output, and a model that emits a thousand one-line blocks would otherwise
	// build a megabyte of HTML inside a request. Generous enough that no real email reaches
	// it: the longest template in the portal is around a dozen blocks.
	maxWizardSections = 120
	// maxWizardSectionHTML bounds ONE rich_text block's HTML, in runes. Exceeding it drops
	// that block rather than truncating it: half an HTML fragment is unbalanced markup, and an
	// email with a missing paragraph is recoverable where one with a dangling <div> is not.
	maxWizardSectionHTML = 20000
	// maxWizardBodyHTML bounds the ASSEMBLED body. Blocks that would push the body past it are
	// dropped, for the same reason as above.
	maxWizardBodyHTML = 200000
)

// decodeWizardSections converts the wire's []any into typed blocks.
//
// Blocks that are not objects, or carry no type, are DROPPED rather than erroring the request.
// The list comes from a UI that round-trips whatever the model produced, so one malformed
// entry must not cost an operator the other twenty they just edited; the count of dropped
// blocks is reported so the caller can log it.
func decodeWizardSections(in []any) (out []wizardSection, dropped int) {
	if len(in) > maxWizardSections {
		dropped += len(in) - maxWizardSections
		in = in[:maxWizardSections]
	}
	for _, raw := range in {
		blob, err := json.Marshal(raw)
		if err != nil {
			dropped++
			continue
		}
		var sec wizardSection
		if err := json.Unmarshal(blob, &sec); err != nil {
			dropped++
			continue
		}
		if strings.TrimSpace(sec.Type) == "" {
			dropped++
			continue
		}
		out = append(out, sec)
	}
	return out, dropped
}

// renderWizardSections assembles the BODY-ONLY HTML — what goes into the HubSpot draft's
// first rich-text block, with no <html>/<head> wrapper.
//
// Every value the blocks carry is escaped EXCEPT a rich_text block's html, which is markup by
// definition and is the one field the UI's rich-text editor owns. Escaping it would show
// operators their own tags as literal text; the bound above is what keeps it from being
// unbounded. Hrefs and image sources go through httpURL, so a `javascript:` or `data:` URL
// supplied by a model or a scraped page cannot reach an anchor.
func renderWizardSections(sections []wizardSection) string {
	var b strings.Builder
	for _, sec := range sections {
		before := b.Len()
		switch sec.Type {
		case "rich_text":
			if utf8.RuneCountInString(sec.HTML) > maxWizardSectionHTML {
				continue
			}
			b.WriteString(`<div class="lfx-block lfx-rich-text">` + sec.HTML + "</div>\n")
		case "button":
			// destination is the tagged/resolved target and url the pre-tagging one; a block
			// that carries both means the same button, so the resolved one wins.
			href := httpURL(firstNonEmpty(sec.Destination, sec.URL))
			label := html.EscapeString(strings.TrimSpace(sec.Text))
			if label == "" {
				continue
			}
			if href == "" {
				// A button with nowhere to go is rendered as TEXT, never as href="#": an
				// anchor that goes nowhere reads as a broken email to a recipient, while the
				// same words as a line of copy read as intentional.
				b.WriteString(`<div class="lfx-block lfx-button"><strong>` + label + "</strong></div>\n")
				break
			}
			b.WriteString(`<div class="lfx-block lfx-button"><a href="` + html.EscapeString(href) + `"` +
				buttonStyleAttr(sec) + ">" + label + "</a></div>\n")
		case "image":
			src := httpURL(sec.Src)
			if src == "" {
				continue
			}
			b.WriteString(`<div class="lfx-block lfx-image"><img src="` + html.EscapeString(src) +
				`" alt="` + html.EscapeString(sec.Alt) + `"></div>` + "\n")
		case "image_row":
			row := renderWizardImageRow(sec.Images)
			if row == "" {
				continue
			}
			b.WriteString(row)
		case "divider":
			b.WriteString(`<hr class="lfx-block lfx-divider">` + "\n")
		case "social_icons":
			row := renderWizardSocialIcons(sec.Networks)
			if row == "" {
				continue
			}
			b.WriteString(row)
		default:
			// An unknown block type is skipped, not guessed at. The UI may know block types
			// this service does not, and rendering one as a generic <div> would put an
			// operator's structured content into the email as unstyled text.
			continue
		}
		if utf8.RuneCountInString(b.String()) > maxWizardBodyHTML {
			// Undo the block that crossed the bound and stop: a partially written block is
			// unbalanced markup.
			s := b.String()[:before]
			b.Reset()
			b.WriteString(s)
			break
		}
	}
	return b.String()
}

// buttonStyleAttr renders the two colour fields a button block may carry, and nothing else.
// Colours are escaped and length-bounded rather than parsed: the values come from the UI's own
// colour picker, and an inline style attribute cannot execute script, but an unbounded value
// could carry an entire stylesheet into every recipient's inbox.
func buttonStyleAttr(sec wizardSection) string {
	var decls []string
	if c := boundedColor(sec.BackgroundColor); c != "" {
		decls = append(decls, "background-color:"+c)
	}
	if c := boundedColor(sec.Color); c != "" {
		decls = append(decls, "color:"+c)
	}
	if len(decls) == 0 {
		return ""
	}
	return ` style="` + html.EscapeString(strings.Join(decls, ";")) + `"`
}

// boundedColor accepts a short colour token and rejects anything else. Semicolons and braces
// are refused outright so a value cannot close the declaration it sits in and open another.
func boundedColor(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 32 || strings.ContainsAny(v, ";{}()\"'<>") {
		return ""
	}
	return v
}

func renderWizardImageRow(images []wizardSectionImage) string {
	var cells []string
	for _, img := range images {
		src := httpURL(img.Src)
		if src == "" {
			continue
		}
		cells = append(cells, `<img src="`+html.EscapeString(src)+`" alt="`+html.EscapeString(img.Alt)+`">`)
	}
	if len(cells) == 0 {
		return ""
	}
	return `<div class="lfx-block lfx-image-row">` + strings.Join(cells, " ") + "</div>\n"
}

// renderWizardSocialIcons renders the NAMES of the networks, not icons.
//
// The source wizard resolved each name to a hosted icon asset; this service has no asset
// host, and linking to a third-party CDN from every recipient's inbox is a decision for
// whoever owns the template, not a default to invent here. Names render as a readable line and
// an operator who wants icons places the template's own social module.
func renderWizardSocialIcons(networks []string) string {
	var names []string
	for _, n := range networks {
		n = strings.TrimSpace(n)
		if n == "" || len(n) > 40 {
			continue
		}
		names = append(names, html.EscapeString(n))
	}
	if len(names) == 0 {
		return ""
	}
	return `<div class="lfx-block lfx-social">` + strings.Join(names, " &middot; ") + "</div>\n"
}

// wizardPreviewHTML wraps the body in a minimal document for the UI's preview pane.
//
// This is NOT what is written to HubSpot — the draft keeps the template's own document and
// receives only the body (see hubspot.ApplyEmailContent). Sending this wrapper upstream would
// replace an operator's tested template chrome with a bare white page.
func wizardPreviewHTML(subject, previewText, bodyHTML string) string {
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
</head>
<body style="margin:0;padding:24px;background:#f5f6f8;font-family:Arial,Helvetica,sans-serif;color:#1b1b1f;">
<div style="display:none;max-height:0;overflow:hidden;">%s</div>
<div style="max-width:640px;margin:0 auto;background:#ffffff;padding:24px;">
%s</div>
</body>
</html>
`, html.EscapeString(subject), html.EscapeString(previewText), bodyHTML)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
