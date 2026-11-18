// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Reddit sub-country geolocation targeting (#549 gap 11): redditConfig.subCountryGeoTargets,
// Reddit region, city and DMA geolocation ids sent in the ad group's targeting.geolocations. The
// rules live once, in reddit.NormalizeSubCountryGeoTargets, and the Reddit read in
// reddit.ValidateSubCountryGeoTargets; this file adds the deployment gate and the JSON-type check,
// and runs the read like the community and interest lookups (reddit_targeting_resolution.go):
// fail-open in PreflightCreate, fail-closed in Dispatch before anything is created.

// redditSubCountryGeoEnabled reports whether this deployment opted in. Read per call, like
// redditTargetingExclusionsEnabled, so it flips without a rebuild.
func redditSubCountryGeoEnabled() bool {
	return os.Getenv(constants.EnvRedditSubCountryGeoEnabled) == "true"
}

// checkRedditSubCountryGeoShape refuses a subCountryGeoTargets that is not a JSON array of
// strings, so a wrong type is the synchronous 400 rather than a job failure. null reads as absent.
func checkRedditSubCountryGeoShape(config json.RawMessage) error {
	return checkRedditStringListsShape(config, []string{reddit.FieldSubCountryGeoTargets})
}

// checkRedditSubCountryGeo refuses a config that sets subCountryGeoTargets while the deployment
// gate is off, or whose list breaks NormalizeSubCountryGeoTargets (a malformed id, an id inside a
// whole-country geoTargets or excludedGeoTargets entry, more than the cap). Every refusal is a
// domain.ErrCreateConfigInvalid wrapped as a PreflightRefusal. Any non-empty list counts as set,
// so the gate never depends on the values' own validity.
func checkRedditSubCountryGeo(cfg redditConfig) error {
	if len(cfg.SubCountryGeoTargets) == 0 {
		return nil
	}
	if !redditSubCountryGeoEnabled() {
		env := constants.EnvRedditSubCountryGeoEnabled
		field := reddit.FieldSubCountryGeoTargets
		return domain.WrapPreflightRefusal(fmt.Errorf(
			"%w: redditConfig: %s disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, field, env, field, env))
	}
	if _, err := reddit.NormalizeSubCountryGeoTargets(cfg.GeoTargets, cfg.ExcludedGeoTargets, cfg.SubCountryGeoTargets); err != nil {
		var exErr *reddit.ExclusionError
		if errors.As(err, &exErr) {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig.%s: %s", domain.ErrCreateConfigInvalid, exErr.Field, exErr.Msg))
		}
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	return nil
}

// needsRedditSubCountryGeoValidation reports whether this create has sub-country ids to confirm
// and the deployment opted in.
func needsRedditSubCountryGeoValidation(cfg redditConfig) bool {
	return redditSubCountryGeoEnabled() && len(cfg.SubCountryGeoTargets) > 0
}

// validateRedditSubCountryGeo confirms cfg's sub-country ids through client. A rule breach or an
// id Reddit refuses is a domain.PreflightRefusal; a read that cannot be completed or believed is a
// plain error.
func validateRedditSubCountryGeo(ctx context.Context, client *reddit.Client, cfg redditConfig) error {
	entries, err := reddit.NormalizeSubCountryGeoTargets(cfg.GeoTargets, cfg.ExcludedGeoTargets, cfg.SubCountryGeoTargets)
	if err != nil {
		// Unreachable after checkRedditSubCountryGeo; kept so the read never sends an unchecked id.
		return domain.PreflightRefusalf("invalid redditConfig.%s", err.Error())
	}
	return client.ValidateSubCountryGeoTargets(ctx, entries)
}
