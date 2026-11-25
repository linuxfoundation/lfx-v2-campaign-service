// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"errors"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service/emailstage"
)

// TestEmailCopyStage pins that a newsletter brief is always written with a newsletter stage. The
// `stage` parameter is optional; an absent one used to send a newsletter brief down the frozen
// event prompt, which drops the operator's notes and skips the newsletter link rule.
func TestEmailCopyStage(t *testing.T) {
	newsletter := &model.CampaignBrief{DeliveryType: model.DeliveryNewsletter, Stage: emailstage.NewsletterIssue}
	unstaged := &model.CampaignBrief{DeliveryType: model.DeliveryNewsletter}
	event := &model.CampaignBrief{DeliveryType: model.DeliveryEmail, Stage: emailstage.RegistrationPush}

	cases := []struct {
		name      string
		brief     *model.CampaignBrief
		requested string
		want      string
	}{
		{"newsletter brief, no stage sent", newsletter, "", emailstage.NewsletterIssue},
		{"newsletter brief, an event stage sent", newsletter, emailstage.RegistrationPush, emailstage.NewsletterIssue},
		{"newsletter brief, newsletter stage sent", newsletter, emailstage.NewsletterIssue, emailstage.NewsletterIssue},
		{"newsletter brief with no stored stage", unstaged, "", emailstage.DefaultNewsletterStage},
		{"event brief, no stage sent keeps the legacy prompt", event, "", ""},
		{"event brief keeps the stage it was sent", event, emailstage.RegistrationPush, emailstage.RegistrationPush},
		{"event brief sent the newsletter stage does not get the newsletter prompt", event, emailstage.NewsletterIssue, emailstage.RegistrationPush},
		{"survey brief sent the newsletter stage does not get the newsletter prompt", &model.CampaignBrief{DeliveryType: model.DeliverySurvey}, emailstage.NewsletterIssue, emailstage.RegistrationPush},
		{"paid brief sent the newsletter stage does not get the newsletter prompt", &model.CampaignBrief{DeliveryType: model.DeliveryPaidMarketing}, emailstage.NewsletterIssue, emailstage.RegistrationPush},
		{"no brief keeps the parameter", nil, "anything", "anything"},
	}
	for _, tc := range cases {
		if got := emailCopyStage(tc.brief, tc.requested); got != tc.want {
			t.Errorf("%s: emailCopyStage = %q, want %q", tc.name, got, tc.want)
		}
		isNewsletterBrief := tc.brief != nil && tc.brief.DeliveryType == model.DeliveryNewsletter
		if tc.brief != nil && emailstage.IsNewsletterStage(emailCopyStage(tc.brief, tc.requested)) != isNewsletterBrief {
			t.Errorf("%s: the prompt surface must follow the brief's delivery type", tc.name)
		}
	}
}

// TestRefuseNewsletterWizard pins that the wizard turns a newsletter brief away: its clone step
// replaces one rich-text block and applies the events UTM tagging, which would leave the previous
// issue's other items in the draft. Event and survey briefs are untouched.
func TestRefuseNewsletterWizard(t *testing.T) {
	refused := []*model.CampaignBrief{
		{DeliveryType: model.DeliveryNewsletter},
		{DeliveryType: model.DeliveryEmail, Stage: emailstage.NewsletterIssue},
	}
	for i, brief := range refused {
		err := refuseNewsletterWizard(brief)
		var bad *briefs.BadRequestError
		if !errors.As(err, &bad) {
			t.Errorf("case %d: a newsletter brief must be a 400 from the wizard, got %v", i, err)
		}
	}
	allowed := []*model.CampaignBrief{
		nil,
		{DeliveryType: model.DeliveryEmail, Stage: emailstage.RegistrationPush},
		{DeliveryType: model.DeliverySurvey},
		{},
	}
	for i, brief := range allowed {
		if err := refuseNewsletterWizard(brief); err != nil {
			t.Errorf("case %d: only newsletter briefs are refused, got %v", i, err)
		}
	}
}
