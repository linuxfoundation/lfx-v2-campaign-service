// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package reddit

import (
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/campaignname"
)

// Campaign naming v2 (#587) for Reddit: LF's convention,
//
//	Events | <event> | <Region> | <Objective> | <AudienceIntent>[ <tactic>][ #<n>] | <Format> | <BusinessUnit> | <Funnel>
//
// in place of the legacy name's "Social" format and LFX project slug. Behind
// REDDIT_NAMING_V2_ENABLED; the dispatcher sets CampaignInput.Naming only while it is on, so with
// the flag off every name is the legacy one byte for byte. The lists, the validator and the
// composer are shared (internal/domain/campaignname); this file holds what is Reddit's own.

// ErrNamingInvalid marks a campaignname.Naming refused for redditConfig.naming. It wraps
// campaignname.ErrInvalid.
var ErrNamingInvalid = fmt.Errorf("reddit-ads: %w", campaignname.ErrInvalid)

// defaultAudienceIntent is the intent a v2 name leads segment 5 with when the caller sends none:
// Retargeting for a row labelled with the retargeting tactic, else Intent — the value the legacy
// Reddit name has always carried. A custom audience is not read as retargeting (it may as well be a
// suppression or lookalike list); the caller who knows sends naming.audienceIntent.
func defaultAudienceIntent(tactic string) string {
	if strings.EqualFold(strings.TrimSpace(tactic), "retargeting") {
		return campaignname.IntentRetargeting
	}
	return campaignname.IntentIntent
}

// campaignNameFormat is the v2 name's Format segment, derived from the create: Motion for the
// video_views objective (a campaign that optimizes for video views promotes a video post); else
// Social when PostURL promotes an EXISTING post, whose content this client never reads (X's
// tweetId maps the same way); else Static, the image post CreateCampaign authors from ImageURL /
// ImageURLSource.
//
// MAPPING HOOK: when a creative branch authors another post kind (a video or a carousel post), it
// maps it here — a video to campaignname.FormatMotion, a carousel to campaignname.FormatCarousel —
// and nowhere else: the format must stay a function of the request, never a caller field, so a
// retry composes the same name.
func campaignNameFormat(in CampaignInput, objective string) string {
	switch {
	case objective == "video_views":
		return campaignname.FormatMotion
	case strings.TrimSpace(in.PostURL) != "":
		return campaignname.FormatSocial
	default:
		return campaignname.FormatStatic
	}
}

// buildRedditCampaignNameV2 composes the v2 name from n (validated, with a business unit). objective
// is the resolved objective and geos the targeted countries (the US default included) the legacy
// region also reads. The region defaults to the one bucket every country falls in, else Global
// (campaignname.CountriesRegion) — unlike the legacy name, which reports the first country's
// bucket. The objective label, the funnel and the tactic and slot tokens are exactly the legacy
// name's.
func buildRedditCampaignNameV2(in CampaignInput, objective string, geos []string, n campaignname.Naming) string {
	objectiveLabel := redditObjectiveLabels[objective]
	if objectiveLabel == "" {
		objectiveLabel = "Conversions"
	}
	funnel := "ToFU"
	if st := strings.TrimSpace(replacePipes(in.StageToken)); st != "" {
		funnel = st
	}
	region := n.Region
	if region == "" {
		region = campaignname.CountriesRegion(geos)
	}
	intent := n.AudienceIntent
	if intent == "" {
		intent = defaultAudienceIntent(in.Tactic)
	}
	return campaignname.Compose(replacePipes(in.EventName), region, objectiveLabel,
		campaignname.IntentSegment(intent, in.Tactic, in.SlotToken), campaignNameFormat(in, objective), n.BusinessUnit, funnel)
}

// resolveCampaignName is the name CreateCampaign creates the campaign under. In order of
// precedence: the name the claimed row already records (FrozenCampaignName, used verbatim so a
// retry never recomposes it), the v2 name while Naming is set, else the legacy name. A Naming that
// fails its closed lists or lacks a business unit is a PreflightRefusal returned before any
// request. step records a frozen name that differs from the composed one.
func resolveCampaignName(in CampaignInput, objective string, geos []string) (name, step string, err error) {
	name = buildRedditCampaignName(in, objective, resolveRegion(geos))
	if in.Naming != nil {
		n, nerr := campaignname.RequireForCreate(*in.Naming, ErrNamingInvalid)
		if nerr != nil {
			return "", "", nerr
		}
		name = buildRedditCampaignNameV2(in, objective, geos, n)
	}
	if frozen := strings.TrimSpace(in.FrozenCampaignName); frozen != "" {
		if frozen != name {
			step = fmt.Sprintf("Campaign name reused from the campaign's recorded name %q (not recomposed)", frozen)
		}
		name = frozen
	}
	return name, step, nil
}
