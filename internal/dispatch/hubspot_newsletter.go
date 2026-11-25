// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// stagedWidgetPrefix opens every widget key this dispatcher's content writers create
// (staging_body, staging_body_2, staging_button, ...; see internal/platform/hubspot/content.go).
const stagedWidgetPrefix = "staging_"

// isStagedWidgetKey reports whether a draft's widget was written by this dispatch rather than
// carried over from the cloned email.
func isStagedWidgetKey(key string) bool { return strings.HasPrefix(key, stagedWidgetPrefix) }

// newsletterStandardFooterWarning tells the operator the one case a newsletter draft does NOT
// carry its brand's own footer.
const newsletterStandardFooterWarning = "The previous issue has no footer this service recognises, so the draft carries the standard Linux Foundation footer instead of the brand's own. Check the footer, the social links and the \"sent by\" line in the HubSpot editor before sending."

// The two things an operator is told when a newsletter draft may not carry its previous issue's
// recipient settings: they could not be read, or HubSpot answered the write with different ones.
const (
	newsletterSendSettingsUnreadWarning  = "The previous issue's recipient settings could not be read, so this draft uses HubSpot's defaults. Check \"Don't send to unengaged contacts\" in the draft's recipient settings before sending."
	newsletterSendSettingsNotKeptWarning = "HubSpot did not keep the previous issue's recipient settings on this draft. Check \"Don't send to unengaged contacts\" and the send frequency cap in the draft's recipient settings before sending."
)

// newsletterSendSettings reads the recipient settings of the issue being cloned, so the new issue
// is staged with them rather than with HubSpot's defaults.
//
// The send list is written as a complete `to` object, and HubSpot fills what that omits with its
// own defaults: every draft this service staged came out with "Don't send to unengaged contacts"
// ON. Each of the five brand newsletters read in the LF portal had been sent with it OFF, so the
// next issue, on the same lists, would have reached fewer subscribers than every issue before it,
// with nothing on the page to say so. Other emails keep the default: their template is some
// earlier send, not a statement about this one.
//
// Best-effort. A failed read stages the draft on the defaults and returns the warning, because
// the draft is still the right email on the right lists.
func newsletterSendSettings(ctx context.Context, client *hubspot.Client, sourceEmailID string) (hubspot.SendSettings, string) {
	settings, err := client.GetEmailSendSettings(ctx, sourceEmailID)
	if err != nil {
		slog.WarnContext(ctx, "could not read the previous newsletter issue's recipient settings; the draft uses HubSpot's defaults",
			"source_email_id", sourceEmailID, "error", err)
		return hubspot.SendSettings{}, newsletterSendSettingsUnreadWarning
	}
	return settings, ""
}

// sendSettingsKept reports whether HubSpot's answer to the send-list write contradicts a setting
// that write stated. A setting the answer does not report counts as kept: it is unknown, and a
// warning on every draft would teach operators to ignore the one that matters.
func sendSettingsKept(stated, reported hubspot.SendSettings) bool {
	differs := func(want, got *bool) bool { return want != nil && got != nil && *want != *got }
	return !differs(stated.SuppressGraymail, reported.SuppressGraymail) && !differs(stated.LimitSendFrequency, reported.LimitSendFrequency)
}

// applyNewsletterContent writes a newsletter issue's copy into its cloned draft and keeps the
// previous issue's header and footer (hubspot.ReplaceEmailBodyKeepingChrome).
//
// ok is false when the clone has no footer to keep. Nothing has been written then, the returned
// warning says the draft will carry the standard footer, and the caller applies the content by
// the full rebuild every other email gets.
//
// BEST-EFFORT otherwise, by the contract applyEmailContentWithHero states: a failed write is
// logged and reported as contentStaged=false, never as a failed dispatch, because the clone is
// already a working draft pointed at the right audience. contentStaged is true only when the
// subject AND the body both landed.
func applyNewsletterContent(ctx context.Context, client *hubspot.Client, emailID string, cfg hubspotConfig) (contentStaged bool, warnings []string, ok bool) {
	_, kept, err := client.ReplaceEmailBodyKeepingChrome(ctx, emailID, hubspot.ReplaceEmailBodyInput{
		BodyHTML:    cfg.BodyHTML,
		ButtonText:  cfg.ButtonText,
		ButtonURL:   cfg.ButtonURL,
		PreviewText: cfg.PreviewText,
	})
	if errors.Is(err, hubspot.ErrNoBrandChrome) {
		slog.WarnContext(ctx, "the newsletter's cloned email has no footer to keep; its content is applied by a full rebuild with the standard footer",
			"email_id", emailID)
		return false, []string{newsletterStandardFooterWarning}, false
	}

	subjectStaged := patchEmailSubject(ctx, client, emailID, cfg.Subject)
	if err != nil {
		// The same two failures applyEmailContentWithHero tells apart: a write HubSpot accepted
		// and reverted leaves a draft nobody can describe, and is an ERROR; anything else leaves
		// the previous issue's content in place, which is wrong but coherent.
		if errors.Is(err, hubspot.ErrContentNotPersisted) {
			slog.ErrorContext(ctx, "the newsletter draft's content was accepted but not persisted; the draft is left in an inconsistent state and needs manual repair",
				"email_id", emailID, "error", err)
		} else {
			slog.WarnContext(ctx, "could not write the newsletter issue into its draft; it keeps the previous issue's content",
				"email_id", emailID, "error", err)
		}
		return false, []string{"The issue's content could not be written into the draft, which may still hold the previous issue's content. Check the draft in the HubSpot editor, or delete it there and stage again."}, true
	}
	slog.InfoContext(ctx, "wrote the newsletter issue between the previous issue's header and footer",
		"email_id", emailID, "content_modules", kept.ContentModules,
		"header_sections", kept.HeaderSections, "footer_sections", kept.FooterSections)

	if kept.HeaderImage != nil && headerLooksIssueSpecific(*kept.HeaderImage) {
		warnings = append(warnings, "The header image was kept from the previous issue and looks specific to it ("+headerImageLabel(*kept.HeaderImage)+"). Replace it in the HubSpot editor if this issue needs its own, and check where it links.")
	}
	return subjectStaged, warnings, true
}

// patchEmailSubject sets a draft's subject line and reports whether it landed. A blank subject is
// not written and is not staged. Best-effort: a failed write is logged, not returned.
func patchEmailSubject(ctx context.Context, client *hubspot.Client, emailID, subject string) bool {
	if subject = strings.TrimSpace(subject); subject == "" {
		return false
	}
	if _, err := client.PatchEmailSettings(ctx, emailID, hubspot.EmailSettings{Subject: &subject}); err != nil {
		slog.WarnContext(ctx, "could not set the generated subject on the email draft; it keeps the template's subject",
			"email_id", emailID, "error", err)
		return false
	}
	return true
}

// monthName matches a month spelled out or abbreviated, standing apart from other letters. "may"
// is left out: as a word it says nothing about a date.
var monthName = regexp.MustCompile(`(?i)(^|[^a-z])(january|february|march|april|june|july|august|september|october|november|december|jan|feb|mar|apr|jun|jul|aug|sept|sep|oct|nov|dec)([^a-z]|$)`)

// fourDigitYear matches a year of this century standing apart from other digits.
var fourDigitYear = regexp.MustCompile(`(^|[^0-9])20[0-9]{2}([^0-9]|$)`)

// uploadTimestamp is the suffix HubSpot's file manager gives a pasted image, e.g.
// "undefined-Nov-17-2026-05-00-21-5285-PM". It dates the UPLOAD, not what the image shows.
var uploadTimestamp = regexp.MustCompile(`(?i)-?(jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)-[0-9]{1,2}-20[0-9]{2}-[0-9]{2}-[0-9]{2}-[0-9]{2}-[0-9]+-(am|pm)`)

// headerLooksIssueSpecific reports whether a kept header image names a month or a year in its
// alt text, its file name or its link: a monthly banner ("…_09-November2026_main.png", linking to
// that month's post) rather than the brand's logo. It decides only whether the operator is told
// to look; the image is kept either way, because a wrong guess must not drop a brand's logo.
func headerLooksIssueSpecific(img hubspot.HeaderImage) bool {
	file := img.Src
	if u, err := url.Parse(img.Src); err == nil && u.Path != "" {
		file = path.Base(u.Path)
	}
	if decoded, err := url.PathUnescape(file); err == nil {
		file = decoded
	}
	file = uploadTimestamp.ReplaceAllString(file, "")
	link := img.Link
	if u, err := url.Parse(img.Link); err == nil {
		link = u.Path
	}
	for _, s := range []string{img.Alt, file, link} {
		s = uploadTimestamp.ReplaceAllString(s, "")
		if monthName.MatchString(s) || fourDigitYear.MatchString(s) {
			return true
		}
	}
	return false
}

// headerImageLabel names the header image for the warning: its alt text, else its file name.
// Bounded and stripped of control characters, because the warning is persisted and shown.
func headerImageLabel(img hubspot.HeaderImage) string {
	label := strings.TrimSpace(img.Alt)
	if label == "" {
		if u, err := url.Parse(img.Src); err == nil && u.Path != "" {
			label = path.Base(u.Path)
			if decoded, derr := url.PathUnescape(label); derr == nil {
				label = decoded
			}
		}
	}
	label = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, label)), " ")
	const maxLabelRunes = 80
	if utf8.RuneCountInString(label) > maxLabelRunes {
		label = string([]rune(label)[:maxLabelRunes]) + "…"
	}
	if label == "" {
		return "no description"
	}
	return `"` + label + `"`
}
