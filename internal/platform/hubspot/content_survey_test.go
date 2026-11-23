// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// savedLayout rebuilds a draft and returns what persisted: the ordered section ids, and the widget
// bodies by key. Read back through the same fake the real verification read uses, so it sees what
// HubSpot would have stored rather than what this test built.
func savedLayout(t *testing.T, in RebuildEmailContentInput) (sectionIDs []string, widgets map[string]map[string]any) {
	t.Helper()
	c, _ := rebuildTestServer(t, map[string]any{
		"widgets":   map[string]any{},
		"flexAreas": map[string]any{"main": map[string]any{"sections": []any{}}},
	})
	if _, err := c.RebuildEmailContent(context.Background(), "email-1", in); err != nil {
		t.Fatalf("RebuildEmailContent: %v", err)
	}
	doc, err := c.readDraftContent(context.Background(), "email-1")
	if err != nil {
		t.Fatalf("readDraftContent: %v", err)
	}
	var flex map[string]struct {
		Sections []struct {
			ID string `json:"id"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(doc.Content["flexAreas"], &flex); err != nil {
		t.Fatalf("decode flexAreas: %v", err)
	}
	for _, area := range flex {
		for _, s := range area.Sections {
			sectionIDs = append(sectionIDs, s.ID)
		}
	}
	var raw map[string]struct {
		Body map[string]any `json:"body"`
	}
	if err := json.Unmarshal(doc.Content["widgets"], &raw); err != nil {
		t.Fatalf("decode widgets: %v", err)
	}
	widgets = map[string]map[string]any{}
	for k, v := range raw {
		widgets[k] = v.Body
	}
	return sectionIDs, widgets
}

// The survey layout is the sent email's order, top to bottom: view-in-browser strip, logo band,
// hero, body, button, then divider / social / divider / sent-by + footer. A reorder here is a
// different email from the one the research team sends.
func TestRebuildEmailContent_SurveyLayoutFollowsTheSentEmail(t *testing.T) {
	ids, widgets := savedLayout(t, RebuildEmailContentInput{
		Layout:       LayoutSurvey,
		HeroImageURL: "https://cdn.hubspot.net/hero.png",
		HeroLinkURL:  "https://www.research.net/r/ABC123",
		BodyHTML:     "<p>hello</p>",
		ButtonText:   "Take the Survey",
		ButtonURL:    "https://www.research.net/r/ABC123",
	})

	want := []string{
		"section-survey-preheader", "section-survey-logo", "section-staging-banner",
		"section-staging-body", "section-staging-button",
		"section-survey-divider-top", "section-survey-social", "section-survey-divider-bottom", "section-survey-footer",
	}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("sections = %v, want %v", ids, want)
	}

	// The strip is HubSpot's built-in View-in-browser module, by id, with no path -- as the real
	// widget carries it.
	strip := widgets["staging_survey_preheader"]
	if got, _ := strip["module_id"].(float64); int(got) != moduleIDViewInBrowser {
		t.Errorf("preheader module_id = %v, want %d", strip["module_id"], moduleIDViewInBrowser)
	}
	if _, hasPath := strip["path"]; hasPath {
		t.Errorf("preheader carries a path; the sent widget has none: %v", strip)
	}

	logo := widgets["staging_survey_logo"]
	if logo["link"] != surveyLogoLink {
		t.Errorf("logo link = %v, want %s", logo["link"], surveyLogoLink)
	}
	if img, _ := logo["img"].(map[string]any); img["src"] != surveyLogoURL {
		t.Errorf("logo src = %v, want %s", img["src"], surveyLogoURL)
	}

	// An absent hero alt falls back to a survey description, not addHeroSection's "Event banner".
	if img, _ := widgets["staging_banner"]["img"].(map[string]any); img["alt"] != "Survey banner" {
		t.Errorf("hero alt = %v, want Survey banner", img["alt"])
	}

	for _, divider := range []string{"staging_survey_divider_top", "staging_survey_divider_bottom"} {
		colour, _ := widgets[divider]["color"].(map[string]any)
		if colour["color"] != surveyNavy {
			t.Errorf("%s colour = %v, want %s", divider, colour["color"], surveyNavy)
		}
	}

	// The social row carries the seven entries the sent row does, in its order.
	social, _ := widgets["staging_survey_social"]["social"].([]any)
	var networks []string
	for _, e := range social {
		networks = append(networks, e.(map[string]any)["network"].(string))
	}
	if got, want := strings.Join(networks, ","), "facebook,linkedin,youtube,instagram,icon,mail,icon"; got != want {
		t.Errorf("social networks = %s, want %s", got, want)
	}
}

// What the survey layout must NOT carry over from the event one. Each of these was in the staged
// survey drafts before the layout existed.
func TestRebuildEmailContent_SurveyLayoutHasNoEventChrome(t *testing.T) {
	ids, widgets := savedLayout(t, RebuildEmailContentInput{
		Layout:   LayoutSurvey,
		BodyHTML: "<p>hello</p>",
		Sponsors: []Sponsor{{Name: "Acme", LogoURL: "https://cdn.example/acme.png"}},
	})
	for _, id := range ids {
		if strings.Contains(id, "sponsor") || strings.Contains(id, "follow-header") {
			t.Errorf("survey layout carries an event section: %s", id)
		}
	}
	for key, body := range widgets {
		blob, _ := json.Marshal(body)
		for _, banned := range []string{"FOLLOW US", "LFX Insights", "The Linux Foundation Events", "Thank You to Our Sponsors"} {
			if strings.Contains(string(blob), banned) {
				t.Errorf("widget %s carries event chrome %q", key, banned)
			}
		}
	}
	sentBy, _ := widgets["staging_survey_sent_by"]["html"].(string)
	if !strings.Contains(sentBy, ">The Linux Foundation<") {
		t.Errorf("sent-by line should name The Linux Foundation, got %s", sentBy)
	}
}

// The organisation name is caller-supplied and lands inside markup.
func TestRebuildEmailContent_SurveySentByEscapesTheOrganisation(t *testing.T) {
	_, widgets := savedLayout(t, RebuildEmailContentInput{
		Layout:    LayoutSurvey,
		BodyHTML:  "<p>x</p>",
		SentByOrg: `CNCF <script>alert(1)</script>`,
	})
	sentBy, _ := widgets["staging_survey_sent_by"]["html"].(string)
	if strings.Contains(sentBy, "<script>") {
		t.Errorf("org was written unescaped: %s", sentBy)
	}
	if !strings.Contains(sentBy, "CNCF &lt;script&gt;") {
		t.Errorf("org should be present, escaped: %s", sentBy)
	}
}

// The zero layout is the event layout, byte for byte what every earlier caller got. Pinned because
// the native footer was extracted into a helper both layouts share.
func TestRebuildEmailContent_EventLayoutIsUnchanged(t *testing.T) {
	ids, widgets := savedLayout(t, RebuildEmailContentInput{BodyHTML: "<p>x</p>"})
	want := []string{
		"section-staging-body", "section-footer-divider", "section-footer-follow-header",
		"section-footer-social", "section-footer-body", "section-footer-hs",
	}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("event sections = %v, want %v", ids, want)
	}
	footer := widgets["staging_footer_hs"]
	font, _ := footer["font"].(map[string]any)
	link, _ := footer["link_font"].(map[string]any)
	if font["color"] != "#000000" || link["color"] != footerLinkColor {
		t.Errorf("event footer colours changed: font=%v link=%v", font["color"], link["color"])
	}
	if !strings.Contains(widgets["staging_footer_body"]["html"].(string), "The Linux Foundation Events") {
		t.Errorf("event sent-by line should still default to The Linux Foundation Events")
	}
}
