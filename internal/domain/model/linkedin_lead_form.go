// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

// LinkedInLeadForm is one Lead Gen Form of a LinkedIn ad account, as the linkedInConfig.leadGenForm
// picker lists it (#664). State is LinkedIn's, verbatim (DRAFT or PUBLISHED; ARCHIVED forms are
// not listed).
type LinkedInLeadForm struct {
	ID    string
	URN   string
	Name  string
	State string
}

// LinkedInLeadForms is the Lead Gen Form read: the ad account read and its forms.
type LinkedInLeadForms struct {
	AccountID string
	Forms     []LinkedInLeadForm
}
