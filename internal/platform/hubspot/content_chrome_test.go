// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// newsletterDraft is a brand newsletter as the LF portal lays one out: a logo, last issue's
// content (text, an image, a button), then the brand's own footer run.
func newsletterDraft() map[string]any {
	section := func(id string, style map[string]any, widgets ...string) map[string]any {
		return map[string]any{"id": id, "style": style, "columns": []any{map[string]any{"id": id + "-col", "width": 12, "widgets": widgets}}}
	}
	white := map[string]any{"backgroundType": "CONTENT", "paddingTop": "0px"}
	lilac := map[string]any{"backgroundColor": "#EEEDF6", "backgroundType": "CONTENT"}
	dark := map[string]any{"backgroundColor": "#222222", "backgroundType": "CONTENT"}
	return map[string]any{
		"templatePath":  "@hubspot/email/dnd/Start_from_scratch.html",
		"styleSettings": map[string]any{"primaryFont": "Georgia", "linksFont": map[string]any{"color": "#351C75"}},
		"widgets": map[string]any{
			"logo":      map[string]any{"body": map[string]any{"module_id": 1367093, "img": map[string]any{"src": "https://f.example/acme-logo.png", "alt": "Acme Foundation"}, "link": "https://acme.example/"}},
			"old_intro": map[string]any{"body": map[string]any{"path": "@hubspot/rich_text", "module_id": 1155639, "css_class": "dnd-module", "hs_wrapper_css": map[string]any{"padding-left": "30px"}, "html": "<h2>October news</h2><p>Stale item</p>"}},
			"old_image": map[string]any{"body": map[string]any{"module_id": 1367093, "img": map[string]any{"src": "https://f.example/october.png"}}},
			"old_button": map[string]any{"body": map[string]any{"path": "@hubspot/button_email", "module_id": 1976948, "background_color": "#003778",
				"text": "Read the October issue", "destination": "https://acme.example/october"}},
			"follow_title": map[string]any{"body": map[string]any{"module_id": 1155639, "html": "<p>Follow Us</p><p>Follow Acme on social media for the latest news.</p>"}},
			"social": map[string]any{"body": map[string]any{"path": "@hubspot/follow_me_email", "module_id": 2763545,
				"social": []any{map[string]any{"network": "linkedin", "url": "https://www.linkedin.com/company/acme-example/"}}}},
			"rule":         map[string]any{"body": map[string]any{"path": "@hubspot/email_divider", "module_id": 2191110, "line_type": "solid"}},
			"sent_by":      map[string]any{"body": map[string]any{"path": "@hubspot/rich_text", "module_id": 1155639, "html": "<h2>This email was sent by: Acme Foundation</h2>"}},
			"footer":       map[string]any{"body": map[string]any{"path": "/email_footer no links", "module_id": 40514205881}},
			"preview_text": map[string]any{"body": map[string]any{"value": "October preview"}},
		},
		"flexAreas": map[string]any{"main": map[string]any{
			"boxed": false,
			"sections": []any{
				section("s-logo", white, "logo"),
				section("s-intro", lilac, "old_intro", "old_button"),
				section("s-image", white, "old_image"),
				section("s-follow-title", dark, "follow_title"),
				section("s-social", dark, "social"),
				section("s-rule", white, "rule"),
				section("s-sent-by", dark, "sent_by", "footer"),
			},
		}},
	}
}

// savedDraft reads the draft back through the test server: its widget map and the widget keys
// its layout places, in order.
func savedDraft(t *testing.T, c *Client) (map[string]map[string]any, []string, rawEmailContent) {
	t.Helper()
	doc, err := c.readDraftContent(context.Background(), "email-1")
	if err != nil {
		t.Fatalf("readDraftContent: %v", err)
	}
	var widgets map[string]map[string]any
	if err := json.Unmarshal(doc.Content["widgets"], &widgets); err != nil {
		t.Fatalf("decode saved widgets: %v", err)
	}
	var ec emailContent
	if err := json.Unmarshal([]byte(`{"content":{"widgets":`+string(doc.Content["widgets"])+`,"flexAreas":`+string(doc.Content["flexAreas"])+`}}`), &ec); err != nil {
		t.Fatalf("decode saved layout: %v", err)
	}
	return widgets, ec.layoutOrder(), doc
}

func widgetHTML(t *testing.T, widgets map[string]map[string]any, key string) string {
	t.Helper()
	body, _ := widgets[key]["body"].(map[string]any)
	s, _ := body["html"].(string)
	return s
}

// TestReplaceEmailBodyKeepingChrome_KeepsTheBrandsHeaderAndFooter is the newsletter defect: a full
// rebuild put the standard LF Events footer and LF's social links under another brand's issue.
func TestReplaceEmailBodyKeepingChrome_KeepsTheBrandsHeaderAndFooter(t *testing.T) {
	c, patchCount := rebuildTestServer(t, newsletterDraft())

	_, kept, err := c.ReplaceEmailBodyKeepingChrome(context.Background(), "email-1", ReplaceEmailBodyInput{
		BodyHTML: `<h1 style="color:#0f172a;">Acme Newsletter: November</h1><p style="margin:0;">Lead-in.</p>` +
			`<h2 style="font-size:21px;">First item</h2><p>About it. <a href="https://acme.example/one?a=1&amp;b=2" style="color:#2563eb;">Read it</a></p>` +
			`<hr /><h2>Second item</h2><ul><li>Point</li></ul>`,
		ButtonText:  "Read More",
		ButtonURL:   "https://acme.example/november",
		PreviewText: "November preview",
	})
	if err != nil {
		t.Fatalf("ReplaceEmailBodyKeepingChrome: %v", err)
	}
	if patchCount.Load() != 1 {
		t.Fatalf("expected exactly one PATCH, got %d", patchCount.Load())
	}
	widgets, order, doc := savedDraft(t, c)

	want := []string{"logo", "staging_body", "staging_body_2", "staging_body_3", "staging_button", "follow_title", "social", "rule", "sent_by", "footer"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("layout order = %v\nwant           %v", order, want)
	}
	for _, gone := range []string{"old_intro", "old_image", "old_button"} {
		if _, ok := widgets[gone]; ok {
			t.Errorf("last issue's %s is still in the draft", gone)
		}
	}
	if kept.HeaderSections != 1 || kept.FooterSections != 4 || kept.ContentModules != 3 {
		t.Errorf("kept = %+v, want 1 header section, 4 footer sections, 3 content modules", kept)
	}
	if kept.HeaderImage == nil || kept.HeaderImage.Alt != "Acme Foundation" || kept.HeaderImage.Link != "https://acme.example/" {
		t.Errorf("header image = %+v, want the brand logo", kept.HeaderImage)
	}

	// The brand's own footer is untouched, social links and footer module included.
	if got := widgetHTML(t, widgets, "sent_by"); !strings.Contains(got, "Acme Foundation") {
		t.Errorf("the sent-by line changed: %q", got)
	}
	footerBody, _ := widgets["footer"]["body"].(map[string]any)
	if footerBody["path"] != "/email_footer no links" {
		t.Errorf("the brand's footer module was replaced: %v", footerBody)
	}
	if strings.Contains(string(doc.Content["widgets"]), "linuxfoundation") {
		t.Errorf("the standard LF footer leaked into a brand's newsletter")
	}

	// The new modules take the draft's styles: no inline style, the previous module's settings.
	first := widgetHTML(t, widgets, "staging_body")
	if strings.Contains(first, "style=") {
		t.Errorf("inline styles were kept: %q", first)
	}
	if !strings.Contains(first, "<h1>Acme Newsletter: November</h1>") || !strings.Contains(first, stagedMarkerPrefix) {
		t.Errorf("first module = %q, want the title, the lead-in and this write's marker", first)
	}
	second := widgetHTML(t, widgets, "staging_body_2")
	if !strings.Contains(second, `<a href="https://acme.example/one?a=1&amp;b=2">Read it</a>`) {
		t.Errorf("the item's link did not survive: %q", second)
	}
	if strings.Contains(second, "<hr") || strings.Contains(widgetHTML(t, widgets, "staging_body_3"), "<hr") {
		t.Errorf("a top-level divider must become a module boundary, not stay in the html")
	}
	body2, _ := widgets["staging_body_2"]["body"].(map[string]any)
	if body2["css_class"] != "dnd-module" || body2["path"] != "@hubspot/rich_text" {
		t.Errorf("a new module should be modelled on the previous issue's text module: %v", body2)
	}

	// The button keeps the brand's colours and takes the new label and destination.
	button, _ := widgets["staging_button"]["body"].(map[string]any)
	if button["background_color"] != "#003778" || button["text"] != "Read More" || button["destination"] != "https://acme.example/november" {
		t.Errorf("button = %v", button)
	}

	// The content sections take the style of the section they replace; the template is not reset.
	if !strings.Contains(string(doc.Content["flexAreas"]), `"backgroundColor":"#EEEDF6"`) {
		t.Errorf("new content sections lost the content area's background")
	}
	var templatePath string
	_ = json.Unmarshal(doc.Content["templatePath"], &templatePath)
	if templatePath != "@hubspot/email/dnd/Start_from_scratch.html" || !strings.Contains(string(doc.Content["styleSettings"]), "Georgia") {
		t.Errorf("template path or style settings were not sent back unchanged")
	}
	preview, _ := widgets["preview_text"]["body"].(map[string]any)
	if preview["value"] != "November preview" {
		t.Errorf("preview text = %v", preview)
	}
}

// TestReplaceEmailBodyKeepingChrome_HeaderIsOnlyWhatOpensTheEmail pins the header rule on the
// other layout the portal uses: a view-in-browser module, then one banner. A second image is
// last issue's content, and a promo image above the follow-us heading is too.
func TestReplaceEmailBodyKeepingChrome_HeaderIsOnlyWhatOpensTheEmail(t *testing.T) {
	draft := newsletterDraft()
	widgets := draft["widgets"].(map[string]any)
	widgets["view_online"] = map[string]any{"body": map[string]any{"module_id": 2794854}}
	widgets["second_image"] = map[string]any{"body": map[string]any{"module_id": 1367093, "img": map[string]any{"src": "https://f.example/promo.png"}}}
	area := draft["flexAreas"].(map[string]any)["main"].(map[string]any)
	sections := area["sections"].([]any)
	one := func(id, widget string) map[string]any {
		return map[string]any{"id": id, "columns": []any{map[string]any{"id": id + "-col", "width": 12, "widgets": []string{widget}}}}
	}
	area["sections"] = append([]any{one("s-view", "view_online"), sections[0], one("s-second", "second_image")}, sections[1:]...)

	c, _ := rebuildTestServer(t, draft)
	_, kept, err := c.ReplaceEmailBodyKeepingChrome(context.Background(), "email-1", ReplaceEmailBodyInput{BodyHTML: "<p>Only item</p>"})
	if err != nil {
		t.Fatalf("ReplaceEmailBodyKeepingChrome: %v", err)
	}
	_, order, _ := savedDraft(t, c)
	want := "view_online,logo,staging_body,follow_title,social,rule,sent_by,footer"
	if strings.Join(order, ",") != want {
		t.Fatalf("layout order = %v, want %s", order, want)
	}
	if kept.HeaderSections != 2 {
		t.Errorf("header sections = %d, want the view-in-browser line and the logo", kept.HeaderSections)
	}
}

// TestReplaceEmailBodyKeepingChrome_NoFooterWritesNothing pins the fallback contract: without a
// footer to keep, nothing is written and the caller is told so by a typed error.
func TestReplaceEmailBodyKeepingChrome_NoFooterWritesNothing(t *testing.T) {
	c, patchCount := rebuildTestServer(t, map[string]any{
		"widgets":   map[string]any{"text": map[string]any{"body": map[string]any{"html": "<p>stale</p>"}}},
		"flexAreas": map[string]any{"main": map[string]any{"sections": []any{map[string]any{"columns": []any{map[string]any{"widgets": []string{"text"}}}}}}},
	})
	_, _, err := c.ReplaceEmailBodyKeepingChrome(context.Background(), "email-1", ReplaceEmailBodyInput{BodyHTML: "<p>new</p>"})
	if !errors.Is(err, ErrNoBrandChrome) {
		t.Fatalf("err = %v, want ErrNoBrandChrome", err)
	}
	if patchCount.Load() != 0 {
		t.Fatalf("a draft with no footer to keep must not be written, got %d PATCH", patchCount.Load())
	}
}

// TestReplaceEmailBodyKeepingChrome_DetectsSilentRevert holds the same verification a full
// rebuild has: a 2xx PATCH that HubSpot reverted is reported, not returned as a staged draft.
func TestReplaceEmailBodyKeepingChrome_DetectsSilentRevert(t *testing.T) {
	original := newsletterDraft()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Every GET answers with the ORIGINAL draft: the PATCH is accepted and thrown away.
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{"content": original})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "email-1"})
	})
	_, _, err := c.ReplaceEmailBodyKeepingChrome(context.Background(), "email-1", ReplaceEmailBodyInput{BodyHTML: "<p>new</p>"})
	if !errors.Is(err, ErrContentNotPersisted) {
		t.Fatalf("err = %v, want ErrContentNotPersisted", err)
	}
}

func TestReplaceEmailBodyKeepingChrome_RequiresAnIDAndABody(t *testing.T) {
	c, patchCount := rebuildTestServer(t, newsletterDraft())
	if _, _, err := c.ReplaceEmailBodyKeepingChrome(context.Background(), " ", ReplaceEmailBodyInput{BodyHTML: "<p>x</p>"}); err == nil {
		t.Error("a blank id must be refused")
	}
	if _, _, err := c.ReplaceEmailBodyKeepingChrome(context.Background(), "email-1", ReplaceEmailBodyInput{BodyHTML: "<p> </p>"}); err == nil {
		t.Error("a body with no visible text must be refused: it would empty the draft between its header and footer")
	}
	if patchCount.Load() != 0 {
		t.Errorf("a refused call must write nothing, got %d PATCH", patchCount.Load())
	}
}

func TestSplitAtTopLevelHeadings(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"no heading is one piece", "<p>a</p><p>b</p>", []string{"<p>a</p><p>b</p>"}},
		{"each top-level h2 starts a piece", "<h1>T</h1><p>lead</p><h2>A</h2><p>a</p><h2>B</h2><p>b</p>",
			[]string{"<h1>T</h1><p>lead</p>", "<h2>A</h2><p>a</p>", "<h2>B</h2><p>b</p>"}},
		{"a nested h2 does not split", "<div><h2>A</h2><p>a</p></div><h2>B</h2>", []string{"<div><h2>A</h2><p>a</p></div>", "<h2>B</h2>"}},
		{"a top-level divider splits and is dropped", "<p>a</p><hr /><p>b</p>", []string{"<p>a</p>", "<p>b</p>"}},
		{"an empty piece is dropped", "<h2>A</h2><hr><hr><h2>B</h2>", []string{"<h2>A</h2>", "<h2>B</h2>"}},
		{"a br does not open a level", "<p>a<br>b</p><h2>B</h2>", []string{"<p>a<br>b</p>", "<h2>B</h2>"}},
		{"blank", "  ", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitAtTopLevelHeadings(tc.in)
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStripInlineStyles(t *testing.T) {
	in := `<p style="margin:0 0 14px;color:#1b1b1f;">Tom &amp; Jerry <a href="https://acme.example/x?a=1&amp;b=2" target="_blank" style="color:#2563eb;">read</a><br/></p><!-- lfx-staged:0123456789abcdef -->`
	want := `<p>Tom &amp; Jerry <a href="https://acme.example/x?a=1&amp;b=2" target="_blank">read</a><br/></p><!-- lfx-staged:0123456789abcdef -->`
	if got := stripInlineStyles(in); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	// Markup with no style attribute is returned byte for byte.
	plain := `<p>The word style is not an attribute here.</p>`
	if got := stripInlineStyles(plain); got != plain {
		t.Errorf("unstyled markup changed: %s", got)
	}
}
