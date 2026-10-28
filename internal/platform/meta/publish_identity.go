// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// PublishIdentity is the part of a Meta create request that decides whether the ads it makes
// can be PUBLISHED, as opposed to merely created: the Instagram identity bound to each ad
// creative and the EU Digital Services Act disclosures set on the ad set.
//
// Field names and placement, per Meta's Marketing API reference:
//   - instagram_user_id — a top-level field of the ad CREATIVE (sibling to object_story_spec),
//     "your Instagram account id, used for ads … on Instagram" (Graph v22.0+; the legacy name was
//     instagram_actor_id). https://developers.facebook.com/docs/marketing-api/reference/ad-creative/
//   - dsa_beneficiary / dsa_payor — fields of the AD SET: "The beneficiary/payor of all ads in this
//     ad set", and "For ad sets targeting the EU and/or associated territories, the dsa_payor and
//     dsa_beneficiary fields are required." https://developers.facebook.com/docs/marketing-api/reference/ad-campaign/
type PublishIdentity struct {
	GeoTargets      []string
	InstagramUserID string
	DSABeneficiary  string
	DSAPayor        string
}

// maxDSANameRunes bounds a DSA disclosure. Meta documents no limit; the value is a legal entity's
// display name shown in the Ad Library, so anything this long is a paste error, not a name.
const maxDSANameRunes = 256

// dsaTerritories are the ISO 3166-1 alpha-2 codes Meta treats as "the EU and associated
// territories" for the DSA disclosure requirement: the 27 member states plus the parts of the EU
// that carry their own ISO code (Åland and France's outermost regions). Over-inclusion costs a
// caller one disclosure Meta may not have insisted on; under-inclusion lets an unpublishable ad
// set be created, so the list errs inclusive. Non-EU EEA states (IS, LI, NO) are not listed:
// the DSA is an EU regulation and Meta's requirement is worded for the EU.
var dsaTerritories = map[string]bool{
	"AT": true, "BE": true, "BG": true, "HR": true, "CY": true, "CZ": true, "DK": true,
	"EE": true, "FI": true, "FR": true, "DE": true, "GR": true, "HU": true, "IE": true,
	"IT": true, "LV": true, "LT": true, "LU": true, "MT": true, "NL": true, "PL": true,
	"PT": true, "RO": true, "SK": true, "SI": true, "ES": true, "SE": true,
	// Integral parts of member states with their own ISO code.
	"AX": true, // Åland Islands (Finland)
	"GF": true, // French Guiana
	"GP": true, // Guadeloupe
	"MQ": true, // Martinique
	"RE": true, // Réunion
	"YT": true, // Mayotte
	"MF": true, // Saint Martin (French part)
}

// DSATargets returns, in the order given, the targeted countries that make Meta require the DSA
// disclosures. It reads the SAME normalized list the ad set is built from (validateGeoTargets), so
// a code the client would drop is never the reason for a refusal, and the default ["US"] for an
// empty list needs none.
func DSATargets(geoTargets []string) []string {
	var out []string
	for _, g := range validateGeoTargets(geoTargets) {
		if dsaTerritories[g] {
			out = append(out, g)
		}
	}
	return out
}

// UsesInstagram reports whether the placements, after the defaults are applied, put the ad set on
// Instagram (Instagram Feed, Stories or Reels — Stories and Reels both include Instagram's
// positions). The default placements include Instagram Feed, so the zero value reports true.
func UsesInstagram(p Placement) bool {
	pl := mergePlacements(p)
	return deref(pl.InstagramFeed) || deref(pl.Stories) || deref(pl.Reels)
}

// CheckPublishIdentity normalizes the identity fields (trimmed in place, so a whitespace-only
// value is absent) and refuses, before any Meta call, a request whose ads Meta would create but
// never publish:
//
//   - a supplied instagramUserId that is not a numeric IGSID;
//   - a dsaBeneficiary or dsaPayor that is too long or carries control characters;
//   - exactly one of the two disclosures (Meta needs both, and they are attached independently);
//   - neither disclosure when a targeted country is in the EU (DSATargets).
//
// An absent instagramUserId is NOT refused, even though the default placements include
// Instagram Feed: Meta still creates the ad, and requiring it would refuse every existing caller
// that has not yet collected an Instagram account. It is sent only for an Instagram placement.
//
// Every refusal wraps domain.ErrCreateConfigInvalid, so the synchronous create path answers 400,
// and is a domain.PreflightRefusal, so the async job reports it verbatim. No caller-typed text is
// echoed except through domain.CallerValue; the country codes come from the vetted ISO table.
func CheckPublishIdentity(id *PublishIdentity) error {
	id.InstagramUserID = strings.TrimSpace(id.InstagramUserID)
	id.DSABeneficiary = strings.TrimSpace(id.DSABeneficiary)
	id.DSAPayor = strings.TrimSpace(id.DSAPayor)

	// A malformed IGSID is only consumed at the creative POST, after the campaign and ad set
	// exist, where a 4xx is a non-fatal per-variant failure: it would leave a billable campaign
	// with no publishable ad. Meta object ids are decimal strings.
	if id.InstagramUserID != "" && !numericIDRE.MatchString(id.InstagramUserID) {
		return publishIdentityInvalid("instagramUserId %s is malformed: Meta Instagram account IDs (IGSID) are numeric strings", domain.CallerValue(id.InstagramUserID))
	}
	for _, f := range []struct{ key, val string }{{"dsaBeneficiary", id.DSABeneficiary}, {"dsaPayor", id.DSAPayor}} {
		if n := len([]rune(f.val)); n > maxDSANameRunes {
			return publishIdentityInvalid("%s is %d characters; an EU DSA disclosure is the name of the advertiser or payer and must be at most %d", f.key, n, maxDSANameRunes)
		}
		if strings.IndexFunc(f.val, unicode.IsControl) >= 0 {
			return publishIdentityInvalid("%s contains a control character (such as a line break); supply the name on one line", f.key)
		}
	}
	if (id.DSABeneficiary == "") != (id.DSAPayor == "") {
		supplied, missing := "dsaBeneficiary", "dsaPayor"
		if id.DSABeneficiary == "" {
			supplied, missing = "dsaPayor", "dsaBeneficiary"
		}
		return publishIdentityInvalid("EU DSA disclosures are incomplete: %s was supplied without %s; Meta requires both to publish a regulated ad set — supply both or omit both", supplied, missing)
	}
	if id.DSABeneficiary == "" {
		if eu := DSATargets(id.GeoTargets); len(eu) > 0 {
			return publishIdentityInvalid("EU DSA disclosures are required: geoTargets include EU countries (%s), and Meta will not publish an ad set targeting the EU without both dsaBeneficiary (who the ads benefit) and dsaPayor (who pays for them); supply both, or remove the EU countries", strings.Join(eu, ", "))
		}
	}
	return nil
}

// publishIdentityInvalid builds a refusal the create path answers 400 (ErrCreateConfigInvalid),
// marked operator-safe because every caller is a pure check that runs before any Meta request.
func publishIdentityInvalid(format string, args ...any) error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: metaConfig: %s", domain.ErrCreateConfigInvalid, fmt.Sprintf(format, args...)))
}
