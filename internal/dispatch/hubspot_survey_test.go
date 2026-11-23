// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

func surveyBrief(sourceText string) *model.CampaignBrief {
	details, _ := json.Marshal(map[string]any{"sourceText": sourceText})
	return &model.CampaignBrief{DeliveryType: model.DeliverySurvey, EventDetails: details}
}

func TestEmailLayoutFor(t *testing.T) {
	cases := []struct {
		name  string
		brief *model.CampaignBrief
		want  hubspot.EmailLayout
	}{
		{"survey brief", &model.CampaignBrief{DeliveryType: model.DeliverySurvey}, hubspot.LayoutSurvey},
		{"email brief", &model.CampaignBrief{DeliveryType: model.DeliveryEmail}, hubspot.LayoutEvent},
		{"paid brief", &model.CampaignBrief{DeliveryType: model.DeliveryPaidMarketing}, hubspot.LayoutEvent},
		{"no delivery type", &model.CampaignBrief{}, hubspot.LayoutEvent},
		{"nil brief", nil, hubspot.LayoutEvent},
	}
	for _, tc := range cases {
		if got := emailLayoutFor(tc.brief); got != tc.want {
			t.Errorf("%s: layout = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSurveyCopyWarnings(t *testing.T) {
	const source = "The State of Neocloud survey has 9 questions and takes about 3 minutes. " +
		"Participants receive a 35% discount, valid until April 30, 2027."

	t.Run("copy that matches the source is silent", func(t *testing.T) {
		body := `<p>Only <strong>9 questions</strong>, about 3 minutes. Get a 35% discount before April 30.</p>`
		if got := surveyCopyWarnings(surveyBrief(source), body); len(got) != 0 {
			t.Errorf("unexpected warnings: %v", got)
		}
	})

	t.Run("an invented percentage, count and date are each reported", func(t *testing.T) {
		body := `<p>Take our 12 questions in 10 minutes and get 50% off before May 1.</p>`
		got := strings.Join(surveyCopyWarnings(surveyBrief(source), body), "\n")
		for _, claim := range []string{`"12 questions"`, `"10 minutes"`, `"50%"`, `"May 1"`} {
			if !strings.Contains(got, claim) {
				t.Errorf("no warning for %s in:\n%s", claim, got)
			}
		}
	})

	t.Run("a figure inside a longer number is still unsupported", func(t *testing.T) {
		const longer = "A 35% discount for a 13 minute survey, plus 2.5% back."
		body := `<p>Get 5% off in 3 minutes, or 5% back.</p>`
		got := strings.Join(surveyCopyWarnings(surveyBrief(longer), body), "\n")
		for _, claim := range []string{`"5%"`, `"3 minutes"`} {
			if !strings.Contains(got, claim) {
				t.Errorf("no warning for %s, which the source only states as part of a longer figure:\n%s", claim, got)
			}
		}
		// The whole figure, found on a clean left edge, is still supported.
		if got := surveyCopyWarnings(surveyBrief(longer), `<p>A 35% discount and 2.5% back.</p>`); len(got) != 0 {
			t.Errorf("a stated figure was reported: %v", got)
		}
	})

	t.Run("a style percentage is not a claim", func(t *testing.T) {
		body := `<p style="line-height:160%;width:100%">Hello</p>`
		if got := surveyCopyWarnings(surveyBrief(source), body); len(got) != 0 {
			t.Errorf("a style attribute was read as a claim: %v", got)
		}
	})

	t.Run("spacing and month spelling do not matter", func(t *testing.T) {
		body := `<p>A 35 % discount, valid until Apr 30th.</p>`
		if got := surveyCopyWarnings(surveyBrief(source), body); len(got) != 0 {
			t.Errorf("formatting differences were reported: %v", got)
		}
	})

	t.Run("an empty source makes every claim unsupported", func(t *testing.T) {
		got := surveyCopyWarnings(surveyBrief(""), `<p>A 35% discount.</p>`)
		if len(got) != 1 || !strings.Contains(got[0], `"35%"`) {
			t.Errorf("warnings = %v, want one for 35%%", got)
		}
	})

	t.Run("the list is capped and de-duplicated", func(t *testing.T) {
		body := `<p>1% 2% 3% 4% 5% 6% 7% 7% 7%</p>`
		if got := surveyCopyWarnings(surveyBrief(""), body); len(got) != maxSurveyCopyWarnings {
			t.Errorf("len = %d, want %d: %v", len(got), maxSurveyCopyWarnings, got)
		}
	})

	t.Run("an event brief is never checked", func(t *testing.T) {
		brief := &model.CampaignBrief{DeliveryType: model.DeliveryEmail}
		if got := surveyCopyWarnings(brief, `<p>50% off</p>`); got != nil {
			t.Errorf("event brief produced warnings: %v", got)
		}
	})

	t.Run("an empty body is silent", func(t *testing.T) {
		if got := surveyCopyWarnings(surveyBrief(source), "  "); got != nil {
			t.Errorf("empty body produced warnings: %v", got)
		}
	})
}
