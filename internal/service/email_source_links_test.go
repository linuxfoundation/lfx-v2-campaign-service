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
	"sync"
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
		"https://acme.example/blog/guide/",
		"https://acme.example/events",
		"https://acme.example/report?x=1&year=2026#top",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing %q in %v", want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("got %d links, want 3: %v", len(got), got)
	}
}

// TestSourceDocumentLinks_DelimitedKeepsPunctuation: inside "<…>" or quotes the address ends at
// the delimiter, so a trailing "!" or "." is the address's own. Trimming it would vouch for a
// shorter address the notes never gave; in open prose it is still trimmed.
func TestSourceDocumentLinks_DelimitedKeepsPunctuation(t *testing.T) {
	notes := "Download <https://acme.example/d?token=abc!> or \"https://acme.example/q?x=1.\" " +
		"or 'https://acme.example/s?y=2)', and see https://acme.example/prose."
	got := sourceDocumentLinks(notes)
	// Keys are compared in match form (the query in httpURL's encoding, so "!" is "%21").
	for _, raw := range []string{
		"https://acme.example/d?token=abc!",
		"https://acme.example/q?x=1.",
		"https://acme.example/s?y=2)",
		"https://acme.example/prose",
	} {
		if _, ok := got[normalizeLinkForMatch(raw)]; !ok {
			t.Errorf("missing %q in %v", raw, got)
		}
	}
	for _, shortened := range []string{"https://acme.example/d?token=abc", "https://acme.example/q?x=1", "https://acme.example/s?y=2"} {
		if _, ok := got[normalizeLinkForMatch(shortened)]; ok {
			t.Errorf("%q is vouched for, but the notes only give it with its punctuation", shortened)
		}
	}
}

// TestEnforceSourceLinks_UnsortedQuerySurvivesSanitising: the copy is sanitised before the check,
// and httpURL sorts the query, so an anchor copied exactly from notes whose parameters are not in
// order must still keep its link. A different value must still lose it.
func TestEnforceSourceLinks_UnsortedQuerySurvivesSanitising(t *testing.T) {
	notes := "The report: https://acme.example/report?year=2026&format=pdf"
	sanitised := hrefAttr([]htmlAttr{{key: "href", val: "https://acme.example/report?year=2026&format=pdf"}})
	if !strings.Contains(sanitised, "format=pdf&amp;year=2026") {
		t.Fatalf("httpURL no longer sorts the query (%s); this test's premise is gone", sanitised)
	}
	body := `<p><a` + sanitised + `>the report</a> <a href="https://acme.example/report?format=pdf&amp;year=2025">last year</a></p>`
	got, dropped := unlinkUnvouchedAnchors(body, sourceDocumentLinks(notes))
	if !strings.Contains(got, `>the report</a>`) || strings.Contains(got, `<a>the report</a>`) || !strings.Contains(got, `<a>last year</a>`) || dropped != 1 {
		t.Errorf("got %s (%d dropped), want the report kept and last year's dropped", got, dropped)
	}
}

// TestUnlinkUnvouchedAnchors_DuplicateHref: a kept anchor is written back with every href it
// carries, and which one a mail client follows is its parser's choice, so an anchor with two
// hrefs loses its link whichever comes first, even when both are vouched for.
func TestUnlinkUnvouchedAnchors_DuplicateHref(t *testing.T) {
	allowed := sourceDocumentLinks("https://acme.example/approved")
	in := `<a href="https://acme.example/invented" HREF="https://acme.example/approved">x</a>` +
		`<a href="https://acme.example/approved"` + "\n" + `href="https://acme.example/invented">y</a>` +
		`<a href="https://acme.example/approved" href = "https://acme.example/approved">z</a>`
	want := `<a>x</a><a>y</a><a>z</a>`
	got, dropped := unlinkUnvouchedAnchors(in, allowed)
	if got != want || dropped != 3 {
		t.Errorf("got %q (%d dropped), want %q (3 dropped)", got, dropped, want)
	}
}

func TestNormalizeLinkForMatch(t *testing.T) {
	cases := map[string]string{
		"https://Acme.Example/Blog/Post/":  "https://acme.example/Blog/Post/",
		" https://acme.example/x#section ": "https://acme.example/x#section",
		"https://acme.example":             "https://acme.example/",
		"HTTPS://ACME.example/":            "https://acme.example/",
		// A slash inside the query is the query's, never trimmed: "file=reports/" and
		// "file=reports" can select different resources.
		"https://acme.example/download?file=reports/": "https://acme.example/download?file=reports%2F",
		// Parameter order is httpURL's (sorted), so a sanitised anchor matches the notes it came from.
		"https://acme.example/r?year=2026&format=pdf": "https://acme.example/r?format=pdf&year=2026",
		// A malformed escape in the query is refused, as httpURL refuses it.
		"https://acme.example/r?q=%zz":   "",
		"https://acme.example/x?a=1&b=2": "https://acme.example/x?a=1&b=2",
		"javascript:alert(1)":            "",
		"/relative/path":                 "",
		"mailto:someone@acme.example":    "",
		"":                               "",
		// Userinfo is refused: it is a credential in a link every recipient receives, and
		// "trusted@evil" reads as one site and opens another.
		"https://user:secret@acme.example/private": "",
		"https://acme.example@evil.example/":       "",
		// A port with no hostname is not a site.
		"https://:8080/path": "",
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
	allowed := sourceDocumentLinks("https://acme.example/guide/ and https://acme.example/report?y=1&z=2 and https://acme.example/faq#fees")
	in := `<p style="margin:0;">Read <a href="https://acme.example/guide/" target="_blank" style="color:#2563eb;">the guide</a>, ` +
		`<a href="https://acme.example/report?y=1&amp;z=2">the report</a>, ` +
		`<a href="https://acme.example/invented-post" style="color:#2563eb;">a post</a> and ` +
		`<a href="https://evil.example/guide">another site</a>. <a>No address</a> &amp; <A HREF="https://acme.example/guide">again</A> ` +
		`<a href="https://acme.example/faq#fees">fees</a> <a href="https://acme.example/faq#invented">a section</a></p>`
	want := `<p style="margin:0;">Read <a href="https://acme.example/guide/" target="_blank" style="color:#2563eb;">the guide</a>, ` +
		`<a href="https://acme.example/report?y=1&amp;z=2">the report</a>, ` +
		`<a>a post</a> and ` +
		`<a>another site</a>. <a>No address</a> &amp; <a>again</A> ` +
		`<a href="https://acme.example/faq#fees">fees</a> <a>a section</a></p>`
	got, dropped := unlinkUnvouchedAnchors(in, allowed)
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	// The notes say ".../guide/" and ".../faq#fees": ".../guide" (no slash) and ".../faq#invented"
	// are addresses they do not carry.
	if dropped != 5 {
		t.Errorf("dropped = %d, want 5 (the invented path, the other site, the anchor with no address, the slashless guide and the invented fragment)", dropped)
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
	// The handler runs on the server's goroutine, so the prompt it saw crosses under a lock.
	var promptMu sync.Mutex
	var seenPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		promptMu.Lock()
		seenPrompt = string(raw)
		promptMu.Unlock()
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
	promptMu.Lock()
	prompt := seenPrompt
	promptMu.Unlock()
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

// TestTrimTrailingLinkPunctuation pins the bracket rule: a ")" that closes one opened inside the
// address is part of it, an unmatched one is the sentence's.
func TestTrimTrailingLinkPunctuation(t *testing.T) {
	cases := map[string]string{
		"https://acme.example/x.":               "https://acme.example/x",
		"https://acme.example/x).":              "https://acme.example/x",
		"https://acme.example/wiki/Foo_(bar)":   "https://acme.example/wiki/Foo_(bar)",
		"https://acme.example/wiki/Foo_(bar)).": "https://acme.example/wiki/Foo_(bar)",
		"https://acme.example/wiki/Foo_(bar),":  "https://acme.example/wiki/Foo_(bar)",
		"https://acme.example/a?b=c!?":          "https://acme.example/a?b=c",
		"":                                      "",
	}
	for in, want := range cases {
		if got := trimTrailingLinkPunctuation(in); got != want {
			t.Errorf("trimTrailingLinkPunctuation(%q) = %q, want %q", in, got, want)
		}
	}
	links := sourceDocumentLinks("Read (https://acme.example/wiki/Foo_(bar)) and https://acme.example/y.")
	for _, want := range []string{"https://acme.example/wiki/Foo_(bar)", "https://acme.example/y"} {
		if _, ok := links[want]; !ok {
			t.Errorf("sourceDocumentLinks lost %q, got %v", want, links)
		}
	}
}
