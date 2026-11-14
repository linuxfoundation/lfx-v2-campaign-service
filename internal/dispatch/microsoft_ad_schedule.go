// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Microsoft create-time ad schedule (#563 gap 10): microsoftConfig.adSchedules. The rules live
// once, in microsoft.ValidateAdSchedules; this file adds the deployment gate, the JSON-type check
// and the required/whole-hour checks a decoded zero would hide, and turns a refusal into the
// synchronous 400 shape. Attaching the criteria happens in the client (CreateCampaign), before the
// ad group, under the radius targets' fail-closed rules.

// microsoftAdScheduleConfig is one microsoftConfig.adSchedules entry. The hours are pointers so a
// missing one is refused rather than read as 0 (midnight, a real hour), and floats so a
// fractional hour gets its own refusal rather than the generic type error.
type microsoftAdScheduleConfig struct {
	Day           string   `json:"day"`
	StartHour     *float64 `json:"startHour"`
	EndHour       *float64 `json:"endHour"`
	BidAdjustment float64  `json:"bidAdjustment"`
}

// microsoftAdScheduleEnabled reports whether this deployment opted in. Read per call, like
// microsoftCreateTargetingEnabled, so it flips without a rebuild.
func microsoftAdScheduleEnabled() bool {
	return os.Getenv(constants.EnvMicrosoftAdScheduleEnabled) == "true"
}

func microsoftAdScheduleInvalid(format string, args ...any) error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s: %s", domain.ErrCreateConfigInvalid, microsoft.FieldAdSchedules, fmt.Sprintf(format, args...)))
}

// microsoftAdSchedules converts the checked config; checkMicrosoftAdSchedule refuses a missing or
// fractional hour before this is reached.
func microsoftAdSchedules(in []microsoftAdScheduleConfig) []microsoft.AdSchedule {
	if len(in) == 0 {
		return nil
	}
	out := make([]microsoft.AdSchedule, len(in))
	for i, s := range in {
		var start, end int
		if s.StartHour != nil {
			start = int(*s.StartHour)
		}
		if s.EndHour != nil {
			end = int(*s.EndHour)
		}
		out[i] = microsoft.AdSchedule{Day: s.Day, StartHour: start, EndHour: end, BidAdjustment: s.BidAdjustment}
	}
	return out
}

// checkMicrosoftAdSchedule refuses adSchedules while the deployment gate is off, or when an entry
// lacks an hour, has a fractional one, or breaks microsoft.ValidateAdSchedules. Any non-empty list
// counts as set, so the gate never depends on the values' own validity.
func checkMicrosoftAdSchedule(cfg microsoftConfig) error {
	if len(cfg.AdSchedules) == 0 {
		return nil
	}
	if !microsoftAdScheduleEnabled() {
		env := constants.EnvMicrosoftAdScheduleEnabled
		return domain.WrapPreflightRefusal(fmt.Errorf(
			"%w: microsoftConfig: %s disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, microsoft.FieldAdSchedules, env, microsoft.FieldAdSchedules, env))
	}
	for i, s := range cfg.AdSchedules {
		for _, h := range []struct {
			name string
			v    *float64
		}{{"startHour", s.StartHour}, {"endHour", s.EndHour}} {
			switch {
			case h.v == nil:
				return microsoftAdScheduleInvalid("entry %d %s is required", i+1, h.name)
			case math.IsNaN(*h.v) || *h.v != math.Trunc(*h.v) || math.Abs(*h.v) > 1000:
				return microsoftAdScheduleInvalid("entry %d %s must be a whole hour", i+1, h.name)
			}
		}
	}
	if _, err := microsoft.ValidateAdSchedules(microsoftAdSchedules(cfg.AdSchedules)); err != nil {
		var sErr *microsoft.AdScheduleError
		if errors.As(err, &sErr) {
			return microsoftAdScheduleInvalid("%s", sErr.Msg)
		}
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	return nil
}

// checkMicrosoftAdScheduleShape refuses an adSchedules that is not an array of objects with a
// string day and numeric hours and bidAdjustment, before any job exists. Keys are matched
// case-insensitively, as encoding/json matches them, in sorted order; null counts as absent.
func checkMicrosoftAdScheduleShape(config json.RawMessage) error {
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
		if isJSONNull(raw) || !strings.EqualFold(k, microsoft.FieldAdSchedules) {
			continue
		}
		var list []microsoftAdScheduleConfig
		if json.Unmarshal(raw, &list) != nil {
			return microsoftAdScheduleInvalid("must be an array of objects with a string day and numeric startHour, endHour and bidAdjustment")
		}
	}
	return nil
}
