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

// Reddit gender, language and dayparting targeting at create time (#549 gap 13):
// redditConfig.gender, redditConfig.languages and redditConfig.schedule. The rules live once, in
// reddit.NormalizeDemographicTargeting; this file adds the deployment gate and the JSON-type
// check, and turns a refusal into the synchronous 400 shape
// ("invalid campaign config: redditConfig.<field>: ...").

// redditDemographicTargetingEnabled reports whether this deployment opted in. Read per call, like
// redditPlacementTargetingEnabled, so it flips without a rebuild.
func redditDemographicTargetingEnabled() bool {
	return os.Getenv(constants.EnvRedditGenderLanguageDaypartingEnabled) == "true"
}

// checkRedditDemographicTargeting refuses a config that sets any of the three while the gate is
// off, or whose values break reddit.NormalizeDemographicTargeting. gender counts as set when it is
// present with a string value (a blank one included, which the rules then refuse); a list counts
// as set when it is non-empty (an empty list means "every language" / "any time", as omitting it
// does). Every refusal is a domain.ErrCreateConfigInvalid (a 400 from PreflightCreate) wrapped as a
// PreflightRefusal (a job that reached Dispatch copies it verbatim).
func checkRedditDemographicTargeting(cfg redditConfig) error {
	var set []string
	if cfg.Gender != nil {
		set = append(set, reddit.FieldGender)
	}
	if len(cfg.Languages) > 0 {
		set = append(set, reddit.FieldLanguages)
	}
	if len(cfg.Schedule) > 0 {
		set = append(set, reddit.FieldSchedule)
	}
	if len(set) == 0 {
		return nil
	}
	if !redditDemographicTargetingEnabled() {
		env := constants.EnvRedditGenderLanguageDaypartingEnabled
		fields := strings.Join(set, " and ")
		return domain.WrapPreflightRefusal(fmt.Errorf(
			"%w: redditConfig: %s disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, fields, env, fields, env))
	}
	if _, err := reddit.NormalizeDemographicTargeting(cfg.Gender, cfg.Languages, cfg.Schedule); err != nil {
		var de *reddit.DemographicError
		if errors.As(err, &de) {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig.%s: %s", domain.ErrCreateConfigInvalid, de.Field, de.Msg))
		}
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	return nil
}

// checkRedditDemographicShape refuses a gender that is not a string, a languages that is not an
// array of strings, and a schedule that is not an array of objects whose four fields are integers,
// so a wrong type is the synchronous 400 rather than a job failure (encoding/json would otherwise
// fail the whole redditConfig decode, which PreflightCreate leaves to Dispatch). null counts as
// absent, as encoding/json reads it. Keys are matched case-insensitively, as encoding/json matches
// them, and visited in sorted order so the refusal is deterministic.
func checkRedditDemographicShape(config json.RawMessage) error {
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
	refuse := func(field, msg string) error {
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig.%s: %s", domain.ErrCreateConfigInvalid, field, msg))
	}
	for _, k := range keys {
		raw := platformCfg[k]
		if isJSONNull(raw) {
			continue
		}
		switch {
		case strings.EqualFold(k, reddit.FieldGender):
			var g string
			if json.Unmarshal(raw, &g) != nil {
				return refuse(reddit.FieldGender, "must be a string, FEMALE or MALE")
			}
		case strings.EqualFold(k, reddit.FieldLanguages):
			var list []string
			if json.Unmarshal(raw, &list) != nil {
				return refuse(reddit.FieldLanguages, "must be an array of strings")
			}
		case strings.EqualFold(k, reddit.FieldSchedule):
			var windows []reddit.ScheduleWindow
			if json.Unmarshal(raw, &windows) != nil {
				return refuse(reddit.FieldSchedule, "must be an array of {startDay, startHour, endDay, endHour} objects with integer values")
			}
		}
	}
	return nil
}
