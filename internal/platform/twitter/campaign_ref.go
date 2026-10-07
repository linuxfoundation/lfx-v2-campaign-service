// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

// maxCampaignIDLen bounds an X campaign id accepted by ValidateCampaignID. X publishes no width
// for its alphanumeric ids; the bound is
// the same 64 the package applies to account ids, and design/connection.go's
// resolve-twitter-ads-campaign declares it too.
const maxCampaignIDLen = 64

// ValidateCampaignID reports whether id is a usable X campaign id: the same campaignIDRe every
// campaign-scoped path in this package interpolates against, plus a length bound. Nothing is
// trimmed — the id is compared verbatim against stored platform ids. It contacts nothing and
// returns ErrInvalidCampaignID, so a caller can errors.Is-classify the refusal.
func ValidateCampaignID(id string) error {
	if len(id) > maxCampaignIDLen || !campaignIDRe.MatchString(id) {
		return ErrInvalidCampaignID
	}
	return nil
}
