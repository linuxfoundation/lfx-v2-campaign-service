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
var twitterTargetingFields = []string{twitter.FieldGeoTargets, twitter.FieldLanguages, twitter.FieldKeywords, twitter.FieldInterestIDs, twitter.FieldConversationTopics, twitter.FieldPlatforms, twitter.FieldDevices}

// twitterTargetingStringFields are the gated single-value twitterConfig keys (#564 gap 9).
var twitterTargetingStringFields = []string{twitter.FieldAgeBucket, twitter.FieldGender}

// twitterCreateTargeting is the normalized create-time targeting behind
// TWITTER_CREATE_TARGETING_ENABLED: countries and languages (#564 gaps 1-2), keywords and
// interest ids (gap 4), conversation topics (gap 8), and device platforms, devices, an age bucket
// and a gender (gap 9).
type twitterCreateTargeting struct {
	geo, langs, keywords, interests, topics, platforms, devices []string
	// ageBucket and gender are normalized ("" = unset); each set one posts one criterion.
	ageBucket, gender string
	// lookalikes are the follower look-alike ids (#564 gap 4c). They sit behind their own flag
	// (checkTwitterFollowerLookalikes), not this file's, and are set by the caller; carried here so
	// the per-create criteria count covers them.
	lookalikes []string
}

// count is how many criteria these lists post (the custom audiences are counted separately).
func (t twitterCreateTargeting) count() int {
	n := len(t.geo) + len(t.langs) + len(t.keywords) + len(t.interests) + len(t.topics) + len(t.platforms) + len(t.devices) + len(t.lookalikes)
	if t.ageBucket != "" {
		n++
	}
	if t.gender != "" {
		n++
	}
	return n
}

// checkTwitterCreateTargeting refuses a config that sets any of the lists while the deployment
// gate is off, or whose lists break NormalizeCreateTargeting / NormalizeKeywordInterestTargeting /
// NormalizeConversationTopics / NormalizePlatforms / NormalizeDevices / NormalizeAgeBucket /
// NormalizeGender,
// and otherwise returns the normalized lists. Every refusal is a domain.ErrCreateConfigInvalid (a
// 400 from PreflightCreate) wrapped as a PreflightRefusal (so a job that reached Dispatch copies
// the sentence verbatim). Any non-empty list counts as set, so the gate never depends on the
// values' own validity; a blank ageBucket or gender counts as unset.
func checkTwitterCreateTargeting(cfg twitterConfig) (twitterCreateTargeting, error) {
	var set []string
	for _, f := range []struct {
		name string
		n    int
	}{
		{twitter.FieldGeoTargets, len(cfg.GeoTargets)},
		{twitter.FieldLanguages, len(cfg.Languages)},
		{twitter.FieldKeywords, len(cfg.Keywords)},
		{twitter.FieldInterestIDs, len(cfg.InterestIDs)},
		{twitter.FieldConversationTopics, len(cfg.ConversationTopics)},
		{twitter.FieldPlatforms, len(cfg.Platforms)},
		{twitter.FieldDevices, len(cfg.Devices)},
		{twitter.FieldAgeBucket, len(strings.TrimSpace(cfg.AgeBucket))},
		{twitter.FieldGender, len(strings.TrimSpace(cfg.Gender))},
	} {
		if f.n > 0 {
			set = append(set, f.name)
		}
	}
	if len(set) == 0 {
		return twitterCreateTargeting{}, nil
	}
	if !twitterCreateTargetingEnabled() {
		env := constants.EnvTwitterCreateTargetingEnabled
		fields := strings.Join(set, " and ")
		return twitterCreateTargeting{}, domain.WrapPreflightRefusal(fmt.Errorf(
			"%w: twitterConfig: %s disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, fields, env, fields, env))
	}
	var t twitterCreateTargeting
	var err error
	t.geo, t.langs, err = twitter.NormalizeCreateTargeting(cfg.GeoTargets, cfg.Languages)
	if err == nil {
		t.keywords, t.interests, err = twitter.NormalizeKeywordInterestTargeting(cfg.Keywords, cfg.InterestIDs)
	}
	if err == nil {
		t.topics, err = twitter.NormalizeConversationTopics(cfg.ConversationTopics)
	}
	if err == nil {
		t.platforms, err = twitter.NormalizePlatforms(cfg.Platforms)
	}
	if err == nil {
		t.devices, err = twitter.NormalizeDevices(cfg.Devices)
	}
	if err == nil {
		t.ageBucket, err = twitter.NormalizeAgeBucket(cfg.AgeBucket)
	}
	if err == nil {
		t.gender, err = twitter.NormalizeGender(cfg.Gender)
	}
	if err != nil {
		var tErr *twitter.TargetingError
		if errors.As(err, &tErr) {
			return twitterCreateTargeting{}, domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig.%s: %s", domain.ErrCreateConfigInvalid, tErr.Field, tErr.Msg))
		}
		return twitterCreateTargeting{}, domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	return t, nil
}

// checkTwitterTargetingShape refuses a targeting list that is not a JSON array of strings (or an
// ageBucket / gender that is not a JSON string), so a
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
		for _, field := range twitterTargetingStringFields {
			if !strings.EqualFold(k, field) || isJSONNull(raw) {
				continue
			}
			var s string
			if json.Unmarshal(raw, &s) != nil {
				return domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig.%s: must be a string", domain.ErrCreateConfigInvalid, field))
			}
		}
	}
	return nil
}
