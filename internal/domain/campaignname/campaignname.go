// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package campaignname holds the platform-independent half of campaign naming v2 (#587): the
// closed lists a v2 name's caller-chosen segments come from, the validator that canonicalises a
// caller's choice, the request-side checks every paid-social dispatcher applies to its
// <platform>Config.naming object, and the composer of LF's 8-segment name,
//
//	Events | <event> | <Region> | <Objective> | <AudienceIntent>[ <tactic>][ #<n>] | <Format> | <BusinessUnit> | <Funnel>
//
// Each platform client decides only what is platform-specific: its objective label, its format
// (from the creative it creates), its region default (from its own geo fields) and its retry
// safety. Every value below comes from a CLOSED list, never free text: a name segment is a
// reporting dimension, and the data pipeline's fallback parsers match words anywhere in a name.
//
// The business unit of an EVENT campaign is the foundation that owns the event (KubeCon →
// "Cloud Native Computing Foundation (CNCF)"); "LF Events" is only for an event the Linux
// Foundation runs with no foundation behind it. The service holds no project→business-unit
// mapping, so the caller supplies it (owner decision, 2026-11-14).
package campaignname

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
)

// Regions a v2 name's Region segment may carry. The buckets are Meta's and Reddit's
// (CountryRegions), so one event reports one region across platforms. lf-dbt's keyword fallback
// (parse_campaign_region) knows NA, EMEA, APAC and Global only, but a well-formed 8-segment name
// is read positionally, so LATAM and India are reported as written.
var Regions = []string{"NA", "EMEA", "APAC", "LATAM", "India", "Global"}

// RegionGlobal is the region of a campaign whose geo targets name no single bucket.
const RegionGlobal = "Global"

// Audience intents a v2 name's segment 5 may lead with. Each platform derives a default when the
// caller sends none; Intent (in-market, high-intent audiences) is a default only where the
// platform's legacy name already said so (Meta, Reddit).
var AudienceIntents = []string{IntentIntent, IntentProspecting, IntentRetargeting}

// The audience intents, by name.
const (
	IntentIntent      = "Intent"
	IntentProspecting = "Prospecting"
	IntentRetargeting = "Retargeting"
)

// Ad formats a v2 name's segment 6 may carry. The format is derived server-side by each platform
// client from the creative it creates and is never sent by a caller, so a retry composes the same
// name.
const (
	FormatStatic   = "Static"   // a single-image (or link-preview) ad
	FormatMotion   = "Motion"   // a video ad
	FormatSocial   = "Social"   // a promoted existing organic post, whose content the service does not read
	FormatText     = "Text"     // a text-only ad (LinkedIn text ads, an X text post)
	FormatCarousel = "Carousel" // a multi-card ad; no client creates one on main yet
)

// BusinessUnits are the values a v2 name's segment 7 may carry: exactly the canonical project
// names lf-dbt resolves to a Salesforce project id. Source, read-only: lf-dbt
// macros/parse_campaign_name.sql at origin/main 0b6b6c15d (unchanged since 2ac0d65dc) — the
// outputs of normalize_project_name, plus the LF umbrella names map_paid_ads_project_id maps to
// the Linux Foundation project id. Each value is a fixed point of normalize_project_name (it
// normalizes to itself) and is listed in map_paid_ads_project_id, so a campaign carrying it lands
// in the right foundation roll-up; linkedin's
// TestNameBusinessUnits_AreFixedPointsOfTheDbtNormalizer pins the first property against a copy
// of the macro.
//
// This is a COPY of a list another repository owns. When lf-dbt adds a project to
// map_paid_ads_project_id, add it here too (or a caller cannot select it); a value removed there
// must be removed here before a campaign reports under a name dbt no longer maps.
var BusinessUnits = []string{
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

// CountryRegions buckets ISO 3166-1 alpha-2 country codes into Regions. It is the GEO_TO_REGION
// table Meta's and Reddit's legacy names already used; a country not listed has no known region.
var CountryRegions = map[string]string{
	"US": "NA", "CA": "NA", "MX": "NA",
	"GB": "EMEA", "DE": "EMEA", "FR": "EMEA", "NL": "EMEA", "SE": "EMEA",
	"CH": "EMEA", "ES": "EMEA", "IT": "EMEA", "AT": "EMEA", "BE": "EMEA", "IL": "EMEA",
	"IN": "India",
	"JP": "APAC", "KR": "APAC", "SG": "APAC", "AU": "APAC", "CN": "APAC", "TW": "APAC", "HK": "APAC",
	"BR": "LATAM",
}

// Naming is the caller's choice of the v2 name's caller-chosen segments (<platform>Config.naming).
// Each is optional at the JSON level; a dispatcher requires a business unit while its platform's
// naming v2 flag is on (ResolveRequest).
type Naming struct {
	Region         string `json:"region,omitempty"`
	AudienceIntent string `json:"audienceIntent,omitempty"`
	BusinessUnit   string `json:"businessUnit,omitempty"`
}

// ErrInvalid marks a Naming refused by Validate. Each platform wraps it in its own sentinel
// ("linkedin-ads: invalid naming"), so errors.Is matches both.
var ErrInvalid = errors.New("invalid naming")

// Validate checks every set field of n against its closed list, matching case-insensitively, and
// returns the copy with each value in its canonical spelling. An empty field is left empty (its
// default is applied when the name is composed). The refusal wraps invalid (the platform's
// sentinel), names the field and the list, and quotes the caller's value through
// domain.CallerValue.
func Validate(n Naming, invalid error) (Naming, error) {
	var err error
	if n.Region, err = canonical(invalid, "region", n.Region, Regions); err != nil {
		return Naming{}, err
	}
	if n.AudienceIntent, err = canonical(invalid, "audienceIntent", n.AudienceIntent, AudienceIntents); err != nil {
		return Naming{}, err
	}
	if n.BusinessUnit, err = canonical(invalid, "businessUnit", n.BusinessUnit, BusinessUnits); err != nil {
		return Naming{}, err
	}
	return n, nil
}

func canonical(invalid error, field, value string, list []string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", nil
	}
	for _, c := range list {
		if strings.EqualFold(v, c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("%w: %s %s is not one of %s", invalid, field, domain.CallerValue(value), strings.Join(list, ", "))
}

// RequireForCreate is a platform client's own fail-closed check of the Naming it composes a name
// from: every value in its closed list and a business unit present. The dispatcher has already
// refused both before the job existed; this is a PreflightRefusal sent before any request.
func RequireForCreate(n Naming, invalid error) (Naming, error) {
	v, err := Validate(n, invalid)
	if err != nil {
		return Naming{}, domain.WrapPreflightRefusal(err)
	}
	if v.BusinessUnit == "" {
		return Naming{}, domain.PreflightRefusalf("campaign naming v2 requires a business unit, one of %s", strings.Join(BusinessUnits, ", "))
	}
	return v, nil
}

// CheckShape refuses a <configKey>.naming whose JSON shape is wrong: an explicit null, a
// non-object, a non-string value, or any key other than region, audienceIntent and businessUnit
// (a misspelled key would otherwise be dropped and the name composed from a default the caller
// did not choose). Anything it cannot attribute to naming is left to the ordinary decode. The
// refusal is an ErrCreateConfigInvalid pre-flight refusal: a synchronous 400 from PreflightCreate
// and a not-created failure from Dispatch.
func CheckShape(config json.RawMessage, configKey string) error {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(config, &envelope) != nil {
		return nil
	}
	var platformCfg map[string]json.RawMessage
	if json.Unmarshal(envelope[configKey], &platformCfg) != nil {
		return nil
	}
	for k, raw := range platformCfg {
		if !strings.EqualFold(k, "naming") {
			continue
		}
		var n Naming
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || dec.Decode(&n) != nil {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: %s.naming must be an object {\"region\": string, \"audienceIntent\": string, \"businessUnit\": string}, each optional", domain.ErrCreateConfigInvalid, configKey))
		}
	}
	return nil
}

// ResolveRequest checks a decoded <configKey>.naming before any job or request and returns what
// the platform client composes the name from: nil while the platform's naming v2 flag is off (the
// legacy name), else the canonical copy. Every value outside its closed list is refused whatever
// the flag says; while it is on a business unit is required, because no default can be derived
// from an LFX project slug without guessing a foundation. Both are ErrCreateConfigInvalid
// pre-flight refusals, so PreflightCreate answers them with a synchronous 400 and Dispatch fails
// the job with the same sentence. invalid is the platform's sentinel; flagEnv names the flag in
// the refusal.
func ResolveRequest(configKey string, n *Naming, invalid error, enabled bool, flagEnv string) (*Naming, error) {
	var v Naming
	if n != nil {
		c, err := Validate(*n, invalid)
		if err != nil {
			return nil, domain.WrapPreflightRefusal(fmt.Errorf("%w: %s.naming: %w", domain.ErrCreateConfigInvalid, configKey, err))
		}
		v = c
	}
	if !enabled {
		return nil, nil
	}
	if v.BusinessUnit == "" {
		return nil, domain.WrapPreflightRefusal(fmt.Errorf("%w: %s.naming.businessUnit is required on this deployment (%s is \"true\"); send one of %s",
			domain.ErrCreateConfigInvalid, configKey, flagEnv, strings.Join(BusinessUnits, ", ")))
	}
	return &v, nil
}

// UnanimousRegion is the Region a v2 name carries when the caller sends none: the one region every
// geo target falls in, else Global. regions holds each target's bucket, "" for a target the
// platform cannot bucket. A campaign spanning two buckets (US and Germany), a target with no
// bucket, and no target at all are all Global. Unlike the legacy Meta and Reddit first-geo rule,
// a campaign is never reported under its first country alone; a caller who knows better sends
// naming.region.
func UnanimousRegion(regions []string) string {
	region := ""
	for _, r := range regions {
		if r == "" || (region != "" && r != region) {
			return RegionGlobal
		}
		region = r
	}
	if region == "" {
		return RegionGlobal
	}
	return region
}

// CountriesRegion applies UnanimousRegion to ISO 3166-1 alpha-2 country codes, bucketed by
// CountryRegions (matched case-insensitively, surrounding space ignored).
func CountriesRegion(codes []string) string {
	regions := make([]string, len(codes))
	for i, c := range codes {
		regions[i] = CountryRegions[strings.ToUpper(strings.TrimSpace(c))]
	}
	return UnanimousRegion(regions)
}

// Token trims and pipe-strips one label token, so it can never add a name segment.
func Token(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "|", "-"))
}

// IntentSegment is segment 5: the audience intent, then each non-empty token (the tactic and a
// later slot's "#<n>") appended after a space, exactly as the legacy names append them
// ("Retargeting retargeting #2"). The tokens keep two campaigns whose legacy names differ
// distinct, because a name is the key some platforms reuse a campaign by; the data pipeline is
// asked to strip them from positional segment 5 (#587 migration note).
func IntentSegment(intent string, tokens ...string) string {
	for _, p := range tokens {
		if p = Token(p); p != "" {
			intent += " " + p
		}
	}
	return intent
}

// Compose joins the eight segments of a v2 name. Each argument arrives already trimmed and
// pipe-stripped (event) or from a closed list.
func Compose(event, region, objective, intentSegment, format, businessUnit, funnel string) string {
	return fmt.Sprintf("Events | %s | %s | %s | %s | %s | %s | %s", event, region, objective, intentSegment, format, businessUnit, funnel)
}
