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

// X line-item audience expansion at create (#564 gap 11): twitterConfig.audienceExpansion, sent as
// the new line item's audience_expansion. The rule lives once, in twitter.NormalizeAudienceExpansion;
// this file adds the deployment gate and the JSON-type check, and turns a refusal into the
// synchronous 400 shape ("invalid campaign config: twitterConfig.audienceExpansion: ...").

// twitterAudienceExpansionEnabled reports whether this deployment opted in. Read per call, like
// twitterCreateTargetingEnabled, so it flips without a rebuild.
func twitterAudienceExpansionEnabled() bool {
	return os.Getenv(constants.EnvTwitterAudienceExpansionEnabled) == "true"
}

// checkTwitterAudienceExpansion refuses an audienceExpansion while the deployment gate is off, or
// one outside BROAD / DEFINED / EXPANDED, and otherwise returns the normalized value ("" when
// unset). Every refusal is a domain.ErrCreateConfigInvalid wrapped as a PreflightRefusal, as
// checkTwitterCreateTargeting's are. A blank value counts as unset, so it never needs the flag.
func checkTwitterAudienceExpansion(cfg twitterConfig) (string, error) {
	if strings.TrimSpace(cfg.AudienceExpansion) == "" {
		return "", nil
	}
	if !twitterAudienceExpansionEnabled() {
		env := constants.EnvTwitterAudienceExpansionEnabled
		return "", domain.WrapPreflightRefusal(fmt.Errorf(
			"%w: twitterConfig: %s disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, twitter.FieldAudienceExpansion, env, twitter.FieldAudienceExpansion, env))
	}
	v, err := twitter.NormalizeAudienceExpansion(cfg.AudienceExpansion)
	if err != nil {
		var tErr *twitter.TargetingError
		if errors.As(err, &tErr) {
			return "", domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig.%s: %s", domain.ErrCreateConfigInvalid, tErr.Field, tErr.Msg))
		}
		return "", domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	return v, nil
}

// checkTwitterAudienceExpansionShape refuses an audienceExpansion that is not a JSON string, so a
// wrong type is the synchronous 400 rather than a job failure. null counts as absent, and the key
// is matched case-insensitively, as encoding/json matches it.
func checkTwitterAudienceExpansionShape(config json.RawMessage) error {
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
		if !strings.EqualFold(k, twitter.FieldAudienceExpansion) || isJSONNull(raw) {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig.%s: must be a string", domain.ErrCreateConfigInvalid, twitter.FieldAudienceExpansion))
		}
	}
	return nil
}
