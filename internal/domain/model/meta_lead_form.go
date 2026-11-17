// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

// MetaLeadForm is one Instant Form of the Meta connection's Page, as the leadFormId picker lists
// it (#550). Status is Meta's, verbatim.
type MetaLeadForm struct {
	ID     string
	Name   string
	Status string
}

// MetaLeadForms is the Instant Form read: the Page read and its forms.
type MetaLeadForms struct {
	PageID string
	Forms  []MetaLeadForm
}
