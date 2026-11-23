// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package hubspot

import (
	"fmt"
	"html"
	"strings"
)

// EmailLayout selects the chrome RebuildEmailContent builds around the generated body.
//
// The body, the button and the hero are the same in every layout; what differs is everything a
// recipient reads as "who sent this": the strip above the logo, the logo, the divider colour, the
// social row and the sent-by line. Two layouts exist because two real families of email exist in
// the portal, and they do not look alike.
type EmailLayout string

const (
	// LayoutEvent is the zero value, deliberately: every caller that predates the survey surface
	// passes no layout and keeps the output it always had.
	LayoutEvent EmailLayout = ""
	// LayoutSurvey is the LF Research survey and report promotion email. Reproduced from the sent
	// "26Q4 - LF Research - State of Neocloud Survey: Email 1 (Survey Promo)" and compared against
	// the VM, CNCF Annual and FINOS survey emails, whose chrome is identical.
	LayoutSurvey EmailLayout = "survey"
)

// moduleIDViewInBrowser is HubSpot's built-in "View in browser" strip, the first section of every
// LF Research survey email. Read from a sent email's draft rather than guessed: like the other ids
// in this package it must be used verbatim, and it is written with no `path`, exactly as the real
// widget carries it.
const moduleIDViewInBrowser = 2794854

const (
	// defaultSurveySentByOrg is what the survey layout's sent-by line names when the caller names
	// no organisation. NOT defaultSentByOrg ("The Linux Foundation Events"): that one is right for
	// an event and wrong for a research survey, whose sent emails all read "The Linux Foundation".
	defaultSurveySentByOrg = "The Linux Foundation"

	surveyNavy = "#23496d"

	// The logo band is the same artwork on every LF Research survey email. The CDN host serves it
	// publicly, so a draft in any portal can show it without the file living in that portal.
	surveyLogoURL  = "https://8112310.fs1.hubspotusercontent-na1.net/hubfs/8112310/lf-research-color.png"
	surveyLogoLink = "https://linuxfoundation.org/research/"
)

// surveySectionStyle is a plain white-content section, the style every survey section but the
// view-in-browser strip uses.
var surveySectionStyle = map[string]any{
	"backgroundType": "CONTENT",
	"breakpointStyles": map[string]any{
		"default": map[string]any{"backgroundType": "CONTENT", "paddingBottom": "0px", "paddingTop": "0px"},
	},
	"paddingBottom": "0px",
	"paddingTop":    "0px",
}

// addSurveyHeader writes the two sections above a survey email's hero: the view-in-browser strip
// and the logo band.
//
// The strip carries an EMPTY backgroundColor on purpose. Every other section takes the page's
// white content colour; this one is transparent so "View in browser" sits on the grey page
// background, as it does in every sent survey email.
func addSurveyHeader(widgets map[string]any, sections *[]map[string]any) {
	const stripKey = "staging_survey_preheader"
	widgets[stripKey] = map[string]any{
		"type": "module",
		"body": map[string]any{
			"module_id": moduleIDViewInBrowser,
			"font": map[string]any{
				"color": "#00a4bd",
				"font":  "Arial, sans-serif",
				"size":  map[string]any{"units": "px", "value": 12},
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

	const logoKey = "staging_survey_logo"
	widgets[logoKey] = map[string]any{
		"type": "module",
		"body": map[string]any{
			"alignment":                "center",
			"css_class":                "dnd-module",
			"hs_enable_module_padding": true,
			"hs_wrapper_css":           wrapperCSS("20px", "20px", "30px", "20px"),
			"img":                      map[string]any{"alt": "LF Research", "height": 50, "src": surveyLogoURL, "width": 281},
			"link":                     surveyLogoLink,
			"module_id":                moduleIDImage,
			"path":                     "@hubspot/image_email",
			"schema_version":           2,
		},
	}

	*sections = append(*sections,
		makeSection("section-survey-preheader", []widgetColumn{{id: "col-survey-preheader-0", widgets: []string{stripKey}, width: 12}},
			map[string]any{"backgroundColor": "", "backgroundType": "CONTENT"}),
		makeSection("section-survey-logo", []widgetColumn{{id: "col-survey-logo-0", widgets: []string{logoKey}, width: 12}}, surveySectionStyle),
	)
}

// surveyDividerWidget is the navy hairline the sent emails use above and below the social row.
func surveyDividerWidget() map[string]any {
	return map[string]any{
		"type": "module",
		"body": map[string]any{
			"color":                    map[string]any{"color": surveyNavy, "opacity": 100},
			"css_class":                "dnd-module",
			"height":                   1,
			"hs_enable_module_padding": true,
			"hs_wrapper_css":           wrapperCSS("10px", "20px", "10px", "20px"),
			"line_type":                "solid",
			"module_id":                moduleIDEmailDivider,
			"path":                     "@hubspot/email_divider",
			"schema_version":           2,
			"width":                    100,
		},
	}
}

// surveySocialWidget is the seven-entry colour row the sent emails carry: HubSpot's stock four
// (Facebook, LinkedIn, YouTube, Instagram), two custom icons (GitHub and the LF "X" mark) and the
// contact address.
//
// The X link is https://, unlike the sent emails' scheme-less twitter.com link, which HubSpot
// rewrites to http://.
func surveySocialWidget() map[string]any {
	return map[string]any{
		"type": "module",
		"body": map[string]any{
			"css_class": "dnd-module",
			"font_style": map[string]any{
				"color": "#00a4bd",
				"font":  "Helvetica,Arial,sans-serif",
				"size":  map[string]any{"units": "px", "value": 14},
				"styles": map[string]any{
					"bold":      false,
					"italic":    false,
					"underline": true,
				},
			},
			"hs_enable_module_padding": false,
			"hs_wrapper_css":           map[string]any{},
			"module_id":                moduleIDFollowMe,
			"path":                     "@hubspot/follow_me_email",
			"schema_version":           2,
			"social": []map[string]any{
				{"network": "facebook", "url": "https://www.facebook.com/TheLinuxFoundation"},
				{"network": "linkedin", "url": "https://www.linkedin.com/company/208777"},
				{"network": "youtube", "url": "https://www.youtube.com/user/TheLinuxFoundation"},
				{"network": "instagram", "url": "https://www.instagram.com/linux_foundation/"},
				{
					"network": "icon",
					"network_image": map[string]any{
						"alt": "GitHub", "height": 256, "width": 256,
						"src": "https://f.hubspotusercontent10.net/hubfs/8112310/github%20icon.png",
					},
					"url": "https://github.com/linuxfoundation",
				},
				{"network": "mail", "url": "info@linuxfoundation.org"},
				{
					"network": "icon",
					"network_image": map[string]any{
						"alt": "X", "height": 2453, "width": 2400,
						"src": "https://www.linuxfoundation.org/hubfs/logo-black.png",
					},
					"url": "https://twitter.com/linuxfoundation",
				},
			},
		},
	}
}

// addSurveyFooterSections writes everything below the button: divider, social row, divider, the
// sent-by line and HubSpot's native footer (postal address and subscription link).
//
// The native footer rather than the sent emails' "/email_footer no links" custom module: that
// module's id belongs to one portal, so naming it would break the draft for a project that
// connected its own. The native one renders the same address and an unsubscribe link everywhere.
func addSurveyFooterSections(widgets map[string]any, sections *[]map[string]any, sentByOrg string) {
	org := strings.TrimSpace(sentByOrg)
	if org == "" {
		org = defaultSurveySentByOrg
	}

	const (
		topDividerKey    = "staging_survey_divider_top"
		socialKey        = "staging_survey_social"
		bottomDividerKey = "staging_survey_divider_bottom"
		sentByKey        = "staging_survey_sent_by"
		nativeFooterKey  = "staging_survey_footer"
	)
	widgets[topDividerKey] = surveyDividerWidget()
	widgets[socialKey] = surveySocialWidget()
	widgets[bottomDividerKey] = surveyDividerWidget()
	widgets[sentByKey] = map[string]any{
		"type": "module",
		"body": map[string]any{
			"path":      "@hubspot/rich_text",
			"module_id": moduleIDRichText,
			// html.EscapeString: the organisation name is caller-supplied and lands inside markup.
			"html": fmt.Sprintf(
				`<h2 style="font-size: 8px; line-height: 175%%; font-weight: normal; text-align: center;"><span style="font-size: 12px;">This email was sent by: <span style="font-weight: bold;">%s</span></span></h2>`,
				html.EscapeString(org),
			),
			"hs_enable_module_padding": true,
			"hs_wrapper_css":           wrapperCSS("0px", "20px", "0px", "20px"),
		},
	}
	widgets[nativeFooterKey] = nativeFooterWidget(surveyNavy, footerLinkColor)

	footerStyle := map[string]any{
		"backgroundColor": "#ffffff",
		"backgroundType":  "CONTENT",
		"paddingBottom":   "20px",
		"paddingTop":      "20px",
	}
	*sections = append(*sections,
		makeSection("section-survey-divider-top", []widgetColumn{{id: "col-survey-divider-top-0", widgets: []string{topDividerKey}, width: 12}}, surveySectionStyle),
		makeSection("section-survey-social", []widgetColumn{{id: "col-survey-social-0", widgets: []string{socialKey}, width: 12}}, surveySectionStyle),
		makeSection("section-survey-divider-bottom", []widgetColumn{{id: "col-survey-divider-bottom-0", widgets: []string{bottomDividerKey}, width: 12}}, surveySectionStyle),
		makeSection("section-survey-footer", []widgetColumn{{id: "col-survey-footer-0", widgets: []string{sentByKey, nativeFooterKey}, width: 12}}, footerStyle),
	)
}
