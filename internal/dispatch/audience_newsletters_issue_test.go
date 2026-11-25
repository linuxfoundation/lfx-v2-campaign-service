// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// TestIsSentIssue pins which emails count as an issue a brand sent. Every name and state below is
// a shape read from the live LF portal on 2026-11-25, with the brand replaced.
func TestIsSentIssue(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	past, future := hubspot.EmailTime("2026-11-02T15:00:00Z"), hubspot.EmailTime("2026-12-02T15:00:00Z")
	cases := []struct {
		name  string
		email hubspot.Email
		want  bool
	}{
		{"a sent issue", hubspot.Email{Name: "26Q4 - Acme - Newsletter - October", State: "PUBLISHED", PublishDate: past}, true},
		{"a sent issue left with its clone suffix", hubspot.Email{Name: "Acme Foundation Newsletter - April 2026 (Clone)", State: "PUBLISHED", PublishDate: past}, true},
		{"the sent half of an A/B test", hubspot.Email{Name: "(A) 26Q2 - Acme - June Newsletter - A/B test", State: "PUBLISHED_AB", PublishDate: past}, true},
		{"the variant half of an A/B test", hubspot.Email{Name: "(B) 26Q2 - Acme - June Newsletter - A/B test", State: "PUBLISHED_AB_VARIANT", PublishDate: past}, false},
		{"a draft", hubspot.Email{Name: "26Q4 - Acme - Newsletter - November", State: "DRAFT"}, false},
		{"a scheduled send", hubspot.Email{Name: "Acme Newsletter - November 2026 (Clone)", State: "PUBLISHED_OR_SCHEDULED", PublishDate: future}, false},
		{"a sign-up form's automated follow-up", hubspot.Email{Name: "23Q3 - Acme - Newsletter Form - Thank You", State: "AUTOMATED", PublishDate: past}, false},
		{"an automated email for a form", hubspot.Email{Name: "22 - Acme - Newsletter Form", State: "AUTOMATED_FOR_FORM", PublishDate: past}, false},
		{"a published send marked as a test", hubspot.Email{Name: "26Q3 - Acme - Summer Newsletter (Marketing Ops Test)", State: "PUBLISHED", PublishDate: past}, false},
		{"a bracketed test marker", hubspot.Email{Name: "[TEST] Acme Newsletter - November", State: "PUBLISHED", PublishDate: past}, false},
		{"a word that only contains test", hubspot.Email{Name: "Acme Newsletter (Latest Contest Winners)", State: "PUBLISHED", PublishDate: past}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSentIssue(tc.email, now); got != tc.want {
				t.Errorf("isSentIssue(%q, %s) = %v, want %v", tc.email.Name, tc.email.State, got, tc.want)
			}
		})
	}
}
