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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Reddit placement, platform and expansion choices at create time (#549 gap 7):
// redditConfig.placements, redditConfig.platforms and redditConfig.expandTargeting, sent as the
// ad group's targeting.locations, targeting.platforms and targeting.expand_targeting. The rules
// live once, in reddit.NormalizePlacementTargeting; this file adds the deployment gate and the
// JSON-type check, and turns a refusal into the synchronous 400 shape
// ("invalid campaign config: redditConfig.<field>: ...").

// redditPlacementTargetingEnabled reports whether this deployment opted in. Read per call, like
// redditTargetingExclusionsEnabled, so it flips without a rebuild.
func redditPlacementTargetingEnabled() bool {
	return os.Getenv(constants.EnvRedditPlacementTargetingEnabled) == "true"
}

// checkRedditPlacementTargeting refuses a config that sets any of the three while the deployment
// gate is off, or whose values break reddit.NormalizePlacementTargeting. A list counts as set when
// it is non-empty (an empty list means the default, as omitting it does), and expandTargeting when
// it is present with a boolean value, true included: the gate never depends on whether the value
// happens to equal the default. Every refusal is a domain.ErrCreateConfigInvalid (a 400 from
// PreflightCreate) wrapped as a PreflightRefusal (a job that reached Dispatch copies it verbatim).
func checkRedditPlacementTargeting(cfg redditConfig) error {
	var set []string
	if len(cfg.Placements) > 0 {
		set = append(set, reddit.FieldPlacements)
	}
	if len(cfg.Platforms) > 0 {
		set = append(set, reddit.FieldPlatforms)
	}
	if cfg.ExpandTargeting != nil {
		set = append(set, reddit.FieldExpandTargeting)
	}
	if len(set) == 0 {
		return nil
	}
	if !redditPlacementTargetingEnabled() {
		env := constants.EnvRedditPlacementTargetingEnabled
		fields := strings.Join(set, " and ")
		return domain.WrapPreflightRefusal(fmt.Errorf(
			"%w: redditConfig: %s disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, fields, env, fields, env))
	}
	if _, err := reddit.NormalizePlacementTargeting(cfg.Placements, cfg.Platforms, cfg.ExpandTargeting); err != nil {
		var pe *reddit.PlacementError
		if errors.As(err, &pe) {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig.%s: %s", domain.ErrCreateConfigInvalid, pe.Field, pe.Msg))
		}
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	return nil
}

// checkRedditPlacementShape refuses a placements or platforms value that is not a JSON array of
// strings, and an expandTargeting that is not a boolean, so a wrong type is the synchronous 400
// rather than a job failure (encoding/json would otherwise fail the whole redditConfig decode,
// which PreflightCreate leaves to Dispatch). null counts as absent, as encoding/json reads it.
// Keys are matched case-insensitively, as encoding/json matches them, and visited in sorted order
// so the refusal is deterministic.
func checkRedditPlacementShape(config json.RawMessage) error {
	if len(config) == 0 {
		return nil
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(config, &envelope) != nil {
		return nil
	}
	var platformCfg map[string]json.RawMessage
	if json.Unmarshal(envelope["redditConfig"], &platformCfg) != nil {
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
		case strings.EqualFold(k, reddit.FieldPlacements), strings.EqualFold(k, reddit.FieldPlatforms):
			var list []string
			if json.Unmarshal(raw, &list) != nil {
				field := reddit.FieldPlacements
				if strings.EqualFold(k, reddit.FieldPlatforms) {
					field = reddit.FieldPlatforms
				}
				return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig.%s: must be an array of strings", domain.ErrCreateConfigInvalid, field))
			}
		case strings.EqualFold(k, reddit.FieldExpandTargeting):
			var b bool
			if json.Unmarshal(raw, &b) != nil {
				return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig.%s: must be true or false", domain.ErrCreateConfigInvalid, reddit.FieldExpandTargeting))
			}
		}
	}
	return nil
}
