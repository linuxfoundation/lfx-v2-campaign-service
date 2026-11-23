// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
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

	t.Run("a range in the source keeps both of its numbers whole", func(t *testing.T) {
		body := `<p>It takes about 10-15 minutes.</p>`
		if got := surveyCopyWarnings(surveyBrief("The survey takes 10-15 minutes."), body); len(got) != 0 {
			t.Errorf("a range the source states was reported: %v", got)
		}
	})

	t.Run("a range does not back a figure made of its ends", func(t *testing.T) {
		got := strings.Join(surveyCopyWarnings(surveyBrief("A 3-5% discount."), `<p>A 35% discount.</p>`), "\n")
		if !strings.Contains(got, `"35%"`) {
			t.Errorf("an invented 35%% passed as the source's 3-5%%: %q", got)
		}
	})

	t.Run("a range whose lower end alone is changed is reported", func(t *testing.T) {
		cases := []struct{ source, body, claim string }{
			{"The survey takes 10-15 minutes.", `<p>It takes 5-15 minutes.</p>`, `"5-15 minutes"`},
			{"The survey takes 10-15 minutes.", `<p>It takes 5–15 minutes.</p>`, `"5–15 minutes"`},
			{"The survey has 8-12 questions.", `<p>Only 2-12 questions.</p>`, `"2-12 questions"`},
			{"A 3-5% discount.", `<p>A 1-5% discount.</p>`, `"1-5%"`},
			{"A 3-5% discount.", `<p>A 1%-5% discount.</p>`, `"1%-5%"`},
		}
		for _, tc := range cases {
			got := strings.Join(surveyCopyWarnings(surveyBrief(tc.source), tc.body), "\n")
			if !strings.Contains(got, tc.claim) {
				t.Errorf("source %q, copy %q: no warning for %s in:\n%s", tc.source, tc.body, tc.claim, got)
			}
		}
	})

	t.Run("a range the source states is silent in any dash or percent spelling", func(t *testing.T) {
		cases := []struct{ source, body string }{
			{"The survey takes 10-15 minutes.", `<p>It takes 10–15 minutes.</p>`},
			{"The survey takes 10–15 minutes.", `<p>It takes 10 - 15 minutes.</p>`},
			{"A 3-5% discount.", `<p>A 3%-5% discount.</p>`},
			{"A 3–5% discount.", `<p>A 3-5% discount.</p>`},
		}
		for _, tc := range cases {
			if got := surveyCopyWarnings(surveyBrief(tc.source), tc.body); len(got) != 0 {
				t.Errorf("source %q, copy %q: a stated range was reported: %v", tc.source, tc.body, got)
			}
		}
	})

	t.Run("a singular or plural unit backs the same figure in either direction", func(t *testing.T) {
		// "questionnaire" and "form" as well as "survey": the earlier text match passed "a 3-minute
		// survey" only because "3minutesurvey" contains "3minutes".
		cases := []struct{ source, body string }{
			{"Take our 3-minute questionnaire.", `<p>It takes 3 minutes.</p>`},
			{"It takes 3 minutes.", `<p>Take our 3-minute questionnaire.</p>`},
			{"A 9-question form.", `<p>Only 9 questions.</p>`},
			{"Only 9 questions.", `<p>A 9-question form.</p>`},
			{"Take our 3-minute survey.", `<p>It takes 3 minutes.</p>`},
		}
		for _, tc := range cases {
			if got := surveyCopyWarnings(surveyBrief(tc.source), tc.body); len(got) != 0 {
				t.Errorf("source %q, copy %q: the same figure was reported: %v", tc.source, tc.body, got)
			}
		}
		// Normalising the unit does not loosen the figure.
		got := strings.Join(surveyCopyWarnings(surveyBrief("Take our 3-minute questionnaire."), `<p>It takes 5 minutes.</p>`), "\n")
		if !strings.Contains(got, `"5 minutes"`) {
			t.Errorf("an invented 5 minutes passed against a 3-minute source: %q", got)
		}
	})

	t.Run("a decimal figure is read whole", func(t *testing.T) {
		reported := []struct{ source, body, claim string }{
			{"It takes 5 minutes.", `<p>It takes 8.5 minutes.</p>`, `"8.5 minutes"`},
			{"It takes 2.5-3.5 minutes.", `<p>It takes 1.5-3.5 minutes.</p>`, `"1.5-3.5 minutes"`},
		}
		for _, tc := range reported {
			got := strings.Join(surveyCopyWarnings(surveyBrief(tc.source), tc.body), "\n")
			if !strings.Contains(got, tc.claim) {
				t.Errorf("source %q, copy %q: no warning for %s in:\n%s", tc.source, tc.body, tc.claim, got)
			}
		}
		silent := []struct{ source, body string }{
			{"It takes 3.5 minutes.", `<p>Just 3.5 minutes.</p>`},
			{"It takes 2.5-3.5 minutes.", `<p>It takes 2.5–3.5 minutes.</p>`},
		}
		for _, tc := range silent {
			if got := surveyCopyWarnings(surveyBrief(tc.source), tc.body); len(got) != 0 {
				t.Errorf("source %q, copy %q: a stated decimal was reported: %v", tc.source, tc.body, got)
			}
		}
	})

	t.Run("a figure after a full stop or another number is still stated", func(t *testing.T) {
		for _, source := range []string{
			"The survey is open. 9 questions take 3 minutes.",
			"In 2026 9 questions take 3 minutes.",
			"Round 2 9 questions, 3 minutes.",
		} {
			if got := surveyCopyWarnings(surveyBrief(source), `<p>9 questions in 3 minutes.</p>`); len(got) != 0 {
				t.Errorf("source %q: a stated figure was reported: %v", source, got)
			}
		}
	})

	t.Run("a non-breaking space or a different spelling of a figure does not matter", func(t *testing.T) {
		cases := []struct{ source, body string }{
			{"The survey has 9\u00a0questions and a 35\u00a0% discount.", `<p>9 questions, 35% off.</p>`},
			{"The survey has 9 questions.", `<p>Only 9&nbsp;questions.</p>`},
			{"A 3.50% discount.", `<p>A 3.5% discount.</p>`},
		}
		for _, tc := range cases {
			if got := surveyCopyWarnings(surveyBrief(tc.source), tc.body); len(got) != 0 {
				t.Errorf("source %q, copy %q: a stated figure was reported: %v", tc.source, tc.body, got)
			}
		}
		// The non-breaking space does not hide an invented figure either.
		got := strings.Join(surveyCopyWarnings(surveyBrief("The survey has 9 questions."), `<p>Only 12&nbsp;questions.</p>`), "\n")
		if !strings.Contains(got, "12") {
			t.Errorf("an invented 12 questions behind &nbsp; was not reported: %q", got)
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

// The tests above call emailLayoutFor and surveyCopyWarnings directly, so they stay green if
// Dispatch stops calling either. These drive the real dispatcher against the recording fake and pin
// the calls themselves, including that the A/B variant is built in the same layout as its parent.

// stagedSurveyBrief is testBrief() turned into a survey brief with the given source text.
func stagedSurveyBrief(sourceText string) *model.CampaignBrief {
	b := testBrief()
	b.DeliveryType = model.DeliverySurvey
	details, _ := json.Marshal(map[string]any{
		"eventName": "State of Neocloud Survey", "registrationUrl": "https://www.research.net/r/859TSRR", "sourceText": sourceText,
	})
	b.EventDetails = details
	return b
}

// stageThroughDispatch stages the brief and returns the fake and the warnings the persisted
// result carries.
func stageThroughDispatch(t *testing.T, brief *model.CampaignBrief, hubspotConfig string) (*hubspotRec, []string) {
	t.Helper()
	srv, rec := hubspotServer(t)
	aud := fakeAudienceReader{auds: builtHubSpotAudience("26724", nil)}
	d := NewHubSpotDispatcher(fakeConnReader{conn: activeHubSpotConn(goodHubSpotCreds)}, identityEncryptor{}, aud, hubspot.WithBaseURL(srv.URL))
	camp, err := d.Dispatch(context.Background(), brief, model.ProviderHubSpot, json.RawMessage(`{"hubspotConfig":`+hubspotConfig+`}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	var blob struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(camp.Result, &blob); err != nil {
		t.Fatalf("decode result blob: %v", err)
	}
	return rec, blob.Warnings
}

// placedWidgets is the widget ids the parent draft and the variant draft ended up placing.
func placedWidgets(rec *hubspotRec) (parent, variant []string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]string(nil), rec.layout...), append([]string(nil), rec.variantLayout...)
}

func hasWidget(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestHubSpot_SurveyBriefIsStagedInTheSurveyLayout(t *testing.T) {
	brief := stagedSurveyBrief("The survey has 9 questions and a 35% discount.")
	rec, warnings := stageThroughDispatch(t, brief, `{"sourceEmailId":"555","bodyHtml":"<p>Take 9 questions, get 50% off.</p>"}`)

	parent, _ := placedWidgets(rec)
	for _, want := range []string{"staging_survey_preheader", "staging_survey_logo", "staging_survey_footer"} {
		if !hasWidget(parent, want) {
			t.Errorf("survey draft does not place %s; placed %v", want, parent)
		}
	}
	for _, banned := range []string{"staging_footer_hs", "staging_footer_body"} {
		if hasWidget(parent, banned) {
			t.Errorf("survey draft carries the event chrome %s; placed %v", banned, parent)
		}
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `"50%"`) {
		t.Errorf("warnings = %v, want exactly the unsupported 50%% figure", warnings)
	}
}

func TestHubSpot_SurveyVariantBGetsTheSameLayoutAndIsCheckedToo(t *testing.T) {
	brief := stagedSurveyBrief("A 3-minute survey with 9 questions.")
	rec, warnings := stageThroughDispatch(t, brief,
		`{"sourceEmailId":"555","bodyHtml":"<p>9 questions.</p>","abTestEnabled":true,"subjectB":"B","bodyHtmlB":"<p>Only 7 questions, 25% off.</p>"}`)

	parent, variant := placedWidgets(rec)
	for name, ids := range map[string][]string{"parent": parent, "variant": variant} {
		if !hasWidget(ids, "staging_survey_logo") || hasWidget(ids, "staging_footer_hs") {
			t.Errorf("%s draft is not in the survey layout; placed %v", name, ids)
		}
	}
	got := strings.Join(warnings, "\n")
	for _, claim := range []string{`"7 questions"`, `"25%"`} {
		if !strings.Contains(got, claim) {
			t.Errorf("no warning for %s, which appears only in Variant B:\n%s", claim, got)
		}
	}
}

func TestHubSpot_EventBriefKeepsTheEventLayoutAndIsNotChecked(t *testing.T) {
	rec, warnings := stageThroughDispatch(t, testBrief(), `{"sourceEmailId":"555","bodyHtml":"<p>50% off</p>"}`)

	parent, _ := placedWidgets(rec)
	if !hasWidget(parent, "staging_footer_hs") || hasWidget(parent, "staging_survey_logo") {
		t.Errorf("event draft is not in the event layout; placed %v", parent)
	}
	if len(warnings) != 0 {
		t.Errorf("an event brief was copy-checked: %v", warnings)
	}
}
