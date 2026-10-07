// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"errors"
	"regexp"
)

// maxCampaignIDLen bounds a Meta campaign id accepted by ValidateCampaignID. Meta documents
// object ids as numeric strings without publishing a width; real campaign ids are 17-18 digits.
// The bound exists so a pathological value is refused at the boundary, not because a legitimate
// id approaches it. design/connection.go's resolve-meta-ads-campaign declares the same bound.
const maxCampaignIDLen = 32

// canonicalCampaignIDRe is numericIDRE without the leading zero numericIDRE admits. A Meta id is
// minted by Meta and never carries one, and a caller comparing it as a STRING against a stored
// platform id would read "0123" as a different row from "123" — so a non-canonical spelling can
// only come back as a confident "not yours", never as a match.
var canonicalCampaignIDRe = regexp.MustCompile(`^[1-9][0-9]*$`)

// ErrInvalidCampaignID marks a campaign id that is not a canonical Meta object id, so a caller
// can errors.Is-classify the refusal instead of matching message text.
var ErrInvalidCampaignID = errors.New("meta-ads: campaign id must be 1-32 digits without a leading zero")

// ValidateCampaignID reports whether id is a canonical Meta campaign id: digits only, no leading
// zero, at most maxCampaignIDLen. Nothing is trimmed — the id is compared verbatim against
// stored platform ids, so " 123" is not "123". It contacts nothing.
func ValidateCampaignID(id string) error {
	if len(id) > maxCampaignIDLen || !canonicalCampaignIDRe.MatchString(id) {
		return ErrInvalidCampaignID
	}
	return nil
}
