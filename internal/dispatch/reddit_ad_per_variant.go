// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"fmt"
	"os"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// redditAdPerVariantEnabled reports whether this deployment opted in to one ad per variant
// (#549 gap 6). Read per call, like REDDIT_DAILY_BUDGET_ENABLED, so it flips without a rebuild.
// See constants.EnvRedditAdPerVariantEnabled for why it is off.
func redditAdPerVariantEnabled() bool {
	return os.Getenv(constants.EnvRedditAdPerVariantEnabled) == "true"
}

// redditAdPerVariantApplies reports whether a create with this config takes the
// one-ad-per-variant path: the flag is on, the post is authored (imageUrl or imageAssetId, no
// postUrl) and there are at least two variants. It mirrors reddit.AdPerVariantApplies on the
// config's side, where a stored asset stands in for the late ImageURLSource.
func redditAdPerVariantApplies(cfg redditConfig) bool {
	return redditAdPerVariantEnabled() && strings.TrimSpace(cfg.PostURL) == "" &&
		(strings.TrimSpace(cfg.ImageURL) != "" || strings.TrimSpace(cfg.ImageAssetID) != "") &&
		len(cfg.Variants) >= 2
}

// checkRedditAdPerVariant applies the one-ad-per-variant rules, shared by PreflightCreate and
// Dispatch: at most reddit.MaxAdVariants variants, each with a non-blank headline of at most
// 300 characters (reddit.ValidateAdVariants). It refuses nothing while the flag is off, or on a
// create that does not author its post or has fewer than two variants: those are the one-ad
// create, unchanged. A refusal is an ErrCreateConfigInvalid (PreflightCreate answers 400 before
// any job exists) and a domain.PreflightRefusal (Dispatch copies the sentence into the job).
func checkRedditAdPerVariant(cfg redditConfig) error {
	if !redditAdPerVariantApplies(cfg) {
		return nil
	}
	if _, err := reddit.ValidateAdVariants(cfg.Variants); err != nil {
		// The client's sentence names the field and the 1-based variant and never echoes a headline.
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig.%s", domain.ErrCreateConfigInvalid, err.Error()))
	}
	return nil
}
