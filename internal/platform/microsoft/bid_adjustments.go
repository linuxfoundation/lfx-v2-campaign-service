// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Create-time device, age and gender bid adjustments (#563, gap 8):
// microsoftConfig.deviceBidAdjustments, ageBidAdjustments and genderBidAdjustments.
//
// Microsoft Advertising Campaign Management v13 reference (learn.microsoft.com):
//   - DeviceCriterion (DeviceName Computers | Smartphones | Tablets), AgeCriterion (AgeRange) and
//     GenderCriterion (GenderType Male | Female) are target criteria a BiddableCampaignCriterion
//     carries with a BidMultiplier CriterionBid, added with AddCampaignCriterions, CriterionType
//     "Targets", like the location, radius and day-time criteria. They adjust the bid for that
//     segment; they do not narrow who is reached, except a device at -100, which stops serving on
//     that device.
//   - BidMultiplier range: -100 through 900 for a device, -90 through 900 otherwise.
//
// They are set on the CAMPAIGN (the issue sketched the ad group): the one ad group this service
// creates inherits them, and the campaign-criterion path already has the fail-closed outcome
// rules, so no new partial-result shape is needed.
//
// VERIFICATION LEVEL: none of this has been exercised against a live account, which is why the
// dispatcher keeps it behind MICROSOFT_BID_ADJUSTMENTS_ENABLED. In particular, whether Microsoft
// accepts an ADD of a device criterion on a new campaign (rather than an update of one it created
// itself) is unconfirmed; a refusal would stop the create before the ad group, fail-closed.
// ---------------------------------------------------------------------------

const (
	// FieldDeviceBidAdjustments, FieldAgeBidAdjustments and FieldGenderBidAdjustments are the
	// microsoftConfig keys, named in refusals.
	FieldDeviceBidAdjustments = "deviceBidAdjustments"
	FieldAgeBidAdjustments    = "ageBidAdjustments"
	FieldGenderBidAdjustments = "genderBidAdjustments"

	// MinDeviceBidAdjustment is -100 (stop serving on the device); age and gender stop at
	// MinDemographicBidAdjustment. MaxBidAdjustment is shared.
	MinDeviceBidAdjustment      = -100
	MinDemographicBidAdjustment = -90
	MaxBidAdjustment            = 900

	criterionTypeDevice = "DeviceCriterion"
	criterionTypeAge    = "AgeCriterion"
	criterionTypeGender = "GenderCriterion"
)

// The accepted segment names in Microsoft's spelling and send order. Age is the five adult
// ranges; Unknown is not offered.
var (
	deviceNames = []string{"Computers", "Smartphones", "Tablets"}
	ageRanges   = []string{"EighteenToTwentyFour", "TwentyFiveToThirtyFour", "ThirtyFiveToFortyNine", "FiftyToSixtyFour", "SixtyFiveAndAbove"}
	genderTypes = []string{"Male", "Female"}
)

// BidAdjustments are the requested whole-percent adjustments, keyed by segment name in any casing.
type BidAdjustments struct {
	Device map[string]float64
	Age    map[string]float64
	Gender map[string]float64
}

// BidAdjustmentError is a refusal of one of the three inputs; Field is the microsoftConfig key.
type BidAdjustmentError struct {
	Field string
	Msg   string
}

func (e *BidAdjustmentError) Error() string { return e.Field + ": " + e.Msg }

// BidAdjustment is one validated adjustment: the criterion type, the segment in Microsoft's
// spelling, and the percent.
type BidAdjustment struct {
	CriterionType string
	Segment       string
	Percent       float64
}

// fields names the requested inputs, as the microsoftConfig keys.
func (b BidAdjustments) fields() []string {
	var fields []string
	if len(b.Device) > 0 {
		fields = append(fields, FieldDeviceBidAdjustments)
	}
	if len(b.Age) > 0 {
		fields = append(fields, FieldAgeBidAdjustments)
	}
	if len(b.Gender) > 0 {
		fields = append(fields, FieldGenderBidAdjustments)
	}
	return fields
}

// ValidateBidAdjustments applies the rules without contacting Microsoft, so the dispatcher's
// synchronous preflight and CreateCampaign agree, and returns the adjustments in send order
// (devices, then ages, then genders, each in Microsoft's enum order). A key is matched ignoring
// case; two keys naming the same segment, an unknown segment, or a percent that is not a whole
// number within the range is a *BidAdjustmentError.
func ValidateBidAdjustments(in BidAdjustments) ([]BidAdjustment, error) {
	var out []BidAdjustment
	for _, g := range []struct {
		field, criterion string
		names            []string
		values           map[string]float64
		lo               int
	}{
		{FieldDeviceBidAdjustments, criterionTypeDevice, deviceNames, in.Device, MinDeviceBidAdjustment},
		{FieldAgeBidAdjustments, criterionTypeAge, ageRanges, in.Age, MinDemographicBidAdjustment},
		{FieldGenderBidAdjustments, criterionTypeGender, genderTypes, in.Gender, MinDemographicBidAdjustment},
	} {
		byName := map[string]float64{}
		// Keys are visited sorted so a refusal is deterministic.
		keys := make([]string, 0, len(g.values))
		for k := range g.values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			v := g.values[key]
			name := ""
			for _, n := range g.names {
				if strings.EqualFold(strings.TrimSpace(key), n) {
					name = n
				}
			}
			if name == "" {
				return nil, &BidAdjustmentError{Field: g.field, Msg: fmt.Sprintf("unknown key; use %s", strings.Join(g.names, ", "))}
			}
			if _, dup := byName[name]; dup {
				return nil, &BidAdjustmentError{Field: g.field, Msg: fmt.Sprintf("%s is named twice", name)}
			}
			if math.IsNaN(v) || v != math.Trunc(v) || v < float64(g.lo) || v > MaxBidAdjustment {
				return nil, &BidAdjustmentError{Field: g.field, Msg: fmt.Sprintf("%s must be a whole percent from %d to %d", name, g.lo, MaxBidAdjustment)}
			}
			byName[name] = v
		}
		for _, n := range g.names {
			if v, ok := byName[n]; ok {
				out = append(out, BidAdjustment{CriterionType: g.criterion, Segment: n, Percent: v})
			}
		}
	}
	return out, nil
}

type msDeviceCriterion struct {
	Type       string `json:"Type"`
	DeviceName string `json:"DeviceName"`
}

type msAgeCriterion struct {
	Type     string `json:"Type"`
	AgeRange string `json:"AgeRange"`
}

type msGenderCriterion struct {
	Type       string `json:"Type"`
	GenderType string `json:"GenderType"`
}

// createBidAdjustmentCriterions attaches every adjustment as a BiddableCampaignCriterion with
// its BidMultiplier, in ONE AddCampaignCriterions call with the outcome rules of
// postCampaignCriterions.
func (c *Client) createBidAdjustmentCriterions(ctx context.Context, campaignID string, adjustments []BidAdjustment) ([]string, error) {
	crits := make([]msTargetCampaignCriterion, 0, len(adjustments))
	for _, a := range adjustments {
		var crit any
		switch a.CriterionType {
		case criterionTypeDevice:
			crit = msDeviceCriterion{Type: a.CriterionType, DeviceName: a.Segment}
		case criterionTypeAge:
			crit = msAgeCriterion{Type: a.CriterionType, AgeRange: a.Segment}
		default:
			crit = msGenderCriterion{Type: a.CriterionType, GenderType: a.Segment}
		}
		crits = append(crits, msTargetCampaignCriterion{
			CampaignId:   json.Number(campaignID),
			Criterion:    crit,
			Type:         campaignCriterionTypeBiddable,
			CriterionBid: &msBidMultiplier{Type: criterionBidMultiplier, Multiplier: a.Percent},
		})
	}
	return c.postCampaignCriterions(ctx, campaignID, "bid adjustments", "bid adjustment criteria",
		addTargetCriterionsRequest{CampaignCriterions: crits, CriterionType: criterionTypeTargets}, len(crits))
}

// bidAdjustmentLabel renders the adjustments for a step: "Smartphones -20%, Female +10%".
func bidAdjustmentLabel(adjustments []BidAdjustment) string {
	parts := make([]string, 0, len(adjustments))
	for _, a := range adjustments {
		parts = append(parts, fmt.Sprintf("%s %+d%%", a.Segment, int64(a.Percent)))
	}
	return strings.Join(parts, ", ")
}
