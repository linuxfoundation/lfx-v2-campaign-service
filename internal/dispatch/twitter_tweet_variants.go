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

// X tweet variants (#564 gap 10): twitterConfig.variants, [{"tweetText": "..."}], one authored and
// promoted tweet per variant under the create's one line item. The rule lives once, in
// twitter.NormalizeTweetVariants; this file adds the deployment gate and the JSON-shape check, and
// turns a refusal into the synchronous 400 shape ("invalid campaign config: twitterConfig.variants:
// ..."). A variant that fails after another was published leaves the row created_degraded with a
// step naming it; nothing is rolled back (twitter.promoteTweetVariants).

// twitterTweetVariant is one entry of twitterConfig.variants. Its only key is tweetText; the shape
// check refuses any other, so a per-variant field this service does not read (an image, a card)
// is a 400 rather than silently dropped.
type twitterTweetVariant struct {
	TweetText string `json:"tweetText"`
}

// twitterTweetVariantsEnabled reports whether this deployment opted in. Read per call.
func twitterTweetVariantsEnabled() bool {
	return os.Getenv(constants.EnvTwitterTweetVariantsEnabled) == "true"
}

// twitterVariantTexts is the variants' texts, in order.
func twitterVariantTexts(cfg twitterConfig) []string {
	if len(cfg.Variants) == 0 {
		return nil
	}
	out := make([]string, len(cfg.Variants))
	for i, v := range cfg.Variants {
		out[i] = v.TweetText
	}
	return out
}

// checkTwitterTweetVariants refuses a non-empty variants list while the gate is off, or one that
// breaks NormalizeTweetVariants, and otherwise returns the normalized texts (nil when unset). Every
// refusal is a domain.ErrCreateConfigInvalid wrapped as a PreflightRefusal.
func checkTwitterTweetVariants(cfg twitterConfig) ([]string, error) {
	if len(cfg.Variants) == 0 {
		return nil, nil
	}
	if !twitterTweetVariantsEnabled() {
		env := constants.EnvTwitterTweetVariantsEnabled
		f := twitter.FieldTweetVariants
		return nil, domain.WrapPreflightRefusal(fmt.Errorf(
			"%w: twitterConfig: %s disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, f, env, f, env))
	}
	texts, err := twitter.NormalizeTweetVariants(twitterVariantTexts(cfg), cfg.TweetID, cfg.TweetText)
	if err != nil {
		var tErr *twitter.TargetingError
		if errors.As(err, &tErr) {
			return nil, domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig.%s: %s", domain.ErrCreateConfigInvalid, tErr.Field, tErr.Msg))
		}
		return nil, domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	return texts, nil
}

// checkTwitterTweetVariantsShape refuses a variants value that is not a JSON array of objects whose
// only key is a string tweetText, so a wrong shape is the synchronous 400 rather than a job
// failure. null counts as absent; keys are matched case-insensitively, as encoding/json matches.
func checkTwitterTweetVariantsShape(config json.RawMessage) error {
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
	refuse := func(msg string) error {
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig.%s: %s", domain.ErrCreateConfigInvalid, twitter.FieldTweetVariants, msg))
	}
	for k, raw := range platformCfg {
		if !strings.EqualFold(k, twitter.FieldTweetVariants) || isJSONNull(raw) {
			continue
		}
		var entries []json.RawMessage
		if json.Unmarshal(raw, &entries) != nil {
			return refuse("must be an array of {\"tweetText\": string} objects")
		}
		for i, e := range entries {
			var obj map[string]json.RawMessage
			if json.Unmarshal(e, &obj) != nil || obj == nil {
				return refuse(fmt.Sprintf("entry %d must be a {\"tweetText\": string} object", i))
			}
			for ek, ev := range obj {
				if !strings.EqualFold(ek, "tweetText") {
					return refuse(fmt.Sprintf("entry %d has an unknown key; only tweetText is accepted", i))
				}
				var s string
				if json.Unmarshal(ev, &s) != nil {
					return refuse(fmt.Sprintf("entry %d tweetText must be a string", i))
				}
			}
		}
	}
	return nil
}
