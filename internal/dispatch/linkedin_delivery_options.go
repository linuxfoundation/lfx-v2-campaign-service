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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// LinkedIn Audience Expansion and Audience Network choices at create time (#667, #552 gap 8):
// linkedInConfig.audienceExpansionEnabled and linkedInConfig.offsiteDeliveryEnabled, sent as the
// campaign's fields of the same names. The format rule lives once, in
// linkedin.ValidateDeliveryOptions; this file adds the deployment gate and the JSON-type check,
// and turns a refusal into the synchronous 400 shape ("invalid campaign config: linkedInConfig...").

// linkedinDeliveryOptionsEnabled reports whether this deployment opted in. Read per call, like
// linkedinCallToActionEnabled, so it flips without a rebuild.
func linkedinDeliveryOptionsEnabled() bool {
	return os.Getenv(constants.EnvLinkedInAdsDeliveryOptionsEnabled) == "true"
}

// linkedinDeliveryOptions refuses either option while the deployment gate is off, and an option
// linkedin.ValidateDeliveryOptions refuses for the create's format (the Audience Network on a
// Text Ad campaign). An option counts as set when it is present with a boolean value, false
// included: the gate never depends on whether the value happens to equal the default. Every
// refusal is a domain.ErrCreateConfigInvalid (a 400 from PreflightCreate) wrapped as a
// PreflightRefusal (a job that reached Dispatch copies it verbatim).
func linkedinDeliveryOptions(cfg linkedinConfig) (linkedin.DeliveryOptions, error) {
	opts := linkedin.DeliveryOptions{
		AudienceExpansionEnabled: cfg.AudienceExpansionEnabled,
		OffsiteDeliveryEnabled:   cfg.OffsiteDeliveryEnabled,
	}
	if !opts.Set() {
		return opts, nil
	}
	if !linkedinDeliveryOptionsEnabled() {
		var set []string
		if opts.AudienceExpansionEnabled != nil {
			set = append(set, linkedin.FieldAudienceExpansionEnabled)
		}
		if opts.OffsiteDeliveryEnabled != nil {
			set = append(set, linkedin.FieldOffsiteDeliveryEnabled)
		}
		env := constants.EnvLinkedInAdsDeliveryOptionsEnabled
		fields := "linkedInConfig." + strings.Join(set, " and linkedInConfig.")
		return linkedin.DeliveryOptions{}, domain.WrapPreflightRefusal(fmt.Errorf(
			"%w: %s disabled on this deployment (%s is not \"true\"); omit it, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, fields, env, env))
	}
	if err := linkedin.ValidateDeliveryOptions(opts, cfg.Format); err != nil {
		return linkedin.DeliveryOptions{}, domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	return opts, nil
}

// checkDeliveryOptionsShape refuses an audienceExpansionEnabled or offsiteDeliveryEnabled that is
// not a JSON boolean, so a wrong type is the synchronous 400 rather than a value the best-effort
// pre-flight decode skips (and the gate then never sees). null counts as absent, as encoding/json
// reads it. Keys are matched case-insensitively, as encoding/json matches them, and visited in
// sorted order so the refusal is deterministic.
func checkDeliveryOptionsShape(config json.RawMessage) error {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(config, &envelope) != nil {
		return nil
	}
	var platformCfg map[string]json.RawMessage
	if json.Unmarshal(envelope["linkedInConfig"], &platformCfg) != nil {
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
		for _, field := range []string{linkedin.FieldAudienceExpansionEnabled, linkedin.FieldOffsiteDeliveryEnabled} {
			var b bool
			if strings.EqualFold(k, field) && json.Unmarshal(raw, &b) != nil {
				return domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig.%s: must be true or false", domain.ErrCreateConfigInvalid, field))
			}
		}
	}
	return nil
}
