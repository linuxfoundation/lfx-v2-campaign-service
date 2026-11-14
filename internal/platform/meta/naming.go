// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package meta

import (
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/campaignname"
)

// Campaign naming v2 (#587) for Meta: LF's convention,
//
//	Events | <event> | <Region> | <Objective> | <AudienceIntent>[ <tactic>][ #<n>] | <Format> | <BusinessUnit> | <Funnel>
//
// in place of the legacy name's "Social" format and LFX project slug. Behind
// META_NAMING_V2_ENABLED; the dispatcher sets CampaignInput.Naming only while it is on, so with
// the flag off every name is the legacy one byte for byte. The lists, the validator and the
// composer are shared (internal/domain/campaignname); this file holds what is Meta's own.

// ErrNamingInvalid marks a campaignname.Naming refused for metaConfig.naming. It wraps
// campaignname.ErrInvalid.
var ErrNamingInvalid = fmt.Errorf("meta-ads: %w", campaignname.ErrInvalid)

// defaultAudienceIntent is the intent a v2 name leads segment 5 with when the caller sends none:
// Retargeting for a row labelled with the retargeting tactic, else Intent — the value the legacy
// Meta name has always carried, so a v2 name changes no intent the caller did not choose. A custom
// audience in metaConfig.targeting is not read as retargeting: it may as well be a lookalike or a
// prospecting list, and the caller who knows sends naming.audienceIntent.
func defaultAudienceIntent(tactic string) string {
	if strings.EqualFold(strings.TrimSpace(tactic), "retargeting") {
		return campaignname.IntentRetargeting
	}
	return campaignname.IntentIntent
}

// campaignNameFormat is the v2 name's Format segment, derived from what this create sends. Every
// creative CreateCampaign builds today is a single-image or bare link ad (object_story_spec
// link_data), so it is Static.
//
// MAPPING HOOK: AdVariant's doc already names video and carousel creatives as the next formats
// (video_data / child_attachments in place of link_data, buildCreativeSpec). When one lands it adds
// its creative kind to the input and maps it here — a video to campaignname.FormatMotion, a
// carousel to campaignname.FormatCarousel — and nowhere else: the format must stay a function of
// the request, never a caller field, so a retry composes the same name.
func campaignNameFormat(_ CampaignInput) string {
	return campaignname.FormatStatic
}

// buildCampaignNameV2 composes the v2 name from n (validated, with a business unit) and the
// geo-filtered country list the legacy name's region also reads. The region defaults to the one
// bucket every country falls in, else Global (campaignname.CountriesRegion) — unlike the legacy
// name, which reports the first country's bucket. The objective, funnel and the tactic and slot
// tokens are exactly the legacy name's, so the name keeps the uniqueness it had.
func buildCampaignNameV2(in CampaignInput, geoTargets []string, n campaignname.Naming) string {
	event := strings.ReplaceAll(strings.TrimSpace(in.EventName), "|", "-")
	objective := objectiveLabel(defaultObjective(strings.ToLower(strings.TrimSpace(in.Objective))))
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
	return campaignname.Compose(event, region, objective, campaignname.IntentSegment(intent, in.Tactic, in.SlotToken), campaignNameFormat(in), n.BusinessUnit, funnel)
}

// resolveCampaignName is the name CreateCampaign creates (or, on the ReconcileByName path, looks
// up) and the legacy name it would have composed. In order of precedence: the name the claimed row
// already records (FrozenCampaignName, used verbatim so a retry never recomposes it), the v2 name
// while Naming is set, else the legacy name. A Naming that fails its closed lists or lacks a
// business unit is a PreflightRefusal returned before any request. steps records a frozen name
// that differs from the composed one.
func resolveCampaignName(in CampaignInput, geoTargets []string) (name, legacy string, steps []string, err error) {
	legacy = buildCampaignName(in, geoTargets)
	name = legacy
	if in.Naming != nil {
		n, nerr := campaignname.RequireForCreate(*in.Naming, ErrNamingInvalid)
		if nerr != nil {
			return "", "", nil, nerr
		}
		name = buildCampaignNameV2(in, geoTargets, n)
	}
	if frozen := strings.TrimSpace(in.FrozenCampaignName); frozen != "" {
		if frozen != name {
			steps = append(steps, fmt.Sprintf("Campaign name reused from the campaign's recorded name %q (not recomposed, so an existing campaign is found by it)", frozen))
		}
		name = frozen
	}
	return name, legacy, steps, nil
}
