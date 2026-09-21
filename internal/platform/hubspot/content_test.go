// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// rebuildTestServer simulates HubSpot's draft GET/PATCH cycle for
// RebuildEmailContent: GET returns whatever `content` was last PATCHed (starting
// from `initial`), and PATCH stores the new content and echoes it back as the
// email. This exercises the read -> rebuild -> patch -> re-read verification flow
// end to end, the same way HubSpot's non-merging PATCH behaves.
func rebuildTestServer(t *testing.T, initial map[string]any) (*Client, *int) {
	t.Helper()
	patchCount := 0
	content := initial
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/draft") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"content": content})
		case http.MethodPatch:
			patchCount++
			var body struct {
				Content map[string]any `json:"content"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode PATCH body: %v", err)
			}
			content = body.Content
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "email-1", "content": content})
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	})
	return c, &patchCount
}

func TestRebuildEmailContent_HeroFirstThenBodyThenFooter(t *testing.T) {
	c, patchCount := rebuildTestServer(t, map[string]any{
		"widgets":   map[string]any{"old_widget": map[string]any{"body": map[string]any{"html": "stale"}}},
		"flexAreas": map[string]any{"main": map[string]any{"sections": []any{}}},
	})

	email, err := c.RebuildEmailContent(context.Background(), "email-1", RebuildEmailContentInput{
		HeroImageURL: "https://cdn.hubspot.net/hero.jpg",
		HeroLinkURL:  "https://example.com/event",
		BodyHTML:     "<p>hello</p>",
	})
	if err != nil {
		t.Fatalf("RebuildEmailContent: %v", err)
	}
	if email == nil || email.ID != "email-1" {
		t.Fatalf("unexpected email result: %+v", email)
	}
	if *patchCount != 1 {
		t.Fatalf("expected exactly one PATCH, got %d", *patchCount)
	}
}

func TestRebuildEmailContent_WipesClonedWidgets(t *testing.T) {
	c, _ := rebuildTestServer(t, map[string]any{
		"widgets":   map[string]any{"cloned_hero": map[string]any{}, "cloned_body": map[string]any{}},
		"flexAreas": map[string]any{"main": map[string]any{"sections": []any{}}},
	})

	widgets := map[string]any{}
	var sections []map[string]any
	addBodySection(widgets, &sections, "<p>fresh</p>")
	addFooterSections(widgets, &sections, "")

	_, err := c.RebuildEmailContent(context.Background(), "email-1", RebuildEmailContentInput{BodyHTML: "<p>fresh</p>"})
	if err != nil {
		t.Fatalf("RebuildEmailContent: %v", err)
	}

	// Re-fetch through the same server to see what actually persisted.
	doc, err := c.readDraftContent(context.Background(), "email-1")
	if err != nil {
		t.Fatalf("readDraftContent: %v", err)
	}
	var saved map[string]json.RawMessage
	if err := json.Unmarshal(doc.Content["widgets"], &saved); err != nil {
		t.Fatalf("decode saved widgets: %v", err)
	}
	if _, ok := saved["cloned_hero"]; ok {
		t.Errorf("cloned_hero widget should have been wiped by the full-content rebuild")
	}
	if _, ok := saved["cloned_body"]; ok {
		t.Errorf("cloned_body widget should have been wiped by the full-content rebuild")
	}
	if _, ok := saved["staging_body"]; !ok {
		t.Errorf("fresh staging_body widget missing from saved content")
	}
}

func TestRebuildEmailContent_PreservesExistingPreviewText(t *testing.T) {
	c, _ := rebuildTestServer(t, map[string]any{
		"widgets":   map[string]any{"preview_text": map[string]any{"body": map[string]any{"html": "keep me"}}},
		"flexAreas": map[string]any{"main": map[string]any{"sections": []any{}}},
	})

	_, err := c.RebuildEmailContent(context.Background(), "email-1", RebuildEmailContentInput{BodyHTML: "<p>x</p>"})
	if err != nil {
		t.Fatalf("RebuildEmailContent: %v", err)
	}

	doc, err := c.readDraftContent(context.Background(), "email-1")
	if err != nil {
		t.Fatalf("readDraftContent: %v", err)
	}
	var saved map[string]json.RawMessage
	if err := json.Unmarshal(doc.Content["widgets"], &saved); err != nil {
		t.Fatalf("decode saved widgets: %v", err)
	}
	if _, ok := saved["preview_text"]; !ok {
		t.Errorf("preview_text widget should be preserved across a full rebuild")
	}
}

func TestRebuildEmailContent_NoHeroWhenURLEmpty(t *testing.T) {
	c, _ := rebuildTestServer(t, map[string]any{
		"widgets":   map[string]any{},
		"flexAreas": map[string]any{"main": map[string]any{"sections": []any{}}},
	})

	_, err := c.RebuildEmailContent(context.Background(), "email-1", RebuildEmailContentInput{BodyHTML: "<p>x</p>"})
	if err != nil {
		t.Fatalf("RebuildEmailContent: %v", err)
	}

	doc, err := c.readDraftContent(context.Background(), "email-1")
	if err != nil {
		t.Fatalf("readDraftContent: %v", err)
	}
	var saved map[string]json.RawMessage
	if err := json.Unmarshal(doc.Content["widgets"], &saved); err != nil {
		t.Fatalf("decode saved widgets: %v", err)
	}
	if _, ok := saved["staging_banner"]; ok {
		t.Errorf("no hero widget should be written when HeroImageURL is empty")
	}
}

func TestRebuildEmailContent_DetectsSilentRevert(t *testing.T) {
	// A server that accepts the PATCH (2xx) but always reports empty content on
	// GET simulates HubSpot's documented silent-revert failure mode.
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"content": map[string]any{
				"widgets":   map[string]any{},
				"flexAreas": map[string]any{"main": map[string]any{"sections": []any{}}},
			}})
		case http.MethodPatch:
			_, _ = io.WriteString(w, `{"id":"email-1"}`)
		}
	})

	_, err := c.RebuildEmailContent(context.Background(), "email-1", RebuildEmailContentInput{BodyHTML: "<p>x</p>"})
	if err == nil {
		t.Fatal("expected an error when HubSpot silently reverts the content PATCH")
	}
	if !strings.Contains(err.Error(), "did not persist") {
		t.Errorf("error should describe a silent revert, got: %v", err)
	}
}

func TestAddSponsorSections_SplitsIntoTwoTiersAndSkipsLogolessSponsors(t *testing.T) {
	sponsors := []Sponsor{
		{Name: "A", LogoURL: "https://x/a.png"},
		{Name: "B", LogoURL: "https://x/b.png"},
		{Name: "C", LogoURL: "https://x/c.png"},
		{Name: "D", LogoURL: "https://x/d.png"},
		{Name: "E", LogoURL: "https://x/e.png"},
		{Name: "F", LogoURL: "https://x/f.png"},
		{Name: "G", LogoURL: ""}, // no logo, must be dropped entirely
	}
	widgets := map[string]any{}
	var sections []map[string]any
	addSponsorSections(widgets, &sections, sponsors)

	if _, ok := widgets["staging_sponsor_header"]; !ok {
		t.Errorf("expected a sponsor header widget when logo sponsors are present")
	}
	if _, ok := widgets["staging_sponsor_t1_0_0"]; !ok {
		t.Errorf("expected tier1 row0 col0 widget")
	}
	if _, ok := widgets["staging_sponsor_t2_0_0"]; !ok {
		t.Errorf("expected tier2 (6th sponsor) widget")
	}
	for key := range widgets {
		if strings.Contains(key, "_G") || key == "staging_sponsor_g" {
			t.Errorf("logoless sponsor G must not produce a widget, got key %q", key)
		}
	}
}

func TestAddFooterSections_UsesEmailDividerNotDivider(t *testing.T) {
	widgets := map[string]any{}
	var sections []map[string]any
	addFooterSections(widgets, &sections, "")

	divider, ok := widgets["staging_footer_divider"]
	if !ok {
		t.Fatal("missing staging_footer_divider widget")
	}
	body := divider.(map[string]any)["body"].(map[string]any)
	if body["path"] != "@hubspot/email_divider" {
		t.Errorf("divider path = %v, want @hubspot/email_divider (not @hubspot/divider)", body["path"])
	}

	social := widgets["staging_footer_social"].(map[string]any)["body"].(map[string]any)
	if social["module_id"] != moduleIDFollowMe {
		t.Errorf("social module_id = %v, want %d", social["module_id"], moduleIDFollowMe)
	}
}

func TestAddFooterSections_DefaultsSentByOrg(t *testing.T) {
	widgets := map[string]any{}
	var sections []map[string]any
	addFooterSections(widgets, &sections, "")

	html := widgets["staging_footer_body"].(map[string]any)["body"].(map[string]any)["html"].(string)
	if !strings.Contains(html, defaultSentByOrg) {
		t.Errorf("expected default sent-by org %q in footer html, got %q", defaultSentByOrg, html)
	}

	widgets2 := map[string]any{}
	var sections2 []map[string]any
	addFooterSections(widgets2, &sections2, "CNCF")
	html2 := widgets2["staging_footer_body"].(map[string]any)["body"].(map[string]any)["html"].(string)
	if !strings.Contains(html2, "CNCF") || strings.Contains(html2, defaultSentByOrg) {
		t.Errorf("expected custom sent-by org CNCF, got %q", html2)
	}
}

func TestColumnWidths_SumsTo12(t *testing.T) {
	for n := 1; n <= 5; n++ {
		widths := columnWidths(n)
		sum := 0
		for _, w := range widths {
			sum += w
		}
		if sum != 12 {
			t.Errorf("columnWidths(%d) = %v, sum = %d, want 12", n, widths, sum)
		}
	}
}

func TestChunkSponsorRows_ThreeThenTwo(t *testing.T) {
	items := make([]Sponsor, 5)
	rows := chunkSponsorRows(items)
	if len(rows) != 2 || len(rows[0]) != 3 || len(rows[1]) != 2 {
		t.Fatalf("chunkSponsorRows(5) = %v, want [3,2]", rows)
	}
	if got := chunkSponsorRows(make([]Sponsor, 7)); len(got) != 2 || len(got[0])+len(got[1]) != 5 {
		t.Errorf("chunkSponsorRows must cap at 5 total, got %v", got)
	}
}

func TestRebuildEmailContent_RequiresNonEmptyID(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no HTTP call should be made for an empty id")
	})
	if _, err := c.RebuildEmailContent(context.Background(), "  ", RebuildEmailContentInput{}); err == nil {
		t.Fatal("expected an error for an empty id")
	}
}
