// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"fmt"
	"os"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Boosting an existing organization post (#673): linkedInConfig.format "boost" with
// linkedInConfig.boostPost creates the usual campaign group and SPONSORED_UPDATES campaign, whose
// one creative sponsors that post instead of a dark post per variant. The client reads the post
// before anything is created (see linkedin/boost.go for the checks and their sources).

// linkedinBoostPostEnabled reports whether this deployment opted in to linkedInConfig.format
// "boost" (#673). Read per call so it flips without a rebuild. Off by default: the post read and
// the reference creative have not been exercised against a live account.
func linkedinBoostPostEnabled() bool {
	return os.Getenv(constants.EnvLinkedInAdsBoostPostEnabled) == "true"
}

// boostPostShapeRefusal is the one sentence a wrong-typed boostPost gets.
func boostPostShapeRefusal() error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig.boostPost must be a string, the post URN urn:li:share:<id> or urn:li:ugcPost:<id>", domain.ErrCreateConfigInvalid))
}

// linkedinBoostFormat checks a format "boost" create before any job or request and returns
// linkedin.FormatBoost: refused while LINKEDIN_ADS_BOOST_POST_ENABLED is not exactly "true", with
// variants (the post is the ad), and without a boostPost that is exactly a post URN. Bidding,
// objective, conversions and targeting are the ordinary SPONSORED_UPDATES ones and are checked
// where they always are. Whether the post can be boosted is read by the client, in the job.
func linkedinBoostFormat(cfg linkedinConfig) (string, error) {
	if !linkedinBoostPostEnabled() {
		return "", domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig.format %q is disabled on this deployment (%s is not \"true\"); omit it, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, linkedin.FormatBoost, constants.EnvLinkedInAdsBoostPostEnabled, constants.EnvLinkedInAdsBoostPostEnabled))
	}
	refuse := func(err error) (string, error) {
		return "", domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	if len(cfg.Variants) > 0 {
		return refuse(fmt.Errorf("format %q sponsors boostPost, an existing post; omit variants", linkedin.FormatBoost))
	}
	if cfg.BoostPost == "" {
		return refuse(fmt.Errorf("format %q requires boostPost, the post URN urn:li:share:<id> or urn:li:ugcPost:<id>", linkedin.FormatBoost))
	}
	if err := linkedin.ValidateBoostPostURN(cfg.BoostPost); err != nil {
		return refuse(err)
	}
	return linkedin.FormatBoost, nil
}
