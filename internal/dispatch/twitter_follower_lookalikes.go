// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// X follower look-alike targeting at create (#564 gap 4c): twitterConfig.followerLookalikeUserIds,
// sent as SIMILAR_TO_FOLLOWERS_OF_USER criteria on the new line item. The rule lives once, in
// twitter.NormalizeFollowerLookalikes; this file adds the deployment gate and the JSON-type check,
// and turns a refusal into the synchronous 400 shape
// ("invalid campaign config: twitterConfig.followerLookalikeUserIds: ..."). Only the SHAPE can be
// checked before create: X has no user lookup, so an id X refuses fails at its criterion POST and
// leaves a retained PAUSED partial (twitter.ErrCreateTargetingIncomplete), like any criterion
// failure. Accepted risk, owner decision 2026-11-14.

// twitterFollowerLookalikesEnabled reports whether this deployment opted in. Read per call, like
// twitterCreateTargetingEnabled, so it flips without a rebuild.
func twitterFollowerLookalikesEnabled() bool {
	return os.Getenv(constants.EnvTwitterFollowerLookalikesEnabled) == "true"
}

// checkTwitterFollowerLookalikes refuses a non-empty followerLookalikeUserIds while the gate is off,
// or one breaking NormalizeFollowerLookalikes, and otherwise returns the normalized ids (nil when
// unset). Every refusal is a domain.ErrCreateConfigInvalid wrapped as a PreflightRefusal.
func checkTwitterFollowerLookalikes(cfg twitterConfig) ([]string, error) {
	if len(cfg.FollowerLookalikeUserIDs) == 0 {
		return nil, nil
	}
	if !twitterFollowerLookalikesEnabled() {
		env := constants.EnvTwitterFollowerLookalikesEnabled
		f := twitter.FieldFollowerLookalikeUserIDs
		return nil, domain.WrapPreflightRefusal(fmt.Errorf(
			"%w: twitterConfig: %s disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, f, env, f, env))
	}
	ids, err := twitter.NormalizeFollowerLookalikes(cfg.FollowerLookalikeUserIDs)
	if err != nil {
		var tErr *twitter.TargetingError
		if errors.As(err, &tErr) {
			return nil, domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig.%s: %s", domain.ErrCreateConfigInvalid, tErr.Field, tErr.Msg))
		}
		return nil, domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	return ids, nil
}

// checkTwitterFollowerLookalikesShape refuses a followerLookalikeUserIds that is not a JSON array
// of strings, so a wrong type is the synchronous 400 rather than a job failure. null counts as
// absent, and the key is matched case-insensitively, as encoding/json matches it.
func checkTwitterFollowerLookalikesShape(config json.RawMessage) error {
	if len(config) == 0 {
		return nil
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(config, &envelope) != nil {
		return nil
	}
	var platformCfg map[string]json.RawMessage
	if json.Unmarshal(envelope["twitterConfig"], &platformCfg) != nil {
		return nil
	}
	for k, raw := range platformCfg {
		if !strings.EqualFold(k, twitter.FieldFollowerLookalikeUserIDs) || isJSONNull(raw) {
			continue
		}
		var list []string
		if json.Unmarshal(raw, &list) != nil {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig.%s: must be an array of strings", domain.ErrCreateConfigInvalid, twitter.FieldFollowerLookalikeUserIDs))
		}
	}
	return nil
}
