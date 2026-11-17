// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Microsoft search network at create (#563 gap 11): microsoftConfig.searchPartners, a boolean
// sent as the new ad group's AdGroup.Network (true: Microsoft's sites and its syndicated search
// partners; false: Microsoft's own sites only). Omitted, no Network is sent and Microsoft's
// default (partners included) applies, exactly as before. The mapping lives in the client
// (microsoft.CampaignInput.SearchPartners); this file adds the deployment gate and the JSON-type
// check as the synchronous 400 shape.

// fieldMicrosoftSearchPartners is the microsoftConfig key, named in refusals.
const fieldMicrosoftSearchPartners = "searchPartners"

// microsoftSearchNetworkEnabled reports whether this deployment opted in. Read per call, like
// microsoftCreateTargetingEnabled, so it flips without a rebuild.
func microsoftSearchNetworkEnabled() bool {
	return os.Getenv(constants.EnvMicrosoftSearchNetworkEnabled) == "true"
}

// checkMicrosoftSearchNetwork refuses searchPartners while the deployment gate is off. Any
// non-null value counts as set, false included: false is the value that changes what Microsoft
// does. The refusal is a domain.ErrCreateConfigInvalid (a 400 from PreflightCreate) wrapped as a
// PreflightRefusal.
func checkMicrosoftSearchNetwork(cfg microsoftConfig) error {
	if cfg.SearchPartners == nil || microsoftSearchNetworkEnabled() {
		return nil
	}
	env := constants.EnvMicrosoftSearchNetworkEnabled
	return domain.WrapPreflightRefusal(fmt.Errorf(
		"%w: microsoftConfig: %s disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
		domain.ErrCreateConfigInvalid, fieldMicrosoftSearchPartners, env, fieldMicrosoftSearchPartners, env))
}

// checkMicrosoftSearchNetworkShape refuses a searchPartners that is not a JSON boolean before any
// job exists. Keys are matched case-insensitively, as encoding/json matches them, in sorted
// order; null counts as absent.
func checkMicrosoftSearchNetworkShape(config json.RawMessage) error {
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
		if isJSONNull(raw) || !strings.EqualFold(k, fieldMicrosoftSearchPartners) {
			continue
		}
		var b bool
		if json.Unmarshal(raw, &b) != nil {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s: must be a boolean", domain.ErrCreateConfigInvalid, fieldMicrosoftSearchPartners))
		}
	}
	return nil
}
