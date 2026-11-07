// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

// MetaCustomAudience is one custom audience of a Meta ad account, as the create form's audience
// picker lists it (#550). ID is what metaConfig.targeting.customAudiences takes.
type MetaCustomAudience struct {
	ID          string
	Name        string
	Subtype     string
	Description string
	CountLower  *int64
	CountUpper  *int64
}

// MetaCustomAudiences is the custom-audience read: every audience of AccountID (act_<digits>), the
// account a Meta create for the project uses.
type MetaCustomAudiences struct {
	AccountID string
	Audiences []MetaCustomAudience
}
