// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"testing"
	"time"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/hubspot"
)

// TestIsSentBrandIssue pins the gate for an email id a brief names as a reference: only an issue
// the brand actually sent passes, which is exactly what the picker lists for that brand.
func TestIsSentBrandIssue(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		email hubspot.Email
		brand string
		want  bool
	}{
		{"the brand's sent issue", hubspot.Email{Name: "26Q4 - Acme - Newsletter - October", State: "PUBLISHED"}, "acme", true},
		{"a draft of the brand's issue", hubspot.Email{Name: "26Q4 - Acme - Newsletter - November", State: "DRAFT"}, "Acme", false},
		{"another brand's sent issue", hubspot.Email{Name: "26Q4 - Other - Newsletter - October", State: "PUBLISHED"}, "Acme", false},
		{"a sent email that is no newsletter", hubspot.Email{Name: "26Q4 - Acme Summit - Registration Push", State: "PUBLISHED"}, "Acme", false},
		{"an automated sign-up follow-up", hubspot.Email{Name: "22 - Acme - Newsletter Form", State: "AUTOMATED"}, "Acme", false},
		{"a marked test send", hubspot.Email{Name: "26Q4 - Acme - Newsletter - October (Marketing Ops Test)", State: "PUBLISHED"}, "Acme", false},
		{"no brand", hubspot.Email{Name: "26Q4 - Acme - Newsletter - October", State: "PUBLISHED"}, " ", false},
	}
	for _, tc := range cases {
		if got := IsSentBrandIssue(tc.email, tc.brand, now); got != tc.want {
			t.Errorf("%s: IsSentBrandIssue = %v, want %v", tc.name, got, tc.want)
		}
	}
}
