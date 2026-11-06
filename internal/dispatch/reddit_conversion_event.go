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
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// redditConversionEventKey is redditConfig's conversion-event field (#549).
const redditConversionEventKey = "conversionEvent"

// redditConversionEventEnabled reports whether this deployment opted in to
// redditConfig.conversionEvent. Read per call, like REDDIT_KEYWORD_TARGETING_WRITES_ENABLED, so it
// flips without a rebuild. See constants.EnvRedditConversionEventEnabled for why it is off.
func redditConversionEventEnabled() bool {
	return os.Getenv(constants.EnvRedditConversionEventEnabled) == "true"
}

// redditConversionEventInvalid is the refusal for a conversionEvent the service will not send: an
// ErrCreateConfigInvalid (PreflightCreate answers 400 before any job exists) and a
// domain.PreflightRefusal (a Dispatch that reaches it copies the sentence into the job verbatim).
func redditConversionEventInvalid(format string, args ...any) error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig.%s: %s", domain.ErrCreateConfigInvalid, redditConversionEventKey, fmt.Sprintf(format, args...)))
}

// checkRedditConversionEvent applies every conversionEvent rule, shared by PreflightCreate and
// Dispatch. It reads the RAW redditConfig for presence and type, because the decoded struct
// cannot tell an absent field from `null` or `""` and encoding/json matches keys
// case-insensitively: only the exact key is accepted, and present it must be a non-blank string.
// A present field then needs the deployment gate, and its value must suit the objective
// (reddit.ResolveConversionEvent). An absent field is never refused, whatever the gate.
//
// cfg is the decoded config, used only for the objective; a decode that failed elsewhere leaves
// cfg.Objective as decoded so far, which is what Dispatch would read too.
func checkRedditConversionEvent(config json.RawMessage, cfg redditConfig) error {
	raw, present, err := redditConfigField(config, redditConversionEventKey)
	if err != nil || !present {
		return err
	}
	var event string
	if isJSONNull(raw) || json.Unmarshal(raw, &event) != nil {
		return redditConversionEventInvalid("must be a string naming a pixel event, one of %s; omit it for the default, PURCHASE", strings.Join(reddit.ConversionEvents(), ", "))
	}
	if strings.TrimSpace(event) == "" {
		return redditConversionEventInvalid("an empty value is refused; omit the field for the default, PURCHASE")
	}
	if !redditConversionEventEnabled() {
		return redditConversionEventInvalid("disabled on this deployment (%s is not \"true\"); omit conversionEvent, or ask an operator to set %s to \"true\"",
			constants.EnvRedditConversionEventEnabled, constants.EnvRedditConversionEventEnabled)
	}
	if _, err := reddit.ResolveConversionEvent(cfg.Objective, event); err != nil {
		return redditConversionEventInvalid("%s", err.Error())
	}
	return nil
}

// redditConfigField returns the raw value of one redditConfig key and whether it is present. A
// case variant of the key is refused: encoding/json would decode it into the same field, so
// accepting it here would let the raw check and the decode disagree (the rule checkBiddingShape
// applies to `bidding`). An envelope or redditConfig that is not a JSON object reports absent:
// Dispatch fails that config on its own decode.
func redditConfigField(config json.RawMessage, key string) (json.RawMessage, bool, error) {
	if len(config) == 0 {
		return nil, false, nil
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(config, &envelope) != nil {
		return nil, false, nil
	}
	var platformCfg map[string]json.RawMessage
	if json.Unmarshal(envelope["redditConfig"], &platformCfg) != nil {
		return nil, false, nil
	}
	keys := make([]string, 0, len(platformCfg))
	for k := range platformCfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k != key && strings.EqualFold(k, key) {
			return nil, false, domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig: %s is not a config field; use %q (exact spelling)",
				domain.ErrCreateConfigInvalid, quoteKey(k), key))
		}
	}
	raw, ok := platformCfg[key]
	return raw, ok, nil
}
