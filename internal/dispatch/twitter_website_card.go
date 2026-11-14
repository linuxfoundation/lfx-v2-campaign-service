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

// X website card (#564 gap 6): twitterConfig.card, {"headline": "..."}, a website card (the
// creative's media, the headline, the UTM'd registration URL) attached by card_uri to the authored
// tweet(s). The rules live in twitter.NormalizeWebsiteCardHeadline and CreateCampaign; this file
// adds the deployment gate, the JSON-shape check and the config-only rules (it needs media and an
// authored tweet), each a synchronous 400 ("invalid campaign config: twitterConfig.card: ...").

// twitterWebsiteCard is twitterConfig.card. Its only key is headline; the shape check refuses any
// other, so a per-card field this service does not send is a 400 rather than silently dropped.
type twitterWebsiteCard struct {
	Headline string `json:"headline"`
}

// twitterWebsiteCardEnabled reports whether this deployment opted in. Read per call.
func twitterWebsiteCardEnabled() bool {
	return os.Getenv(constants.EnvTwitterWebsiteCardEnabled) == "true"
}

// checkTwitterWebsiteCard refuses a card while the gate is off, one whose headline breaks
// NormalizeWebsiteCardHeadline, one with no media (imageAssetId) and one on a create that authors no
// tweet (tweetId set, or neither tweetText nor variants), and otherwise returns the card input
// (nil when unset). Every refusal is a domain.ErrCreateConfigInvalid wrapped as a PreflightRefusal.
func checkTwitterWebsiteCard(cfg twitterConfig) (*twitter.WebsiteCardInput, error) {
	if cfg.Card == nil {
		return nil, nil
	}
	f := twitter.FieldWebsiteCard
	refuse := func(msg string) error {
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig.%s: %s", domain.ErrCreateConfigInvalid, f, msg))
	}
	if !twitterWebsiteCardEnabled() {
		env := constants.EnvTwitterWebsiteCardEnabled
		return nil, domain.WrapPreflightRefusal(fmt.Errorf(
			"%w: twitterConfig: %s disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, f, env, f, env))
	}
	h, err := twitter.NormalizeWebsiteCardHeadline(cfg.Card.Headline)
	if err != nil {
		var tErr *twitter.TargetingError
		if errors.As(err, &tErr) {
			return nil, refuse(tErr.Msg)
		}
		return nil, refuse(err.Error())
	}
	if strings.TrimSpace(cfg.TweetID) != "" || (strings.TrimSpace(cfg.TweetText) == "" && len(cfg.Variants) == 0) {
		return nil, refuse("a website card rides on an authored tweet, so it requires tweetText (or variants) and no tweetId")
	}
	if strings.TrimSpace(cfg.ImageAssetID) == "" {
		return nil, refuse("a website card needs media, so it requires imageAssetId")
	}
	return &twitter.WebsiteCardInput{Headline: h}, nil
}

// checkTwitterWebsiteCardShape refuses a card that is not a JSON object whose only key is a string
// headline, so a wrong shape is the synchronous 400 rather than a job failure. null counts as
// absent; keys are matched case-insensitively, as encoding/json matches them.
func checkTwitterWebsiteCardShape(config json.RawMessage) error {
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
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig.%s: %s", domain.ErrCreateConfigInvalid, twitter.FieldWebsiteCard, msg))
	}
	for k, raw := range platformCfg {
		if !strings.EqualFold(k, twitter.FieldWebsiteCard) || isJSONNull(raw) {
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil || obj == nil {
			return refuse("must be a {\"headline\": string} object")
		}
		for ok, ov := range obj {
			if !strings.EqualFold(ok, "headline") {
				return refuse("has an unknown key; only headline is accepted")
			}
			var s string
			if json.Unmarshal(ov, &s) != nil {
				return refuse("headline must be a string")
			}
		}
	}
	return nil
}
