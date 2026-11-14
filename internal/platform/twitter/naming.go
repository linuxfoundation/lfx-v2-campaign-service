// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package twitter

import (
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/campaignname"
)

// Campaign naming v2 (#587) for X: LF's convention,
//
//	Events | <event> | <Region> | <Objective> | <AudienceIntent>[ <tactic>][ #<n>] | <Format> | <BusinessUnit> | <Funnel>
//
// in place of the legacy name's fixed "Global" region, "Promoted Post" format and LFX project slug.
// Behind TWITTER_NAMING_V2_ENABLED; the dispatcher sets CampaignInput.Naming only while it is on,
// so with the flag off every name is the legacy one byte for byte. The lists, the validator and
// the composer are shared (internal/domain/campaignname); this file holds what is X's own.

// ErrNamingInvalid marks a campaignname.Naming refused for twitterConfig.naming. It wraps
// campaignname.ErrInvalid.
var ErrNamingInvalid = fmt.Errorf("twitter-ads: %w", campaignname.ErrInvalid)

// nameObjective is the v2 name's Objective segment: "Awareness", exactly the legacy name's, so the
// objective dimension a campaign reports under does not move with the naming change. (The line
// item is created with objective WEBSITE_CLICKS; whether X should report as Traffic is a separate
// data-owner question, #587 log.)
const nameObjective = "Awareness"

// defaultAudienceIntent is the intent a v2 name leads segment 5 with when the caller sends none:
// Retargeting for a row labelled with the retargeting tactic, else Prospecting — the value the
// legacy X name has always carried. A custom audience is not read as retargeting; the caller who
// knows sends naming.audienceIntent.
func defaultAudienceIntent(tactic string) string {
	if strings.EqualFold(strings.TrimSpace(tactic), "retargeting") {
		return campaignname.IntentRetargeting
	}
	return campaignname.IntentProspecting
}

// campaignNameFormat is the v2 name's Format segment, derived from the promoted post this create
// builds: Social when it promotes an EXISTING post (TweetID wins over TweetText), whose content
// this client never reads; otherwise the post it authors — Static with an image
// (CampaignInput.Image, uploaded through media.go), Text without one.
//
// MAPPING HOOK: when a creative branch authors a video or a carousel post, it maps it here — a
// video to campaignname.FormatMotion, a carousel to campaignname.FormatCarousel — and nowhere
// else: the format must stay a function of the request, never a caller field, so a retry composes
// the same name.
func campaignNameFormat(in CampaignInput) string {
	switch {
	case strings.TrimSpace(in.TweetID) != "":
		return campaignname.FormatSocial
	case in.Image != nil:
		return campaignname.FormatStatic
	default:
		return campaignname.FormatText
	}
}

// buildTwitterCampaignNameV2 composes the v2 name from n (validated, with a business unit).
// geoTargets are the normalized twitterConfig.geoTargets (empty unless
// TWITTER_CREATE_TARGETING_ENABLED admitted them); the region defaults to the one bucket every
// country falls in, else Global — so a campaign with no geo targets, which serves worldwide, stays
// Global as in the legacy name. The funnel and the tactic and slot tokens are exactly the legacy
// name's, so the name keeps the uniqueness findCampaignByName reuses a campaign by.
func buildTwitterCampaignNameV2(in CampaignInput, geoTargets []string, n campaignname.Naming) string {
	event := strings.ReplaceAll(in.EventName, "|", "-")
	funnel := "MoFU"
	if st := nameToken(in.StageToken); st != "" {
		funnel = st
	}
	region := n.Region
	if region == "" {
		region = campaignname.CountriesRegion(geoTargets)
	}
	intent := n.AudienceIntent
	if intent == "" {
		intent = defaultAudienceIntent(in.Tactic)
	}
	return campaignname.Compose(event, region, nameObjective, campaignname.IntentSegment(intent, in.Tactic, in.SlotToken), campaignNameFormat(in), n.BusinessUnit, funnel)
}

// resolveCampaignName is the name CreateCampaign finds or creates the campaign by, and the legacy
// name it would have composed. In order of precedence: the name the claimed row already records
// (FrozenCampaignName, used verbatim so a retry never recomposes it), the v2 name while Naming is
// set, else the legacy name. A Naming that fails its closed lists or lacks a business unit is a
// PreflightRefusal returned before any request. step records a frozen name that differs from the
// composed one.
func resolveCampaignName(in CampaignInput, geoTargets []string) (name, legacy, step string, err error) {
	legacy = buildTwitterCampaignName(in)
	name = legacy
	if in.Naming != nil {
		n, nerr := campaignname.RequireForCreate(*in.Naming, ErrNamingInvalid)
		if nerr != nil {
			return "", "", "", nerr
		}
		name = buildTwitterCampaignNameV2(in, geoTargets, n)
	}
	if frozen := strings.TrimSpace(in.FrozenCampaignName); frozen != "" {
		if frozen != name {
			step = fmt.Sprintf("Campaign name reused from the campaign's recorded name %q (not recomposed, so an existing campaign is found by it)", frozen)
		}
		name = frozen
	}
	return name, legacy, step, nil
}
