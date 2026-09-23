// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Hard-coded HubSpot drag-and-drop module ids. These are NOT arbitrary — they were
// reverse-engineered from real, published Linux Foundation marketing emails on this
// portal (see the reference prototype's backend/integrations/hubspot.py,
// update_email_content) and must be used verbatim: an email PATCHed with the wrong
// module_id for a given widget silently fails to render in HubSpot's editor.
const (
	moduleIDImage        = 1367093
	moduleIDRichText     = 1155639
	moduleIDButton       = 1976948
	moduleIDEmailDivider = 2191110
	moduleIDFollowMe     = 2763545
	moduleIDEmailFooter  = 2869621
)

const templatePathStartFromScratch = "@hubspot/email/dnd/Start_from_scratch.html"

const defaultSentByOrg = "The Linux Foundation Events"

// Sponsor is one sponsor logo to render in a rebuilt email body.
type Sponsor struct {
	Name    string
	LogoURL string
}

// RebuildEmailContentInput carries everything RebuildEmailContent needs to replace
// an email draft's ENTIRE content. Every widget the clone source carried over is
// discarded, not merged with — this is what makes a "fresh" rebuild possible.
type RebuildEmailContentInput struct {
	// HeroImageURL is an already HubSpot-hosted hero/banner image (see UploadImage).
	// Rendered as the FIRST section of the email, ahead of everything else. Empty
	// skips the hero section entirely, replacing any hero the clone source had.
	HeroImageURL string
	// HeroLinkURL is where the hero image links when clicked (e.g. the event page).
	// Empty makes the image non-clickable.
	HeroLinkURL string
	// BodyHTML is the generated email body, rendered as a single rich-text section
	// immediately after the hero.
	BodyHTML string
	// ButtonText is the call-to-action button's label (e.g. "Register Now"). Rendered as a
	// native, centered HubSpot button module immediately after the body, instead of relying on
	// BodyHTML to embed a CTA as raw HTML (a hand-styled <table>/<a> hack does not render as a
	// real button in HubSpot's editor and is what this module replaces). Empty ButtonURL skips
	// the button section entirely; ButtonText defaults to "Register Now" when ButtonURL is set
	// but ButtonText is empty.
	ButtonText string
	// ButtonURL is the CTA button's destination. Empty skips the button section entirely.
	ButtonURL string
	// Sponsors renders as up to two logo tiers (5 logos each, split 3+2 across rows)
	// after the body, preceded by a "Thank You to Our Sponsors!" heading. Only
	// sponsors with a LogoURL are rendered; entries past the first 10 are dropped.
	Sponsors []Sponsor
	// SentByOrg names the sending organization in the footer's "This email was
	// sent by: <org>" line. Defaults to "The Linux Foundation Events" when empty.
	SentByOrg string
	// PreviewText is the inbox preheader shown next to the subject line. Written into
	// the draft's preview_text widget (the only place a DnD email stores it — Marketing
	// Emails v3 exposes no separate preheader property). Empty preserves whatever
	// preview_text widget the clone source already had, since that's usually more
	// useful than showing no preheader at all.
	PreviewText string
}

// RebuildEmailContent replaces an email draft's entire widget/section tree: hero
// image first (if provided), then the body, then sponsor tiers, then a fixed
// footer chain (divider, follow-us header, social icons, sent-by line, native
// HubSpot footer). MUTATING, non-idempotent.
//
// This targets the /draft sub-route via patchEmail, consistent with this package's
// existing SetEmailHTMLWidgets convention, rather than the base /{id} route the
// reference prototype uses — /draft is what the rest of this file already relies
// on for staging content edits without mutating the live email.
//
// HubSpot does not merge `content` on a drag-and-drop email: a partial widget PATCH
// destroys every widget not included in it. That is deliberately exploited here to
// wipe all widgets the clone source carried over.
func (c *Client) RebuildEmailContent(ctx context.Context, id string, in RebuildEmailContentInput) (*Email, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("hubspot: RebuildEmailContent requires a non-empty id")
	}

	current, err := c.readDraftContent(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("hubspot: read email %s draft before rebuilding its content: %w", id, err)
	}

	flexAreaName := "main"
	var currentFlex map[string]json.RawMessage
	if uerr := json.Unmarshal(current.Content["flexAreas"], &currentFlex); uerr == nil {
		for name := range currentFlex {
			flexAreaName = name
			break
		}
	}

	var currentWidgets map[string]json.RawMessage
	_ = json.Unmarshal(current.Content["widgets"], &currentWidgets)
	previewText, hasPreview := currentWidgets["preview_text"]

	widgets := map[string]any{}
	var sections []map[string]any

	if strings.TrimSpace(in.HeroImageURL) != "" {
		addHeroSection(widgets, &sections, in.HeroImageURL, in.HeroLinkURL)
	}

	// Conditional, and the clone's body is NOT carried forward — it is dropped from the tree.
	//
	// Stated plainly because an earlier version of this comment claimed the opposite and the code
	// never did it. This rebuild replaces the entire widget tree, and both shapes lose the body
	// when the caller supplies none: an unconditional call writes an empty staging_body over it,
	// and this conditional omits the section entirely. Carrying it forward the way preview_text
	// is carried below does not work either, because a cloned template's body lives under its own
	// widget keys (module_1, module_hdr, ...), not under the staging_body key this rebuild
	// invents — so there is no single key to copy.
	//
	// Reachable today whenever a hero-only, button-only or sponsors-only change is dispatched:
	// applyEmailContentWithHero skips the call only when body, hero, button AND sponsors are all
	// empty, so those requests do reach here with an empty body and lose the clone's body.
	// Closing that needs a content path that edits widgets in place rather than replacing them;
	// tracked with the rest of LFXV2-2775.
	if strings.TrimSpace(in.BodyHTML) != "" {
		addBodySection(widgets, &sections, in.BodyHTML)
	}

	if strings.TrimSpace(in.ButtonURL) != "" {
		addButtonSection(widgets, &sections, in.ButtonText, in.ButtonURL)
	}

	addSponsorSections(widgets, &sections, in.Sponsors)

	addFooterSections(widgets, &sections, in.SentByOrg)

	if text := strings.TrimSpace(in.PreviewText); text != "" {
		widgets["preview_text"] = map[string]any{"body": map[string]any{"value": text}}
	} else if hasPreview {
		widgets["preview_text"] = previewText
	}

	newContent := map[string]any{
		"templatePath": templatePathStartFromScratch,
		"widgets":      widgets,
		"flexAreas": map[string]any{
			flexAreaName: map[string]any{
				"boxed":                   false,
				"isSingleColumnFullWidth": false,
				"sections":                sections,
			},
		},
	}
	if raw := current.Content["styleSettings"]; len(raw) > 0 {
		newContent["styleSettings"] = raw
	}

	email, err := c.patchEmail(ctx, id, map[string]any{"content": newContent})
	if err != nil {
		return nil, err
	}

	if verr := c.verifyContentSaved(ctx, id, widgets); verr != nil {
		// Only a PROVEN revert carries the typed error. A verification READ that failed --
		// timeout, malformed JSON -- leaves the outcome unknown and retryable, and tagging it
		// ErrContentNotPersisted sent the caller's ERROR branch (which says "needs manual
		// repair") for a draft that may be perfectly correct.
		if errors.Is(verr, errVerifyReadFailed) {
			return email, verr
		}
		return email, fmt.Errorf("%w: %w", ErrContentNotPersisted, verr)
	}
	return email, nil
}

// ErrContentNotPersisted marks the one rebuild failure a caller CANNOT treat as
// "the draft kept the template's content": the content PATCH returned 2xx but the
// re-read shows the widgets are not referenced, i.e. HubSpot accepted and silently
// reverted the write (see verifyContentSaved).
//
// It is worth distinguishing because the two failures need opposite responses. A
// failed PATCH leaves the clone intact and is recoverable by retrying; a reverted
// one leaves a draft whose state matches neither the template nor the generated
// copy, and no retry of the same payload is known to fix it. Callers that swallow
// rebuild errors as best-effort should still surface THIS one.
var ErrContentNotPersisted = errors.New("hubspot: email content was accepted but not persisted")

// errVerifyReadFailed marks the OTHER verification outcome: the re-read itself failed, so
// whether the write persisted is unknown. Separate from ErrContentNotPersisted because the two
// need opposite responses — this one is retryable, that one is not.
var errVerifyReadFailed = errors.New("hubspot: could not verify email content")

// rawEmailContent decodes only the `content` object one level deep, leaving each
// key (widgets/flexAreas/styleSettings/templatePath) as raw JSON — widget shapes
// vary per template, so a fully-typed decode isn't possible here (mirrors the
// approach emailContent already uses elsewhere in this package).
type rawEmailContent struct {
	Content map[string]json.RawMessage `json:"content"`
}

// readDraftContent GETs the email's draft content. It reads /draft (not the base
// /{id} route) to stay consistent with GetEmailHTMLWidgets/SetEmailHTMLWidgets.
func (c *Client) readDraftContent(ctx context.Context, id string) (rawEmailContent, error) {
	raw, err := c.doRequest(ctx, http.MethodGet, emailsPath+"/"+url.PathEscape(id)+"/draft", nil, true)
	if err != nil {
		return rawEmailContent{}, err
	}
	var doc rawEmailContent
	if uerr := json.Unmarshal(raw, &doc); uerr != nil {
		return rawEmailContent{}, fmt.Errorf("hubspot: decode email %s draft content: %w", id, uerr)
	}
	return doc, nil
}

// verifyContentSaved re-fetches the draft and checks that EVERY widget we just wrote is
// referenced from flexAreas. HubSpot has, in practice, accepted a content PATCH with a 2xx and
// silently reverted it; this mirrors the reference prototype's inline post-write verification
// rather than trusting the PATCH response alone.
//
// It required only ONE match until 2026-09-18. The widget keys are fixed, so a draft that kept a
// single key from an earlier generation confirmed a rebuild whose new body and layout were lost
// — a partial apply, which is precisely what this exists to catch and the one thing it could not
// see. preview_text is excluded because it is carried over from the clone rather than written
// here when the caller supplies none.
//
// Two distinct failures come out: a proven revert (wrapped as ErrContentNotPersisted by the
// caller, unrecoverable) and a failed verification READ (errVerifyReadFailed, retryable, because
// the write's outcome is then unknown rather than known-bad).
func (c *Client) verifyContentSaved(ctx context.Context, id string, ourWidgets map[string]any) error {
	doc, err := c.readDraftContent(ctx, id)
	if err != nil {
		// NOT ErrContentNotPersisted. The verification READ failed -- timeout, malformed JSON --
		// so whether the write persisted is UNKNOWN, which is a different state from "HubSpot
		// accepted and reverted it". Reporting it as the latter would send an operator to
		// manually repair a draft that may be perfectly correct, and the caller raises that case
		// to ERROR precisely because it is unrecoverable. This one is retryable.
		return fmt.Errorf("%w: email %s: %w", errVerifyReadFailed, id, err)
	}

	var savedFlex map[string]struct {
		Sections []struct {
			Columns []struct {
				Widgets []string `json:"widgets"`
			} `json:"columns"`
		} `json:"sections"`
	}
	_ = json.Unmarshal(doc.Content["flexAreas"], &savedFlex)

	saved := map[string]bool{}
	for _, area := range savedFlex {
		for _, sec := range area.Sections {
			for _, col := range sec.Columns {
				for _, w := range col.Widgets {
					saved[w] = true
				}
			}
		}
	}

	// EVERY widget we wrote, not merely one. Returning on the first match confirmed the whole
	// rebuild from a single surviving key -- and the keys are fixed (`staging_body`, the footer
	// pair), so a draft that kept ONE of them from an earlier generation passed while the new
	// body and layout were lost. A partial apply is the failure mode this check exists to catch,
	// and it was the one it could not see.
	//
	// preview_text is excluded because it is carried over from the clone rather than written by
	// this rebuild when the caller supplies none.
	var missing []string
	for key := range ourWidgets {
		if key == "preview_text" {
			continue
		}
		if !saved[key] {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing) // deterministic message; map iteration order is not
		return fmt.Errorf("hubspot: email %s content PATCH did not persist %d of the rebuilt widgets (%s) (HubSpot may have silently reverted it)",
			id, len(missing), strings.Join(missing, ", "))
	}
	return nil
}

var defaultSectionStyle = map[string]any{
	"backgroundType": "CONTENT",
	"breakpointStyles": map[string]any{
		"default": map[string]any{"backgroundType": "CONTENT"},
	},
}

var bannerSectionStyle = map[string]any{
	"backgroundImageType": "REPEAT",
	"backgroundType":      "CONTENT",
	"breakpointStyles": map[string]any{
		"default": map[string]any{"backgroundImageType": "REPEAT", "backgroundType": "CONTENT"},
		"mobile":  map[string]any{},
	},
	"paddingBottom": "0px",
	"paddingTop":    "0px",
}

type widgetColumn struct {
	id      string
	widgets []string
	width   int
}

func makeSection(id string, cols []widgetColumn, style map[string]any) map[string]any {
	colsJSON := make([]map[string]any, 0, len(cols))
	for _, c := range cols {
		colsJSON = append(colsJSON, map[string]any{
			"id":      c.id,
			"widgets": c.widgets,
			"width":   c.width,
		})
	}
	return map[string]any{"id": id, "columns": colsJSON, "style": style}
}

// columnWidths distributes a 12-column grid across n columns as evenly as
// possible, matching the reference prototype's _col_widths helper.
func columnWidths(n int) []int {
	if n <= 0 {
		return nil
	}
	base, rem := 12/n, 12%n
	out := make([]int, n)
	for i := range out {
		out[i] = base
		if i < rem {
			out[i]++
		}
	}
	return out
}

func wrapperCSS(top, right, bottom, left string) map[string]any {
	return map[string]any{
		"padding-top":    top,
		"padding-right":  right,
		"padding-bottom": bottom,
		"padding-left":   left,
	}
}

func addHeroSection(widgets map[string]any, sections *[]map[string]any, imageURL, linkURL string) {
	const key = "staging_banner"
	widgets[key] = map[string]any{
		"type":      "module",
		"module_id": moduleIDImage,
		"body": map[string]any{
			"module_id": moduleIDImage,
			"img": map[string]any{
				"src":   imageURL,
				"alt":   "Email Banner",
				"width": 600,
			},
			"link":                     linkURL,
			"stretch_on_mobile":        true,
			"hs_enable_module_padding": false,
			"hs_wrapper_css":           wrapperCSS("0px", "0px", "0px", "0px"),
		},
	}
	*sections = append(*sections, makeSection(
		"section-staging-banner",
		[]widgetColumn{{id: "col-banner-0", widgets: []string{key}, width: 12}},
		bannerSectionStyle,
	))
}

func richTextWidget(html string, padTop, padRight, padBottom, padLeft string) map[string]any {
	return map[string]any{
		"type": "module",
		"body": map[string]any{
			"path":                     "@hubspot/rich_text",
			"module_id":                moduleIDRichText,
			"html":                     html,
			"hs_enable_module_padding": true,
			"hs_wrapper_css":           wrapperCSS(padTop, padRight, padBottom, padLeft),
		},
	}
}

// bodyWidgetKey names the rebuild's own body widget. Shared with the preserve path above, so
// the two cannot drift onto different keys.
const bodyWidgetKey = "staging_body"

func addBodySection(widgets map[string]any, sections *[]map[string]any, bodyHTML string) {
	const key = bodyWidgetKey
	widgets[key] = richTextWidget(bodyHTML, "15px", "20px", "10px", "20px")
	*sections = append(*sections, makeSection(
		"section-staging-body",
		[]widgetColumn{{id: "col-body-0", widgets: []string{key}, width: 12}},
		defaultSectionStyle,
	))
}

// addButtonSection renders a native, centered HubSpot CTA button module (module_id
// moduleIDButton) — the drag-and-drop button widget, not a rich-text approximation.
// Reverse-engineered from the reference prototype's backend/integrations/hubspot.py
// (the "button" content-section branch) and verified against a live portal draft: the
// widget centers itself within its full-width (12-column) section, so no separate
// alignment field is needed. text defaults to "Register Now" when empty.
func addButtonSection(widgets map[string]any, sections *[]map[string]any, text, destination string) {
	text = strings.TrimSpace(text)
	if text == "" {
		text = "Register Now"
	}
	const key = "staging_button"
	widgets[key] = map[string]any{
		"type": "module",
		"body": map[string]any{
			"path":             "@hubspot/button",
			"module_id":        moduleIDButton,
			"background_color": "#0094ff",
			"corner_radius":    8,
			"destination":      destination,
			"font":             "Arial, sans-serif",
			"font_color":       "#ffffff",
			"font_size":        16,
			"font_style": map[string]any{
				"color": "#ffffff",
				"font":  "Arial, sans-serif",
				"size":  map[string]any{"units": "px", "value": 16},
				"styles": map[string]any{
					"bold":        true,
					"font-weight": "bold",
					"italic":      false,
					"underline":   false,
				},
			},
			"text":                     text,
			"hs_enable_module_padding": true,
			"hs_wrapper_css":           wrapperCSS("5px", "20px", "20px", "20px"),
		},
	}
	*sections = append(*sections, makeSection(
		"section-staging-button",
		[]widgetColumn{{id: "col-button-0", widgets: []string{key}, width: 12}},
		defaultSectionStyle,
	))
}

// chunkSponsorRows splits up to 5 sponsors into rows of 3 then 2, matching the
// reference prototype's _chunk_rows.
func chunkSponsorRows(items []Sponsor) [][]Sponsor {
	if len(items) > 5 {
		items = items[:5]
	}
	if len(items) > 3 {
		return [][]Sponsor{items[:3], items[3:]}
	}
	if len(items) == 0 {
		return nil
	}
	return [][]Sponsor{items}
}

func addSponsorTier(widgets map[string]any, sections *[]map[string]any, items []Sponsor, tierKey string, height, width int, pad string) {
	for r, row := range chunkSponsorRows(items) {
		widths := columnWidths(len(row))
		cols := make([]widgetColumn, 0, len(row))
		for j, sp := range row {
			key := fmt.Sprintf("staging_sponsor_%s_%d_%d", tierKey, r, j)
			alt := strings.TrimSpace(sp.Name)
			if alt == "" {
				alt = "Sponsor"
			}
			widgets[key] = map[string]any{
				"type": "module",
				"body": map[string]any{
					"module_id": moduleIDImage,
					"img": map[string]any{
						"alt":     alt,
						"height":  height,
						"loading": "disabled",
						"src":     sp.LogoURL,
						"width":   width,
					},
					"link":                     "",
					"hs_enable_module_padding": true,
					"hs_wrapper_css":           wrapperCSS(pad, pad, pad, pad),
				},
			}
			cols = append(cols, widgetColumn{
				id:      fmt.Sprintf("col-sponsor-%s-%d-%d", tierKey, r, j),
				widgets: []string{key},
				width:   widths[j],
			})
		}
		*sections = append(*sections, makeSection(
			fmt.Sprintf("section-sponsor-%s-row%d", tierKey, r),
			cols,
			defaultSectionStyle,
		))
	}
}

func addSponsorSections(widgets map[string]any, sections *[]map[string]any, sponsors []Sponsor) {
	var withLogos []Sponsor
	for _, sp := range sponsors {
		if strings.TrimSpace(sp.LogoURL) != "" {
			withLogos = append(withLogos, sp)
		}
	}
	if len(withLogos) > 10 {
		withLogos = withLogos[:10]
	}
	if len(withLogos) == 0 {
		return
	}

	tier1, tier2 := withLogos[:min(5, len(withLogos))], []Sponsor(nil)
	if len(withLogos) > 5 {
		tier2 = withLogos[5:]
	}

	const headerKey = "staging_sponsor_header"
	widgets[headerKey] = richTextWidget(
		`<p style="font-weight:bold;text-align:center;font-size:18px;line-height:175%;">Thank You to Our Sponsors!</p>`,
		"10px", "20px", "10px", "20px",
	)
	*sections = append(*sections, makeSection(
		"section-sponsor-header",
		[]widgetColumn{{id: "col-sponsor-header-0", widgets: []string{headerKey}, width: 12}},
		defaultSectionStyle,
	))

	addSponsorTier(widgets, sections, tier1, "t1", 60, 180, "15px")
	addSponsorTier(widgets, sections, tier2, "t2", 45, 140, "10px")
}

func addFooterSections(widgets map[string]any, sections *[]map[string]any, sentByOrg string) {
	org := strings.TrimSpace(sentByOrg)
	if org == "" {
		org = defaultSentByOrg
	}

	const dividerKey = "staging_footer_divider"
	widgets[dividerKey] = map[string]any{
		"type": "module",
		"body": map[string]any{
			"path":                     "@hubspot/email_divider",
			"module_id":                moduleIDEmailDivider,
			"line_type":                "solid",
			"color":                    map[string]any{"color": "#000000", "opacity": 100},
			"height":                   1,
			"width":                    100,
			"hs_enable_module_padding": true,
			"hs_wrapper_css":           wrapperCSS("5px", "20px", "10px", "20px"),
		},
	}

	const followHeaderKey = "staging_footer_follow_header"
	widgets[followHeaderKey] = map[string]any{
		"type": "module",
		"body": map[string]any{
			"path":                     "@hubspot/rich_text",
			"module_id":                moduleIDRichText,
			"html":                     `<p style="font-weight: bold; text-align: center;">FOLLOW US</p>`,
			"hs_enable_module_padding": false,
			"hs_wrapper_css":           map[string]any{},
		},
	}

	const socialKey = "staging_footer_social"
	widgets[socialKey] = map[string]any{
		"type": "module",
		"body": map[string]any{
			"path":         "@hubspot/follow_me_email",
			"module_id":    moduleIDFollowMe,
			"color_scheme": "black",
			"icon_shape":   "circle",
			"font_style": map[string]any{
				"color": "#000000",
				"font":  "Helvetica,Arial,sans-serif",
				"size":  map[string]any{"units": "px", "value": 14},
				"styles": map[string]any{
					"bold":      true,
					"italic":    false,
					"underline": false,
				},
			},
			"hs_enable_module_padding": false,
			"hs_wrapper_css":           map[string]any{},
			"social": []map[string]any{
				{
					"network": "icon",
					"network_image": map[string]any{
						"alt":    "LFX Insights",
						"height": 675,
						"src":    "https://8112310.fs1.hubspotusercontent-na1.net/hubfs/8112310/LFX%20Logo%20-%20white%20-%203-1.png",
						"width":  1536,
					},
					"url": "https://insights.linuxfoundation.org/",
				},
				{"network": "twitter", "url": "https://twitter.com/linuxfoundation"},
				{"network": "linkedin", "url": "https://www.linkedin.com/company/the-linux-foundation"},
				{"network": "youtube", "url": "https://www.youtube.com/user/TheLinuxFoundation"},
				{"network": "facebook", "url": "https://www.facebook.com/TheLinuxFoundation"},
			},
		},
	}

	const sentByKey = "staging_footer_body"
	widgets[sentByKey] = map[string]any{
		"type": "module",
		"body": map[string]any{
			"path":      "@hubspot/rich_text",
			"module_id": moduleIDRichText,
			"html": fmt.Sprintf(
				`<h2 style="font-size:8px;line-height:175%%;font-weight:normal;text-align:center;"><span style="font-size:12px;color:#000000;">This email was sent by: <span style="font-weight:normal;">%s</span></span></h2>`,
				org,
			),
			"hs_enable_module_padding": true,
			"hs_wrapper_css":           wrapperCSS("0px", "20px", "0px", "20px"),
		},
	}

	const nativeFooterKey = "staging_footer_hs"
	widgets[nativeFooterKey] = map[string]any{
		"type": "module",
		"body": map[string]any{
			"path":      "@hubspot/email_footer",
			"module_id": moduleIDEmailFooter,
			"font": map[string]any{
				"color": "#000000",
				"font":  "Arial, sans-serif",
				"size":  map[string]any{"units": "px", "value": 12},
				"styles": map[string]any{
					"bold":      false,
					"italic":    false,
					"underline": false,
				},
			},
			"link_font": map[string]any{
				"color":    "#0094ff",
				"font":     "Arial, sans-serif",
				"font_set": "DEFAULT",
				"size":     map[string]any{"units": "px", "value": 12},
				"styles": map[string]any{
					"bold":      false,
					"italic":    false,
					"underline": true,
				},
			},
			"hs_enable_module_padding": false,
			"hs_wrapper_css":           map[string]any{},
		},
	}

	*sections = append(*sections,
		makeSection("section-footer-divider", []widgetColumn{{id: "col-footer-divider-0", widgets: []string{dividerKey}, width: 12}}, defaultSectionStyle),
		makeSection("section-footer-follow-header", []widgetColumn{{id: "col-footer-follow-header-0", widgets: []string{followHeaderKey}, width: 12}}, defaultSectionStyle),
		makeSection("section-footer-social", []widgetColumn{{id: "col-footer-social-0", widgets: []string{socialKey}, width: 12}}, defaultSectionStyle),
		makeSection("section-footer-body", []widgetColumn{{id: "col-footer-body-0", widgets: []string{sentByKey}, width: 12}}, defaultSectionStyle),
		makeSection("section-footer-hs", []widgetColumn{{id: "col-footer-hs-0", widgets: []string{nativeFooterKey}, width: 12}}, defaultSectionStyle),
	)
}
