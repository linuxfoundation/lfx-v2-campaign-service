// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Microsoft create-time device, age and gender bid adjustments (#563 gap 8):
// microsoftConfig.deviceBidAdjustments, ageBidAdjustments and genderBidAdjustments, each an object
// of segment name to whole percent. The rules live once, in microsoft.ValidateBidAdjustments; this
// file adds the deployment gate and the JSON-type check and turns a refusal into the synchronous
// 400 shape. Attaching the criteria happens in the client (CreateCampaign), before the ad group,
// under the other campaign criteria's fail-closed rules.

// microsoftBidAdjustmentsEnabled reports whether this deployment opted in. Read per call, like
// microsoftCreateTargetingEnabled, so it flips without a rebuild.
func microsoftBidAdjustmentsEnabled() bool {
	return os.Getenv(constants.EnvMicrosoftBidAdjustmentsEnabled) == "true"
}

func microsoftBidAdjustments(cfg microsoftConfig) microsoft.BidAdjustments {
	return microsoft.BidAdjustments{Device: cfg.DeviceBidAdjustments, Age: cfg.AgeBidAdjustments, Gender: cfg.GenderBidAdjustments}
}

// checkMicrosoftBidAdjustments refuses any of the three while the deployment gate is off, or a
// value microsoft.ValidateBidAdjustments refuses. Any non-empty object counts as set, so the gate
// never depends on the values' own validity.
func checkMicrosoftBidAdjustments(cfg microsoftConfig) error {
	var set []string
	if len(cfg.DeviceBidAdjustments) > 0 {
		set = append(set, microsoft.FieldDeviceBidAdjustments)
	}
	if len(cfg.AgeBidAdjustments) > 0 {
		set = append(set, microsoft.FieldAgeBidAdjustments)
	}
	if len(cfg.GenderBidAdjustments) > 0 {
		set = append(set, microsoft.FieldGenderBidAdjustments)
	}
	if len(set) == 0 {
		return nil
	}
	if !microsoftBidAdjustmentsEnabled() {
		env := constants.EnvMicrosoftBidAdjustmentsEnabled
		fields := strings.Join(set, ", ")
		return domain.WrapPreflightRefusal(fmt.Errorf(
			"%w: microsoftConfig: %s disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, fields, env, fields, env))
	}
	if _, err := microsoft.ValidateBidAdjustments(microsoftBidAdjustments(cfg)); err != nil {
		var bErr *microsoft.BidAdjustmentError
		if errors.As(err, &bErr) {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s: %s", domain.ErrCreateConfigInvalid, bErr.Field, bErr.Msg))
		}
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	return nil
}

// checkMicrosoftBidAdjustmentsShape refuses any of the three that is not a JSON object of numbers,
// before any job exists. Keys are matched case-insensitively, as encoding/json matches them, in
// sorted order; null counts as absent.
func checkMicrosoftBidAdjustmentsShape(config json.RawMessage) error {
	if len(config) == 0 {
		return nil
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(config, &envelope) != nil {
		return nil
	}
	var platformCfg map[string]json.RawMessage
	if json.Unmarshal(envelope["microsoftConfig"], &platformCfg) != nil {
		return nil
	}
	keys := make([]string, 0, len(platformCfg))
	for k := range platformCfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		raw := platformCfg[k]
		if isJSONNull(raw) {
			continue
		}
		for _, field := range []string{microsoft.FieldDeviceBidAdjustments, microsoft.FieldAgeBidAdjustments, microsoft.FieldGenderBidAdjustments} {
			if !strings.EqualFold(k, field) {
				continue
			}
			var m map[string]float64
			if json.Unmarshal(raw, &m) != nil {
				return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s: must be an object of segment names to numbers", domain.ErrCreateConfigInvalid, field))
			}
		}
	}
	return nil
}
