// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"errors"
	"testing"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/service/emailstage"
)

// TestBriefIdentityPairProblem pins the HTTP-edge copy of the column's delivery/stage pair CHECK.
//
// The rule is not "is this a valid delivery type AND a valid stage" but "do these two go
// together": each surface owns its own stage vocabulary, and a pair drawn from two surfaces names
// an identity slot no generator can serve. Both ends enforce it -- this one so the caller gets a
// 400 naming the problem, the CHECK so a non-HTTP writer cannot bypass it.
func TestBriefIdentityPairProblem(t *testing.T) {
	cases := []struct {
		name     string
		delivery model.DeliveryType
		stage    string
		wantErr  bool
	}{
		{"paid with no stage", model.DeliveryPaidMarketing, "", false},
		{"paid carrying an email stage", model.DeliveryPaidMarketing, emailstage.RegistrationPush, true},
		{"paid carrying a survey stage", model.DeliveryPaidMarketing, emailstage.SurveyPromo, true},

		{"email with an event stage", model.DeliveryEmail, emailstage.CFPLaunch, false},
		{"email with the last event stage", model.DeliveryEmail, emailstage.PostEvent, false},
		{"email with no stage", model.DeliveryEmail, "", true},
		// The cross-surface pair. Both halves are individually valid, which is exactly why the
		// rule has to judge them together.
		{"email carrying a survey stage", model.DeliveryEmail, emailstage.SurveyPromo, true},

		{"survey promoting a survey", model.DeliverySurvey, emailstage.SurveyPromo, false},
		{"survey promoting a report", model.DeliverySurvey, emailstage.ReportPromo, false},
		{"survey with no stage", model.DeliverySurvey, "", true},
		{"survey carrying an event stage", model.DeliverySurvey, emailstage.FinalCountdown, true},

		{"an unknown surface", model.DeliveryType("newsletter"), "", true},
		{"the empty surface", model.DeliveryType(""), "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := briefIdentityPairProblem(tc.delivery, tc.stage)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("briefIdentityPairProblem(%q, %q) = nil, want an error", tc.delivery, tc.stage)
				}
				// The sentinel is what maps this to a 400 rather than a 500; a bare error here
				// would surface as an internal failure for what is a caller mistake.
				if !errors.Is(err, domain.ErrBriefIdentityPairInvalid) {
					t.Errorf("error does not wrap ErrBriefIdentityPairInvalid: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("briefIdentityPairProblem(%q, %q) = %v, want nil", tc.delivery, tc.stage, err)
			}
		})
	}
}

// TestEverySurveyStageIsAcceptedOnItsOwnSurface is the reverse of the table above: it iterates the
// taxonomy rather than naming stages by hand, so a stage added to emailstage.SurveyNames() without
// a matching branch here fails rather than quietly becoming unwritable.
func TestEverySurveyStageIsAcceptedOnItsOwnSurface(t *testing.T) {
	for _, stage := range emailstage.SurveyNames() {
		if err := briefIdentityPairProblem(model.DeliverySurvey, stage); err != nil {
			t.Errorf("survey stage %q is declared by SurveyNames() but refused on the survey "+
				"surface: %v", stage, err)
		}
	}
}
