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

// Caller responsive search ad copy at create time (#563, gap 1): microsoftConfig.headlines,
// descriptions, path1 and path2, sent on the ResponsiveSearchAd instead of the auto-composed copy.
// The rules live once, in microsoft.ValidateCallerAdCopy; this file adds the deployment gate and
// the JSON-type check, and turns a refusal into the synchronous 400 shape
// ("invalid campaign config: microsoftConfig.<field>: ...").

// microsoftAdCopyEnabled reports whether this deployment opted in. Read per call, like
// redditTargetingExclusionsEnabled, so it flips without a rebuild.
func microsoftAdCopyEnabled() bool {
	return os.Getenv(constants.EnvMicrosoftAdCopyEnabled) == "true"
}

// microsoftAdCopyListFields / microsoftAdCopyStringFields are the gated microsoftConfig keys.
var (
	microsoftAdCopyListFields   = []string{microsoft.FieldHeadlines, microsoft.FieldDescriptions}
	microsoftAdCopyStringFields = []string{microsoft.FieldPath1, microsoft.FieldPath2}
)

// microsoftAdCopySet lists, in config order, the copy fields cfg sets. A non-empty list or a
// non-empty string counts as set, so the gate never depends on the values' own validity.
func microsoftAdCopySet(cfg microsoftConfig) []string {
	var set []string
	if len(cfg.Headlines) > 0 {
		set = append(set, microsoft.FieldHeadlines)
	}
	if len(cfg.Descriptions) > 0 {
		set = append(set, microsoft.FieldDescriptions)
	}
	if cfg.Path1 != "" {
		set = append(set, microsoft.FieldPath1)
	}
	if cfg.Path2 != "" {
		set = append(set, microsoft.FieldPath2)
	}
	return set
}

// checkMicrosoftAdCopy refuses a config that sets any copy field while the deployment gate is
// off, or whose copy breaks microsoft.ValidateCallerAdCopy. Every refusal is a
// domain.ErrCreateConfigInvalid (a 400 from PreflightCreate) wrapped as a PreflightRefusal (so a
// job that reached Dispatch copies the sentence verbatim).
func checkMicrosoftAdCopy(cfg microsoftConfig) error {
	set := microsoftAdCopySet(cfg)
	if len(set) == 0 {
		return nil
	}
	if !microsoftAdCopyEnabled() {
		env := constants.EnvMicrosoftAdCopyEnabled
		fields := strings.Join(set, ", ")
		return domain.WrapPreflightRefusal(fmt.Errorf(
			"%w: microsoftConfig: %s disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, fields, env, fields, env))
	}
	if err := microsoft.ValidateCallerAdCopy(cfg.Headlines, cfg.Descriptions, cfg.Path1, cfg.Path2); err != nil {
		var cErr *microsoft.AdCopyError
		if errors.As(err, &cErr) {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s: %s", domain.ErrCreateConfigInvalid, cErr.Field, cErr.Msg))
		}
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	return nil
}

// checkMicrosoftAdCopyShape refuses a copy field of the wrong JSON type, so a wrong type is the
// synchronous 400 rather than a job failure (encoding/json would otherwise fail the whole
// microsoftConfig decode, which PreflightCreate leaves to Dispatch). null needs no special case:
// it decodes into a []string or a string without error, as absent. Keys are matched case-insensitively, as encoding/json matches them, and
// visited in sorted order so the refusal is deterministic.
func checkMicrosoftAdCopyShape(config json.RawMessage) error {
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
		for _, field := range microsoftAdCopyListFields {
			if !strings.EqualFold(k, field) {
				continue
			}
			var list []string
			if json.Unmarshal(raw, &list) != nil {
				return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s: must be an array of strings", domain.ErrCreateConfigInvalid, field))
			}
		}
		for _, field := range microsoftAdCopyStringFields {
			if !strings.EqualFold(k, field) {
				continue
			}
			var s string
			if json.Unmarshal(raw, &s) != nil {
				return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s: must be a string", domain.ErrCreateConfigInvalid, field))
			}
		}
	}
	return nil
}
