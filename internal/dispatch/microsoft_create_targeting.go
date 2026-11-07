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

// Microsoft create-time excluded countries, radius targets and campaign languages (#563, gaps 5
// and 6): microsoftConfig.excludedGeoTargets, proximityTargets and languages. The rules live once,
// in microsoft.ValidateCreateTargeting; this file adds the deployment gate and the JSON-type check
// and turns a refusal into the synchronous 400 shape ("invalid campaign config:
// microsoftConfig.<field>: ..."). Resolving the excluded codes and attaching the criteria happen in
// the client (CreateCampaign), still before the ad group, under geoTargets' fail-closed rules.

// microsoftProximityConfig is one microsoftConfig.proximityTargets entry.
type microsoftProximityConfig struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Radius    float64 `json:"radius"`
	Unit      string  `json:"unit"`
	Name      string  `json:"name"`
}

// microsoftCreateTargetingEnabled reports whether this deployment opted in. Read per call, like
// microsoftAudiencesEnabled, so it flips without a rebuild.
func microsoftCreateTargetingEnabled() bool {
	return os.Getenv(constants.EnvMicrosoftCreateTargetingEnabled) == "true"
}

func microsoftProximityTargets(in []microsoftProximityConfig) []microsoft.ProximityTarget {
	if len(in) == 0 {
		return nil
	}
	out := make([]microsoft.ProximityTarget, len(in))
	for i, p := range in {
		out[i] = microsoft.ProximityTarget{Latitude: p.Latitude, Longitude: p.Longitude, Radius: p.Radius, Unit: p.Unit, Name: p.Name}
	}
	return out
}

// checkMicrosoftCreateTargeting refuses a config that sets any of the three while the deployment
// gate is off, or whose values break microsoft.ValidateCreateTargeting. Every refusal is a
// domain.ErrCreateConfigInvalid (a 400 from PreflightCreate) wrapped as a PreflightRefusal. Any
// non-empty list counts as set, so the gate never depends on the values' own validity.
func checkMicrosoftCreateTargeting(cfg microsoftConfig) error {
	var set []string
	if len(cfg.ExcludedGeoTargets) > 0 {
		set = append(set, microsoft.FieldExcludedGeoTargets)
	}
	if len(cfg.ProximityTargets) > 0 {
		set = append(set, microsoft.FieldProximityTargets)
	}
	if len(cfg.Languages) > 0 {
		set = append(set, microsoft.FieldLanguages)
	}
	if len(set) == 0 {
		return nil
	}
	if !microsoftCreateTargetingEnabled() {
		env := constants.EnvMicrosoftCreateTargetingEnabled
		fields := strings.Join(set, ", ")
		return domain.WrapPreflightRefusal(fmt.Errorf(
			"%w: microsoftConfig: %s disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, fields, env, fields, env))
	}
	if _, err := microsoft.ValidateCreateTargeting(cfg.GeoTargets, cfg.ExcludedGeoTargets, microsoftProximityTargets(cfg.ProximityTargets), cfg.Languages); err != nil {
		var tErr *microsoft.CreateTargetingError
		if errors.As(err, &tErr) {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s: %s", domain.ErrCreateConfigInvalid, tErr.Field, tErr.Msg))
		}
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	return nil
}

// checkMicrosoftCreateTargetingShape refuses a list of the wrong JSON type before any job exists:
// excludedGeoTargets and languages must be arrays of strings, proximityTargets an array of
// objects with numeric latitude, longitude and radius and string unit and name. Keys are matched
// case-insensitively, as encoding/json matches them, in sorted order; null counts as absent.
func checkMicrosoftCreateTargetingShape(config json.RawMessage) error {
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
		switch {
		case strings.EqualFold(k, microsoft.FieldExcludedGeoTargets), strings.EqualFold(k, microsoft.FieldLanguages):
			field := microsoft.FieldExcludedGeoTargets
			if strings.EqualFold(k, microsoft.FieldLanguages) {
				field = microsoft.FieldLanguages
			}
			var list []string
			if json.Unmarshal(raw, &list) != nil {
				return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s: must be an array of strings", domain.ErrCreateConfigInvalid, field))
			}
		case strings.EqualFold(k, microsoft.FieldProximityTargets):
			var list []microsoftProximityConfig
			if json.Unmarshal(raw, &list) != nil {
				return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s: must be an array of objects with numeric latitude, longitude and radius and string unit and name", domain.ErrCreateConfigInvalid, microsoft.FieldProximityTargets))
			}
		}
	}
	return nil
}
