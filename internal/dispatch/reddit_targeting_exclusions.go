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

// Reddit targeting exclusions at create time (#549): redditConfig.excludedSubreddits and
// redditConfig.excludedGeoTargets, sent as the ad group's excluded_communities and
// excluded_geolocations. The rules live once, in reddit.NormalizeTargetingExclusions; this file
// adds the deployment gate and the JSON-type check, and turns a refusal into the synchronous 400
// shape ("invalid campaign config: redditConfig.<field>: ...").

// redditTargetingExclusionsEnabled reports whether this deployment opted in. Read per call, like
// redditKeywordTargetingWritesEnabled, so it flips without a rebuild.
func redditTargetingExclusionsEnabled() bool {
	return os.Getenv(constants.EnvRedditTargetingExclusionsEnabled) == "true"
}

// redditExclusionFields are the gated redditConfig keys, in config order.
var redditExclusionFields = []string{reddit.FieldExcludedSubreddits, reddit.FieldExcludedGeoTargets}

// checkRedditTargetingExclusions refuses a config that sets either exclusion list while the
// deployment gate is off, or whose lists break NormalizeTargetingExclusions. Every refusal is a
// domain.ErrCreateConfigInvalid (a 400 from PreflightCreate) wrapped as a PreflightRefusal (so a
// job that reached Dispatch copies the sentence verbatim). Any non-empty list counts as set, so
// the gate never depends on the values' own validity.
func checkRedditTargetingExclusions(cfg redditConfig) error {
	var set []string
	if len(cfg.ExcludedSubreddits) > 0 {
		set = append(set, reddit.FieldExcludedSubreddits)
	}
	if len(cfg.ExcludedGeoTargets) > 0 {
		set = append(set, reddit.FieldExcludedGeoTargets)
	}
	if len(set) == 0 {
		return nil
	}
	if !redditTargetingExclusionsEnabled() {
		env := constants.EnvRedditTargetingExclusionsEnabled
		fields := strings.Join(set, " and ")
		return domain.WrapPreflightRefusal(fmt.Errorf(
			"%w: redditConfig: %s disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, fields, env, fields, env))
	}
	if _, err := reddit.NormalizeTargetingExclusions(cfg.Subreddits, cfg.GeoTargets, cfg.ExcludedSubreddits, cfg.ExcludedGeoTargets); err != nil {
		var exErr *reddit.ExclusionError
		if errors.As(err, &exErr) {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig.%s: %s", domain.ErrCreateConfigInvalid, exErr.Field, exErr.Msg))
		}
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	return nil
}

// checkRedditExclusionsShape refuses an exclusion list that is not a JSON array of strings, so a
// wrong type is the synchronous 400 rather than a job failure (encoding/json would otherwise fail
// the whole redditConfig decode, which PreflightCreate leaves to Dispatch). null counts as absent,
// as encoding/json reads it. Keys are matched case-insensitively, as encoding/json matches them,
// so a case variant cannot slip a wrong type past this check; they are visited in sorted order so
// the refusal is deterministic.
func checkRedditExclusionsShape(config json.RawMessage) error {
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
		for _, field := range redditExclusionFields {
			if !strings.EqualFold(k, field) || isJSONNull(raw) {
				continue
			}
			var list []string
			if json.Unmarshal(raw, &list) != nil {
				return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig.%s: must be an array of strings", domain.ErrCreateConfigInvalid, field))
			}
		}
	}
	return nil
}
