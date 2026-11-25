// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// newsletterBrief is one issue of a brand's newsletter, as the newsletter surface saves it.
func newsletterBrief() *model.CampaignBrief {
	b := testBrief()
	b.DeliveryType = model.DeliveryNewsletter
	b.EventSlug = "acme-newsletter-november-2026"
	b.EventDetails = json.RawMessage(`{"name":"Acme Newsletter - November 2026","sourceText":"notes"}`)
	return b
}

// seedNewsletter makes the fake portal's cloned draft a brand newsletter: the brand's logo, last
// issue's item, then the brand's own footer run (social links, a "sent by" line with a link, the
// footer module). headerAlt names the logo.
func seedNewsletter(rec *hubspotRec, headerAlt string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.widgets = map[string]map[string]any{
		"logo": {"body": map[string]any{"module_id": 1367093, "link": "https://acme.example/",
			"img": map[string]any{"src": "https://f.example/hubfs/acme-logo.png", "alt": headerAlt}}},
		"old_item": richModule(`<h2>October news</h2><p><a href="https://acme.example/october">Last month</a></p>`),
		"social": {"body": map[string]any{"path": "@hubspot/follow_me_email", "module_id": 2763545,
			"social": []any{map[string]any{"network": "linkedin", "url": "https://www.linkedin.com/company/acme-example/"}}}},
		"sent_by":      richModule(`<p>This email was sent by: Acme Foundation. <a href="https://acme.example/preferences">Manage preferences</a></p>`),
		"footer":       {"body": map[string]any{"path": "/email_footer no links", "module_id": 40514205881}},
		"preview_text": {"body": map[string]any{"value": "October preview"}},
	}
	rec.layout = []string{"logo", "old_item", "social", "sent_by", "footer"}
	// The previous issue went to every subscriber on its list, unengaged ones included, as each
	// brand newsletter read in the LF portal did.
	rec.sourceEmailTo = map[string]any{
		"contactIlsLists":  map[string]any{"include": []any{"26724"}, "exclude": []any{}},
		"suppressGraymail": false,
	}
}

// sendListTo is the `to` object the dispatch wrote on the clone.
func sendListTo(t *testing.T, rec *hubspotRec) map[string]any {
	t.Helper()
	to, _ := rec.SendListBody()["to"].(map[string]any)
	if to == nil {
		t.Fatalf("no send-list write reached the portal: %v", rec.SendListBody())
	}
	return to
}

// draftState is the fake draft as it stands: the keys its layout places and its rich-text bodies.
func draftState(rec *hubspotRec) ([]string, map[string]string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.layout...), rec.richBodies()
}

func resultWarnings(t *testing.T, camp *model.Campaign) []string {
	t.Helper()
	if camp == nil {
		t.Fatal("no campaign returned")
	}
	var blob struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(camp.Result, &blob); err != nil {
		t.Fatalf("result blob is not valid JSON: %v", err)
	}
	return blob.Warnings
}

const newsletterCfg = `{"hubspotConfig":{"sourceEmailId":"555","subject":"Acme Newsletter: November","previewText":"November preview",` +
	`"bodyHtml":"<h1 style=\"color:#0f172a;\">Acme Newsletter: November</h1><p style=\"margin:0;\">Lead-in.</p>` +
	`<h2>First item</h2><p>About it. <a href=\"https://acme.example/one\" style=\"color:#2563eb;\">Read it</a></p>"}}`

// TestHubSpot_NewsletterKeepsThePreviousIssuesHeaderAndFooter is the defect this path exists for:
// a newsletter issue staged by the full rebuild lost its brand's footer to the standard Linux
// Foundation Events one, social links and "sent by" line included.
func TestHubSpot_NewsletterKeepsThePreviousIssuesHeaderAndFooter(t *testing.T) {
	srv, rec := hubspotServer(t)
	seedNewsletter(rec, "Acme Foundation")
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	camp, err := d.Dispatch(context.Background(), newsletterBrief(), model.ProviderHubSpot, json.RawMessage(newsletterCfg))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !resultContentStaged(t, camp) {
		t.Error("result contentStaged = false, want true once the subject and the issue both landed")
	}
	if w := resultWarnings(t, camp); len(w) != 0 {
		t.Errorf("a brand logo and a kept footer need no warning, got %v", w)
	}

	layout, bodies := draftState(rec)
	if got, want := strings.Join(layout, ","), "logo,staging_body,staging_body_2,social,sent_by,footer"; got != want {
		t.Fatalf("layout = %s\nwant     %s", got, want)
	}
	if _, stale := bodies["old_item"]; stale {
		t.Error("last issue's item is still in the draft")
	}
	if strings.Contains(strings.Join([]string{bodies["staging_body"], bodies["staging_body_2"]}, ""), "style=") {
		t.Errorf("the issue must take the draft's own styles, got inline styles: %v", bodies)
	}
	if !strings.Contains(bodies["staging_body"], "Acme Newsletter: November") {
		t.Errorf("first module = %q, want the issue's title", bodies["staging_body"])
	}

	// A newsletter's links are staged exactly as the operator wrote them: the events convention
	// (utm_medium=LF-Events) would file a brand's newsletter clicks under LF Events.
	if strings.Contains(bodies["staging_body_2"], "utm_") || !strings.Contains(bodies["staging_body_2"], `href="https://acme.example/one"`) {
		t.Errorf("the issue's link must be staged as written, untagged: %q", bodies["staging_body_2"])
	}
	if bodies["sent_by"] != `<p>This email was sent by: Acme Foundation. <a href="https://acme.example/preferences">Manage preferences</a></p>` {
		t.Errorf("the brand's footer text changed: %q", bodies["sent_by"])
	}
	if subject, _ := rec.snapshotContent(); subject != "Acme Newsletter: November" {
		t.Errorf("subject = %q", subject)
	}
}

// TestHubSpot_NewsletterWithANamedCampaignTagsOnlyItsOwnLinks: an explicit utmCampaign is a
// deliberate request and is honoured, on the modules this dispatch wrote and no others. The
// brand's footer link is the previous issue's own and is not this send's to rewrite.
func TestHubSpot_NewsletterWithANamedCampaignTagsOnlyItsOwnLinks(t *testing.T) {
	srv, rec := hubspotServer(t)
	seedNewsletter(rec, "Acme Foundation")
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	cfg := strings.Replace(newsletterCfg, `"sourceEmailId":"555",`, `"sourceEmailId":"555","utmCampaign":"acme-november",`, 1)
	if _, err := d.Dispatch(context.Background(), newsletterBrief(), model.ProviderHubSpot, json.RawMessage(cfg)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	_, bodies := draftState(rec)
	if !strings.Contains(bodies["staging_body_2"], "utm_campaign=acme-november") {
		t.Errorf("the issue's link was not tagged with the named campaign: %q", bodies["staging_body_2"])
	}
	if strings.Contains(bodies["sent_by"], "utm_") {
		t.Errorf("the brand's footer link was rewritten: %q", bodies["sent_by"])
	}
}

// TestHubSpot_AnEventEmailStillGetsTheFullRebuild pins the scope: the same cloned layout under an
// EVENT brief is rebuilt whole, because there the clone source may be another event's email and
// everything it carried is stale.
func TestHubSpot_AnEventEmailStillGetsTheFullRebuild(t *testing.T) {
	srv, rec := hubspotServer(t)
	seedNewsletter(rec, "Acme Foundation")
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot, json.RawMessage(newsletterCfg)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	layout, _ := draftState(rec)
	joined := strings.Join(layout, ",")
	if strings.Contains(joined, "sent_by") || strings.Contains(joined, "logo") || !strings.Contains(joined, "staging_footer_hs") {
		t.Errorf("an event email must be rebuilt whole with the standard footer, got layout %s", joined)
	}
}

// TestHubSpot_NewsletterWithoutAFooterFallsBackAndSaysSo: a clone with no footer modules cannot
// keep one, so the issue is applied by the full rebuild and the operator is told the draft
// carries the standard footer.
func TestHubSpot_NewsletterWithoutAFooterFallsBackAndSaysSo(t *testing.T) {
	srv, rec := hubspotServer(t) // the default seed: one text block, no footer modules
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	camp, err := d.Dispatch(context.Background(), newsletterBrief(), model.ProviderHubSpot, json.RawMessage(newsletterCfg))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	layout, bodies := draftState(rec)
	if !strings.Contains(strings.Join(layout, ","), "staging_footer_hs") || !strings.Contains(bodies["staging_body"], "First item") {
		t.Errorf("the issue must still be applied, by the full rebuild; layout %v", layout)
	}
	warnings := resultWarnings(t, camp)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "standard Linux Foundation footer") {
		t.Errorf("warnings = %v, want the standard-footer warning", warnings)
	}
}

// TestHubSpot_NewsletterWarnsWhenTheKeptHeaderLooksIssueSpecific: a monthly banner is kept like a
// logo, because a wrong guess must not drop a brand's header, but the operator is told to look.
func TestHubSpot_NewsletterWarnsWhenTheKeptHeaderLooksIssueSpecific(t *testing.T) {
	srv, rec := hubspotServer(t)
	seedNewsletter(rec, "Acme Newsletter banner October 2026")
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	camp, err := d.Dispatch(context.Background(), newsletterBrief(), model.ProviderHubSpot, json.RawMessage(newsletterCfg))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	warnings := resultWarnings(t, camp)
	if len(warnings) != 1 || !strings.Contains(warnings[0], `"Acme Newsletter banner October 2026"`) {
		t.Errorf("warnings = %v, want one naming the kept header image", warnings)
	}
	if layout, _ := draftState(rec); layout[0] != "logo" {
		t.Errorf("the header image must be kept either way, layout %v", layout)
	}
}

// TestHubSpot_NewsletterRefusesAnABVariantUnderAnotherFooter: the variant's content would go
// through the full rebuild, so a direct request for one is declined with a reason.
func TestHubSpot_NewsletterRefusesAnABVariantUnderAnotherFooter(t *testing.T) {
	srv, rec := hubspotServer(t)
	seedNewsletter(rec, "Acme Foundation")
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	cfg := strings.Replace(newsletterCfg, `"sourceEmailId":"555",`, `"sourceEmailId":"555","abTestEnabled":true,"subjectB":"B","bodyHtmlB":"<p>B</p>",`, 1)
	camp, err := d.Dispatch(context.Background(), newsletterBrief(), model.ProviderHubSpot, json.RawMessage(cfg))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if rec.SawCreateVariation() {
		t.Error("no variant may be created for a newsletter that kept its footer")
	}
	if w := resultWarnings(t, camp); len(w) != 1 || !strings.Contains(w[0], "No A/B variant") {
		t.Errorf("warnings = %v, want the A/B refusal", w)
	}
}

// TestHubSpot_NewsletterKeepsThePreviousIssuesRecipientSettings is the defect: the send list is
// written as a complete `to` object, HubSpot fills what it omits with its defaults, and so every
// staged draft had "Don't send to unengaged contacts" on. The brands send their newsletters with
// it off, so the next issue would have reached fewer subscribers than the one before it.
func TestHubSpot_NewsletterKeepsThePreviousIssuesRecipientSettings(t *testing.T) {
	srv, rec := hubspotServer(t)
	seedNewsletter(rec, "Acme Foundation")
	rec.sourceEmailTo["limitSendFrequency"] = true
	// A portal that reports what it ended up with, in agreement with what was stated.
	rec.sendListAnswer = `{"id":"999","state":"DRAFT","to":{"suppressGraymail":false,"limitSendFrequency":true}}`
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", []string{"111"})}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	camp, err := d.Dispatch(context.Background(), newsletterBrief(), model.ProviderHubSpot, json.RawMessage(newsletterCfg))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	to := sendListTo(t, rec)
	if got, stated := to["suppressGraymail"].(bool); !stated || got {
		t.Errorf("to.suppressGraymail = %v (stated %v), want the previous issue's false", to["suppressGraymail"], stated)
	}
	if got, stated := to["limitSendFrequency"].(bool); !stated || !got {
		t.Errorf("to.limitSendFrequency = %v (stated %v), want the previous issue's true", to["limitSendFrequency"], stated)
	}
	// The same write still carries the lists: a second one naming only the settings would be a
	// second complete `to`, with no recipients in it.
	ils, _ := to["contactIlsLists"].(map[string]any)
	if include, _ := ils["include"].([]any); len(include) != 1 || include[0] != "26724" {
		t.Errorf("the send list must travel in the same write, got %v", to["contactIlsLists"])
	}
	if w := resultWarnings(t, camp); len(w) != 0 {
		t.Errorf("settings that were read and kept need no warning, got %v", w)
	}
}

// TestHubSpot_AnEventEmailLeavesTheRecipientSettingsToHubSpot pins the scope: an event email's
// template is some earlier send, so its settings say nothing about this one. They are not read
// and not stated, exactly as before.
func TestHubSpot_AnEventEmailLeavesTheRecipientSettingsToHubSpot(t *testing.T) {
	srv, rec := hubspotServer(t)
	seedNewsletter(rec, "Acme Foundation")
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	if _, err := d.Dispatch(context.Background(), testBrief(), model.ProviderHubSpot, json.RawMessage(newsletterCfg)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	to := sendListTo(t, rec)
	for _, key := range []string{"suppressGraymail", "limitSendFrequency"} {
		if _, stated := to[key]; stated {
			t.Errorf("an event email must not state %s, got %v", key, to[key])
		}
	}
	if gets := rec.SourceEmailGets(); gets != 0 {
		t.Errorf("an event email's template settings must not be read at dispatch, saw %d read(s)", gets)
	}
}

// TestHubSpot_NewsletterStatesNoSettingThePreviousIssueDoesNotReport: an email that reports no
// settings leaves nothing to keep, which is not a failure and warrants no warning.
func TestHubSpot_NewsletterStatesNoSettingThePreviousIssueDoesNotReport(t *testing.T) {
	srv, rec := hubspotServer(t)
	seedNewsletter(rec, "Acme Foundation")
	rec.sourceEmailTo = nil
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	camp, err := d.Dispatch(context.Background(), newsletterBrief(), model.ProviderHubSpot, json.RawMessage(newsletterCfg))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if _, stated := sendListTo(t, rec)["suppressGraymail"]; stated {
		t.Error("a setting the previous issue does not report must not be invented")
	}
	if w := resultWarnings(t, camp); len(w) != 0 {
		t.Errorf("nothing to keep is not a warning, got %v", w)
	}
}

// TestHubSpot_NewsletterSaysWhenThePreviousIssuesSettingsCannotBeRead: the draft is still the
// right email on the right lists, so it is staged, on HubSpot's defaults, and the operator is
// told which setting to check.
func TestHubSpot_NewsletterSaysWhenThePreviousIssuesSettingsCannotBeRead(t *testing.T) {
	srv, rec := hubspotServer(t)
	seedNewsletter(rec, "Acme Foundation")
	rec.sourceEmailStatus = http.StatusNotFound
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	camp, err := d.Dispatch(context.Background(), newsletterBrief(), model.ProviderHubSpot, json.RawMessage(newsletterCfg))
	if err != nil {
		t.Fatalf("a failed settings read must not fail the stage: %v", err)
	}
	if _, stated := sendListTo(t, rec)["suppressGraymail"]; stated {
		t.Error("an unread setting must not be guessed")
	}
	if !resultContentStaged(t, camp) {
		t.Error("the issue must still be written into the draft")
	}
	warnings := resultWarnings(t, camp)
	if len(warnings) != 1 || warnings[0] != newsletterSendSettingsUnreadWarning {
		t.Errorf("warnings = %v, want the could-not-be-read warning", warnings)
	}
}

// TestHubSpot_NewsletterSaysWhenHubSpotDoesNotKeepTheSettings: stating a setting is not the same
// as it holding. A portal that answers the write with a different one is reported, not trusted.
func TestHubSpot_NewsletterSaysWhenHubSpotDoesNotKeepTheSettings(t *testing.T) {
	srv, rec := hubspotServer(t)
	seedNewsletter(rec, "Acme Foundation")
	rec.sendListAnswer = `{"id":"999","state":"DRAFT","to":{"suppressGraymail":true}}`
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))

	camp, err := d.Dispatch(context.Background(), newsletterBrief(), model.ProviderHubSpot, json.RawMessage(newsletterCfg))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got, stated := sendListTo(t, rec)["suppressGraymail"].(bool); !stated || got {
		t.Errorf("the previous issue's false must still have been stated, got %v", sendListTo(t, rec)["suppressGraymail"])
	}
	warnings := resultWarnings(t, camp)
	if len(warnings) != 1 || warnings[0] != newsletterSendSettingsNotKeptWarning {
		t.Errorf("warnings = %v, want the not-kept warning", warnings)
	}
}

func TestSendSettingsKept(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name             string
		stated, reported hubspot.SendSettings
		want             bool
	}{
		{"nothing stated", hubspot.SendSettings{}, hubspot.SendSettings{SuppressGraymail: &yes}, true},
		{"stated and echoed", hubspot.SendSettings{SuppressGraymail: &no}, hubspot.SendSettings{SuppressGraymail: &no}, true},
		{"stated, answer silent", hubspot.SendSettings{SuppressGraymail: &no}, hubspot.SendSettings{}, true},
		{"unengaged suppression flipped", hubspot.SendSettings{SuppressGraymail: &no}, hubspot.SendSettings{SuppressGraymail: &yes}, false},
		{"frequency cap flipped", hubspot.SendSettings{LimitSendFrequency: &yes}, hubspot.SendSettings{LimitSendFrequency: &no}, false},
		{"one kept, one flipped", hubspot.SendSettings{SuppressGraymail: &no, LimitSendFrequency: &no}, hubspot.SendSettings{SuppressGraymail: &no, LimitSendFrequency: &yes}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sendSettingsKept(tc.stated, tc.reported); got != tc.want {
				t.Errorf("sendSettingsKept = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHeaderLooksIssueSpecific(t *testing.T) {
	cases := []struct {
		name string
		img  hubspot.HeaderImage
		want bool
	}{
		{"a brand logo", hubspot.HeaderImage{Src: "https://f.example/hubfs/1/acme-horizontal-color.png", Alt: "Acme Newsletter", Link: "https://acme.example/"}, false},
		{"a monthly banner by file name", hubspot.HeaderImage{Src: "https://f.example/hubfs/1/Newsletter_Graphics_09-November2026_main.png"}, true},
		{"a banner linking to that month's post", hubspot.HeaderImage{Src: "https://f.example/hubfs/1/banner.png", Link: "https://acme.example/blog/newsletter-november-2026"}, true},
		{"a year in the alt text", hubspot.HeaderImage{Src: "https://f.example/hubfs/1/banner.png", Alt: "State of the project 2026"}, true},
		{"an upload timestamp is not a date", hubspot.HeaderImage{Src: "https://f.example/hubfs/1/undefined-Nov-17-2026-05-00-21-5285-PM.png", Alt: "Acme"}, false},
		{"a pixel size is not a year", hubspot.HeaderImage{Src: "https://f.example/hubfs/1/Header%20(2400%20x%20600%20px).png", Alt: "Header (2400 x 600 px)"}, false},
		{"may is a word", hubspot.HeaderImage{Src: "https://f.example/hubfs/1/logo.png", Alt: "Whatever may come"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := headerLooksIssueSpecific(tc.img); got != tc.want {
				t.Errorf("headerLooksIssueSpecific(%+v) = %v, want %v", tc.img, got, tc.want)
			}
		})
	}
}

func TestHeaderImageLabel(t *testing.T) {
	if got := headerImageLabel(hubspot.HeaderImage{Alt: " Acme\nbanner "}); got != `"Acme banner"` {
		t.Errorf("alt label = %s", got)
	}
	if got := headerImageLabel(hubspot.HeaderImage{Src: "https://f.example/hubfs/1/My%20Banner.png"}); got != `"My Banner.png"` {
		t.Errorf("file label = %s", got)
	}
	if got := headerImageLabel(hubspot.HeaderImage{Alt: strings.Repeat("x", 200)}); len([]rune(got)) != 83 {
		t.Errorf("a long label must be cut to 80 characters plus an ellipsis and quotes, got %d runes", len([]rune(got)))
	}
	if got := headerImageLabel(hubspot.HeaderImage{}); got != "no description" {
		t.Errorf("empty label = %s", got)
	}
}
