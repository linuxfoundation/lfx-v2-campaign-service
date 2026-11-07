// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

// TwitterCustomAudience is one custom audience on an X ad account (#564 gap 3), for the
// twitterConfig.customAudienceIds / excludedCustomAudienceIds picker. AudienceType is X's value
// verbatim; AudienceSize is nil when X sent none. ReasonsNotTargetable holds X's enum-shaped
// reasons only (TOO_SMALL, PROCESSING).
type TwitterCustomAudience struct {
	ID                   string
	Name                 string
	AudienceType         string
	Targetable           bool
	AudienceSize         *int64
	ReasonsNotTargetable []string
}

// TwitterCustomAudiences is the custom-audience read on the ad account an X create for the
// project uses. Truncated reports that the page walk stopped at its cap with more pages left.
type TwitterCustomAudiences struct {
	AccountID string
	Audiences []TwitterCustomAudience
	Truncated bool
}
