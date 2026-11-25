// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service/emailstage"
)

func TestSourceDocumentLinks(t *testing.T) {
	notes := "Guide is out: https://acme.example/blog/guide/.\n" +
		"Events (https://Acme.example/events) and a report: HTTPS://acme.example/report?year=2026&x=1#top,\n" +
		"not a link: ftp://acme.example/file, example.org/bare"
	got := sourceDocumentLinks(notes)
	for _, want := range []string{
		"https://acme.example/blog/guide",
		"https://acme.example/events",
		"https://acme.example/report?year=2026&x=1",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %q in %v", want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("got %d links, want 3: %v", len(got), got)
	}
}

func TestNormalizeLinkForMatch(t *testing.T) {
	cases := map[string]string{
		"https://Acme.Example/Blog/Post/":          "https://acme.example/Blog/Post",
		" https://acme.example/x#section ":         "https://acme.example/x",
		"https://acme.example":                     "https://acme.example",
		"https://acme.example/x?a=1&b=2":           "https://acme.example/x?a=1&b=2",
		"javascript:alert(1)":                      "",
		"/relative/path":                           "",
		"mailto:someone@acme.example":              "",
		"":                                         "",
		"https://user:secret@acme.example/private": "https://user:secret@acme.example/private",
	}
	for in, want := range cases {
		if got := normalizeLinkForMatch(in); got != want {
			t.Errorf("normalizeLinkForMatch(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUnlinkUnvouchedAnchors pins both halves: a link the notes carry is returned byte for byte,
// and one they do not carry keeps its words and loses every attribute.
func TestUnlinkUnvouchedAnchors(t *testing.T) {
	allowed := sourceDocumentLinks("https://acme.example/guide and https://acme.example/report?y=1&z=2")
	in := `<p style="margin:0;">Read <a href="https://acme.example/guide/" target="_blank" style="color:#2563eb;">the guide</a>, ` +
		`<a href="https://acme.example/report?y=1&amp;z=2">the report</a>, ` +
		`<a href="https://acme.example/invented-post" style="color:#2563eb;">a post</a> and ` +
		`<a href="https://evil.example/guide">another site</a>. <a>No address</a> &amp; <A HREF="https://acme.example/guide">again</A></p>`
	want := `<p style="margin:0;">Read <a href="https://acme.example/guide/" target="_blank" style="color:#2563eb;">the guide</a>, ` +
		`<a href="https://acme.example/report?y=1&amp;z=2">the report</a>, ` +
		`<a>a post</a> and ` +
		`<a>another site</a>. <a>No address</a> &amp; <A HREF="https://acme.example/guide">again</A></p>`
	got, dropped := unlinkUnvouchedAnchors(in, allowed)
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if dropped != 3 {
		t.Errorf("dropped = %d, want 3 (the invented path, the other site and the anchor with no address)", dropped)
	}

	plain := `<p>No anchors here, only a &lt;a&gt; in text.</p>`
	if got, n := unlinkUnvouchedAnchors(plain, allowed); got != plain || n != 0 {
		t.Errorf("markup without anchors changed: %q (%d)", got, n)
	}
}

func TestEnforceSourceLinks_ButtonWithoutAVouchedDestinationLosesIt(t *testing.T) {
	html := `<p><a href="https://acme.example/elsewhere">x</a></p>`
	kept, gone := "https://acme.example/guide", "https://acme.example/not-in-notes"
	copy := &briefs.EmailCopy{Sections: []*briefs.EmailCopySection{
		{Type: "rich_text", HTML: &html},
		{Type: "button", URL: &kept},
		{Type: "button", URL: &gone},
		nil,
	}}
	if dropped := enforceSourceLinks(copy, "Guide: https://acme.example/guide"); dropped != 2 {
		t.Errorf("dropped = %d, want 2", dropped)
	}
	if *copy.Sections[0].HTML != `<p><a>x</a></p>` {
		t.Errorf("rich text = %q", *copy.Sections[0].HTML)
	}
	if copy.Sections[1].URL == nil || *copy.Sections[1].URL != kept {
		t.Error("a button whose destination is in the notes must keep it")
	}
	if copy.Sections[2].URL != nil {
		t.Error("a button whose destination is not in the notes must lose it")
	}
	if enforceSourceLinks(nil, "x") != 0 {
		t.Error("a nil copy has nothing to remove")
	}
}

// TestNewsletterFactsBlock_KeepsTheNotesLines: the notes reach the prompt one item per line, so
// the model can tell which address belongs to which item.
func TestNewsletterFactsBlock_KeepsTheNotesLines(t *testing.T) {
	d := emailCopyEventDetails{SourceText: "  First item:   https://acme.example/one \r\n\r\nSecond\titem Third item: https://acme.example/three"}
	got := d.newsletterFactsBlock()
	want := "\nSource document: \n  - First item: https://acme.example/one\n  - Second item\n  - Third item: https://acme.example/three"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	// Every other brief keeps the single-line form.
	if flat := d.eventFactsBlock(); strings.Contains(flat, "\n  - ") || !strings.Contains(flat, "First item: https://acme.example/one Second item Third item") {
		t.Errorf("eventFactsBlock must stay single-line, got %q", flat)
	}
}

// TestNewsletterFactsBlock_FallsBackWhenLinesWouldNotFit: thousands of one-character lines cost
// more in markers than the bound allows; the notes must then arrive collapsed, never be left out.
func TestNewsletterFactsBlock_FallsBackWhenLinesWouldNotFit(t *testing.T) {
	d := emailCopyEventDetails{SourceText: strings.Repeat("a\n", 1400)}
	if serr := sourceTextTooLong(d.SourceText); serr != nil {
		t.Fatalf("the fixture must be notes the service accepts: %v", serr)
	}
	got := d.newsletterFactsBlock()
	if strings.Contains(got, "\n  - ") || !strings.Contains(got, "Source document: a a a") {
		t.Errorf("want the collapsed single-line form, got %.80q...", got)
	}
}

// TestComposeEmailCopyPrompt_NewsletterStatesWhereLinksComeFrom: the shared link rules are written
// for an event email and, read alone, tell the model to write a newsletter with no links.
func TestComposeEmailCopyPrompt_NewsletterStatesWhereLinksComeFrom(t *testing.T) {
	system, user := composeEmailCopyPrompt(emailCopyPromptVars{
		eventName:  "Acme Newsletter - November 2026",
		dates:      "Date TBD",
		stage:      emailstage.NewsletterIssue,
		eventFacts: "\nSource document: \n  - Guide: https://acme.example/guide",
	})
	if !strings.Contains(system, "NEWSLETTER LINKS") || !strings.Contains(system, "The Source document is the only place a link may come from") {
		t.Error("the newsletter system prompt must state its own link rule")
	}
	if strings.Contains(user, "Location:") || strings.Contains(user, "Date TBD") {
		t.Errorf("a newsletter has no venue or dates; the prompt must not print the lines: %q", user)
	}
	if !strings.Contains(user, "Event Name: Acme Newsletter - November 2026\nSource document: \n  - Guide: https://acme.example/guide") {
		t.Errorf("the issue's name and notes must reach the prompt: %q", user)
	}

	// An event stage is unchanged: no newsletter rule, and its venue and dates lines stay.
	eventSystem, eventUser := composeEmailCopyPrompt(emailCopyPromptVars{eventName: "Acme Summit", location: "Lisbon", dates: "May 4", stage: emailstage.RegistrationPush})
	if strings.Contains(eventSystem, "NEWSLETTER LINKS") || !strings.Contains(eventUser, "Location: Lisbon\nDates: May 4") {
		t.Error("the newsletter rule and prompt shape must not reach an event stage")
	}
}

// TestGenerateEmailCopy_NewsletterKeepsOnlyLinksFromTheNotes drives the endpoint: the model links
// one item to the address the notes give and invents one for another. The first survives, the
// second loses its href, and the notes reached the model with their lines.
func TestGenerateEmailCopy_NewsletterKeepsOnlyLinksFromTheNotes(t *testing.T) {
	repo := newFakeBriefRepo()
	repo.briefs[briefKey("proj-123", "brief-nl")] = &model.CampaignBrief{
		ID:           "brief-nl",
		ProjectID:    "proj-123",
		DeliveryType: model.DeliveryNewsletter,
		EventDetails: json.RawMessage(`{"eventName":"Acme Newsletter - November 2026","sourceText":"Guide is out: https://acme.example/guide\nNew members joined"}`),
	}
	var prompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		prompt = string(raw)
		content := `{"subject":"Acme Newsletter: the guide is out","preheader":"New members too","sections":[` +
			`{"type":"rich_text","html":"<h1>Acme Newsletter</h1><p>Read <a href=\"https://acme.example/guide\">the guide</a>.</p>"},` +
			`{"type":"rich_text","html":"<h2>New members</h2><p>Meet <a href=\"https://acme.example/members\">them</a>.</p>"}]}`
		encoded, _ := json.Marshal(content)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":` + string(encoded) + `},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	svc := newTestBriefService(repo)
	svc.SetLLMClient(newTestLLMClient(t, srv))
	stage := emailstage.NewsletterIssue
	result, err := svc.GenerateEmailCopy(context.Background(), &briefs.GenerateEmailCopyPayload{
		ProjectID: "proj-123", BriefID: "brief-nl", BearerToken: strPtr("token"), Stage: &stage,
	})
	if err != nil {
		t.Fatalf("GenerateEmailCopy: %v", err)
	}
	if len(result.Sections) != 2 {
		t.Fatalf("sections = %d, want 2", len(result.Sections))
	}
	first, second := *result.Sections[0].HTML, *result.Sections[1].HTML
	if !strings.Contains(first, `href="https://acme.example/guide"`) {
		t.Errorf("the link the notes carry must survive: %s", first)
	}
	if strings.Contains(second, "href=") || !strings.Contains(second, ">them</a>") {
		t.Errorf("a link the notes do not carry must lose its href and keep its words: %s", second)
	}
	if !strings.Contains(prompt, `- Guide is out: https://acme.example/guide\n  - New members joined`) {
		t.Errorf("the notes must reach the model one item per line; request was %s", prompt)
	}
}
