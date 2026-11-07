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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// X create-time location and language targeting (#564): twitterConfig.geoTargets and
// twitterConfig.languages, sent as LOCATION and LANGUAGE criteria on the new line item. The rules
// live once, in twitter.NormalizeCreateTargeting; this file adds the deployment gate and the
// JSON-type check, and turns a refusal into the synchronous 400 shape
// ("invalid campaign config: twitterConfig.<field>: ..."). Resolving the values against X's
// lookups needs the connection, so it happens at dispatch (twitter.Client.CreateCampaign), still
// before anything is created.

// twitterCreateTargetingEnabled reports whether this deployment opted in. Read per call, like
// redditTargetingExclusionsEnabled, so it flips without a rebuild.
func twitterCreateTargetingEnabled() bool {
	return os.Getenv(constants.EnvTwitterCreateTargetingEnabled) == "true"
}

// twitterTargetingFields are the gated twitterConfig keys, in config order.
var twitterTargetingFields = []string{twitter.FieldGeoTargets, twitter.FieldLanguages}

// checkTwitterCreateTargeting refuses a config that sets either list while the deployment gate is
// off, or whose lists break NormalizeCreateTargeting, and otherwise returns the normalized lists.
// Every refusal is a domain.ErrCreateConfigInvalid (a 400 from PreflightCreate) wrapped as a
// PreflightRefusal (so a job that reached Dispatch copies the sentence verbatim). Any non-empty
// list counts as set, so the gate never depends on the values' own validity.
func checkTwitterCreateTargeting(cfg twitterConfig) (geo, langs []string, err error) {
	var set []string
	if len(cfg.GeoTargets) > 0 {
		set = append(set, twitter.FieldGeoTargets)
	}
	if len(cfg.Languages) > 0 {
		set = append(set, twitter.FieldLanguages)
	}
	if len(set) == 0 {
		return nil, nil, nil
	}
	if !twitterCreateTargetingEnabled() {
		env := constants.EnvTwitterCreateTargetingEnabled
		fields := strings.Join(set, " and ")
		return nil, nil, domain.WrapPreflightRefusal(fmt.Errorf(
			"%w: twitterConfig: %s disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, fields, env, fields, env))
	}
	geo, langs, err = twitter.NormalizeCreateTargeting(cfg.GeoTargets, cfg.Languages)
	if err != nil {
		var tErr *twitter.TargetingError
		if errors.As(err, &tErr) {
			return nil, nil, domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig.%s: %s", domain.ErrCreateConfigInvalid, tErr.Field, tErr.Msg))
		}
		return nil, nil, domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	return geo, langs, nil
}

// checkTwitterTargetingShape refuses a targeting list that is not a JSON array of strings, so a
// wrong type is the synchronous 400 rather than a job failure (encoding/json would otherwise fail
// the whole twitterConfig decode, which PreflightCreate leaves to Dispatch). null counts as absent,
// as encoding/json reads it. Keys are matched case-insensitively, as encoding/json matches them,
// and visited in sorted order so the refusal is deterministic.
func checkTwitterTargetingShape(config json.RawMessage) error {
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
	keys := make([]string, 0, len(platformCfg))
	for k := range platformCfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		raw := platformCfg[k]
		for _, field := range twitterTargetingFields {
			if !strings.EqualFold(k, field) || isJSONNull(raw) {
				continue
			}
			var list []string
			if json.Unmarshal(raw, &list) != nil {
				return domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig.%s: must be an array of strings", domain.ErrCreateConfigInvalid, field))
			}
		}
	}
	return nil
}
