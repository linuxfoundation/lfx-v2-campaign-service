// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package linkedin

import (
	"errors"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Campaign naming v2 (#587): LF's own LinkedIn ad set convention,
//
//	Events | <event> | <Region> | <Objective> | <AudienceIntent> | <Format> | <BusinessUnit> | <Funnel>
//
// e.g. "Events | Open Source Summit Japan 2026 | APAC | Conversions | Intent | Static | LF Events | MoFU".
// It replaces the legacy name's "LinkedIn" region, "Prospecting" targeting and LFX project slug
// with values the data pipeline (lf-dbt parse_campaign_name_position, which reads a name
// positionally only when it has exactly 8 segments) reports as they are. Behind
// LINKEDIN_ADS_NAMING_V2_ENABLED; the dispatcher sets CampaignInput.Naming only when it is on, so
// with the flag off every name is the legacy one byte for byte.
//
// Every value below comes from a CLOSED list, never free text: a name segment is a reporting
// dimension, and the pipeline's fallback parsers match words anywhere in a name.

// Regions a v2 name's Region segment may carry. The buckets are Meta's and Reddit's (geoToRegion
// in internal/platform/meta), so one event reports one region across platforms. lf-dbt's
// keyword fallback (parse_campaign_region) knows NA, EMEA, APAC and Global only, but a well-formed
// 8-segment name is read positionally, so LATAM and India are reported as written.
var NameRegions = []string{"NA", "EMEA", "APAC", "LATAM", "India", "Global"}

// NameRegionGlobal is the region of a campaign whose geo targets name no single bucket.
const NameRegionGlobal = "Global"

// Audience intents a v2 name's segment 5 may lead with. Prospecting and Retargeting are derived
// when the caller sends none (defaultAudienceIntent); Intent (in-market, high-intent audiences)
// is only ever the caller's choice.
var NameAudienceIntents = []string{"Intent", "Prospecting", "Retargeting"}

// Ad formats a v2 name's segment 6 may carry. The format is derived server-side from the creative
// type (campaignNameFormat) and never sent by a caller.
const (
	NameFormatStatic = "Static" // single-image sponsored content: every campaign this client creates today
	NameFormatMotion = "Motion" // video sponsored content
	NameFormatSocial = "Social" // reserved: LF's list carries it; no LinkedIn creative type maps to it yet
	NameFormatText   = "Text"   // text ads
)

// NameBusinessUnits are the values a v2 name's segment 7 may carry: exactly the canonical project
// names lf-dbt resolves to a Salesforce project id. Source, read-only: lf-dbt
// macros/parse_campaign_name.sql at origin/main 0b6b6c15d (unchanged since 2ac0d65dc) — the
// outputs of normalize_project_name, plus the LF umbrella names map_paid_ads_project_id maps to
// the Linux Foundation project id. Each value is a fixed point of normalize_project_name (it
// normalizes to itself) and is listed in map_paid_ads_project_id, so a campaign carrying it lands
// in the right foundation roll-up; TestNameBusinessUnits_AreFixedPointsOfTheDbtNormalizer pins the
// first property against a copy of the macro.
//
// This is a COPY of a list another repository owns. When lf-dbt adds a project to
// map_paid_ads_project_id, add it here too (or a caller cannot select it); a value removed there
// must be removed here before a campaign reports under a name dbt no longer maps.
var NameBusinessUnits = []string{
	"Cloud Native Computing Foundation (CNCF)",
	"LF Education",
	"LF Events",
	"PyTorch Project",
	"OpenSearch Project",
	"Open Source Security Foundation (OpenSSF)",
	"RISC-V International",
	"Agentic AI Foundation (AAIF)",
	"Valkey",
	"FinOps Project",
	"LF Core",
	"LF Corporate",
	"LF Training",
	"LFX Platform",
	"The Linux Foundation",
}

// CampaignNaming is the caller's choice of the v2 name's caller-chosen segments
// (linkedInConfig.naming). Each is optional at the JSON level; see the dispatcher for which are
// required while v2 is on.
type CampaignNaming struct {
	Region         string `json:"region,omitempty"`
	AudienceIntent string `json:"audienceIntent,omitempty"`
	BusinessUnit   string `json:"businessUnit,omitempty"`
}

// ErrNamingInvalid marks a CampaignNaming refused by ValidateNaming.
var ErrNamingInvalid = errors.New("linkedin-ads: invalid naming")

// ValidateNaming checks every set field against its closed list, matching case-insensitively,
// and returns the copy with each value in its canonical spelling. An empty field is left empty
// (its default is applied when the name is composed). The refusal names the field and the list;
// the caller's value is quoted through domain.CallerValue.
func ValidateNaming(n CampaignNaming) (CampaignNaming, error) {
	var err error
	if n.Region, err = canonicalName("region", n.Region, NameRegions); err != nil {
		return CampaignNaming{}, err
	}
	if n.AudienceIntent, err = canonicalName("audienceIntent", n.AudienceIntent, NameAudienceIntents); err != nil {
		return CampaignNaming{}, err
	}
	if n.BusinessUnit, err = canonicalName("businessUnit", n.BusinessUnit, NameBusinessUnits); err != nil {
		return CampaignNaming{}, err
	}
	return n, nil
}

func canonicalName(field, value string, list []string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", nil
	}
	for _, c := range list {
		if strings.EqualFold(v, c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("%w: %s %s is not one of %s", ErrNamingInvalid, field, domain.CallerValue(value), strings.Join(list, ", "))
}

// geoRegion buckets the URNs of geoResolveMap (the only geo URNs this package has verified) the
// way Meta's geoToRegion buckets the same countries. A URN not listed has no known region.
var geoRegion = map[string]string{
	"urn:li:geo:103644278": "NA",    // United States
	"urn:li:geo:101282230": "EMEA",  // Germany
	"urn:li:geo:101165590": "EMEA",  // United Kingdom
	"urn:li:geo:102713980": "India", // India
	"urn:li:geo:101355337": "APAC",  // Japan
	"urn:li:geo:102454443": "APAC",  // Singapore
	"urn:li:geo:105149562": "APAC",  // South Korea
	"urn:li:geo:101452733": "APAC",  // Australia
	"urn:li:geo:104441761": "APAC",  // Taiwan
	"urn:li:geo:103291313": "APAC",  // Hong Kong
}

// defaultRegion is the Region a v2 name carries when the caller sends none: the one region every
// geo target falls in, else Global. Unlike Meta's first-geo rule, a campaign spanning two buckets
// (US and Germany) is Global rather than reported under its first country, and a geo this
// package cannot bucket makes it Global too. A caller who knows better sends naming.region.
func defaultRegion(geos []GeoTarget) string {
	region := ""
	for _, g := range geos {
		r, ok := geoRegion[strings.TrimSpace(g.URN)]
		if !ok || (region != "" && r != region) {
			return NameRegionGlobal
		}
		region = r
	}
	if region == "" {
		return NameRegionGlobal
	}
	return region
}

// defaultAudienceIntent is the intent a v2 name leads segment 5 with when the caller sends none:
// Retargeting for a row labelled with the retargeting tactic, or for caller targeting that
// includes dynamicSegments (LinkedIn's retargeting audiences); Prospecting otherwise. An excluded
// dynamic segment is not retargeting.
func defaultAudienceIntent(tactic string, t *Targeting) string {
	if strings.EqualFold(strings.TrimSpace(tactic), "retargeting") {
		return "Retargeting"
	}
	if t != nil {
		for _, clause := range t.Include {
			if len(clause[FacetDynamicSegments]) > 0 {
				return "Retargeting"
			}
		}
	}
	return "Prospecting"
}

// campaignNameFormat is the v2 name's Format segment, derived from what this create sends. Every
// campaign CreateCampaign creates today is single-image sponsored content, so it is Static.
//
// MAPPING HOOK: the text-ad and video creative branches are being built separately. When one
// lands, it adds its creative type to CampaignInput and maps it here — a text ad to
// NameFormatText, a video to NameFormatMotion — and nowhere else: the format must stay a
// function of the request, never a caller field, so a retry composes the same name.
func campaignNameFormat(_ CampaignInput) string {
	return NameFormatStatic
}

// buildCampaignNameV2 composes the v2 name. eventName arrives trimmed and pipe-stripped from
// CreateCampaign; n has passed ValidateNaming and carries a business unit (resolveNaming). The
// tactic and slot tokens are appended to segment 5 exactly as in the legacy name ("Retargeting
// retargeting #2"), so two campaigns whose legacy names differ still differ: the name is the key
// findCampaignByNameInGroup reuses a campaign by, inside a group every slot of the event shares.
// The data pipeline is asked to strip those trailing tokens from positional segment 5 (#587 Q4).
func buildCampaignNameV2(eventName, nameObjective string, n CampaignNaming, in CampaignInput) string {
	funnel := "MoFU"
	if st := nameToken(in.StageToken); st != "" {
		funnel = st
	}
	region := n.Region
	if region == "" {
		region = defaultRegion(in.GeoTargets)
	}
	intent := n.AudienceIntent
	if intent == "" {
		intent = defaultAudienceIntent(in.Tactic, in.Targeting)
	}
	for _, p := range []string{in.Tactic, in.SlotToken} {
		if p = nameToken(p); p != "" {
			intent += " " + p
		}
	}
	return fmt.Sprintf("Events | %s | %s | %s | %s | %s | %s | %s", eventName, region, nameObjective, intent, campaignNameFormat(in), n.BusinessUnit, funnel)
}

// resolveNaming validates in.Naming for CreateCampaign: every value in its closed list and a
// business unit present. The dispatcher has already refused both before the job existed; this is
// the client's own fail-closed check, a PreflightRefusal sent before any request.
func resolveNaming(n CampaignNaming) (CampaignNaming, error) {
	v, err := ValidateNaming(n)
	if err != nil {
		return CampaignNaming{}, domain.WrapPreflightRefusal(err)
	}
	if v.BusinessUnit == "" {
		return CampaignNaming{}, domain.PreflightRefusalf("campaign naming v2 requires a business unit, one of %s", strings.Join(NameBusinessUnits, ", "))
	}
	return v, nil
}
