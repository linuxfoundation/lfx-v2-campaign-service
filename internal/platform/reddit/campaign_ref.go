// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import "fmt"

// maxCampaignIDLen bounds a Reddit campaign id accepted by ValidateCampaignID. Reddit publishes
// no width for its ids; the bound mirrors the MaxLength(64) design/connection.go puts on the
// connection's account_id, which shares this charset, and resolve-reddit-ads-campaign declares
// the same bound.
const maxCampaignIDLen = 64

// ValidateCampaignID reports whether id is a usable Reddit campaign id: the letters, digits and
// underscores charset (accountIDRe) every Reddit v3 path and report filter in this package
// interpolates ids against, plus a length bound. Nothing is trimmed — the id is compared
// verbatim against stored platform ids. It contacts nothing and wraps ErrInvalidCampaignID.
func ValidateCampaignID(id string) error {
	if id == "" || len(id) > maxCampaignIDLen || !accountIDRe.MatchString(id) {
		return fmt.Errorf("validate campaign id: %w", ErrInvalidCampaignID)
	}
	return nil
}
