// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/reddit"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Reddit custom audiences (#549): redditConfig.customAudienceIds / excludedCustomAudienceIds on
// create, and the picker read behind list-reddit-ads-custom-audiences.
//
// Two gates, independent of each other. REDDIT_CUSTOM_AUDIENCES_ENABLED (here) keeps shapes no
// live Reddit call has exercised off the shared ad account until an operator turns it on. The
// global_marketing_ops check (service.RedditMarketingOpsInputs for the create inputs, the
// gateway's marketing-ops-api rule for the read) keeps a foundation's executive director, who is
// a campaign_manager, from using or listing audiences built on the LF ad account for another
// foundation.

// redditCustomAudienceFields are the two create inputs, as the JSON keys a caller spells them.
var redditCustomAudienceFields = []string{"customAudienceIds", "excludedCustomAudienceIds"}

// redditCustomAudiencesEnabled reports whether this deployment opted in. Read per call, like
// REDDIT_KEYWORD_TARGETING_WRITES_ENABLED, so it flips without a rebuild.
func redditCustomAudiencesEnabled() bool {
	return os.Getenv(constants.EnvRedditCustomAudiencesEnabled) == "true"
}

// redditCustomAudienceInvalid is the refusal for a bad custom audience input: a 400 from
// PreflightCreate (domain.ErrCreateConfigInvalid) and, from Dispatch, a refusal the job copies
// verbatim (domain.PreflightRefusal). The text is service-authored; it names fields and 1-based
// positions, never a caller value.
func redditCustomAudienceInvalid(format string, args ...any) error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: redditConfig.%s", domain.ErrCreateConfigInvalid, fmt.Sprintf(format, args...)))
}

// checkRedditCustomAudienceShape refuses a custom audience list of the wrong JSON type before
// any job exists. encoding/json matches keys case-insensitively, so every case variant of either
// key is checked, as the decoder would read it. An explicit null is an absent list, as it decodes.
func checkRedditCustomAudienceShape(config json.RawMessage) error {
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
	for _, k := range keys {
		for _, field := range redditCustomAudienceFields {
			if !strings.EqualFold(k, field) {
				continue
			}
			var ids []string
			if json.Unmarshal(platformCfg[k], &ids) != nil {
				return redditCustomAudienceInvalid("%s must be an array of strings", field)
			}
		}
	}
	return nil
}

// checkRedditCustomAudiences applies the deployment gate and the id rules
// (reddit.ValidateCustomAudienceIDs) to a decoded config. Shared by PreflightCreate (a 400
// before any job) and Dispatch (the same rules again, before any Reddit call, for a path that
// skipped the pre-check).
func checkRedditCustomAudiences(cfg redditConfig) error {
	if len(cfg.CustomAudienceIDs) == 0 && len(cfg.ExcludedCustomAudienceIDs) == 0 {
		return nil
	}
	if !redditCustomAudiencesEnabled() {
		// Name the list the request actually set (the first, when both are), so a client mapping
		// redditConfig.<field> onto a form control highlights the right input.
		field := "customAudienceIds"
		if len(cfg.CustomAudienceIDs) == 0 {
			field = "excludedCustomAudienceIds"
		}
		return redditCustomAudienceInvalid(
			"%s: Reddit custom audiences are disabled on this deployment (%s is not \"true\"); "+
				"omit customAudienceIds and excludedCustomAudienceIds, or ask an operator to set %s to \"true\"",
			field, constants.EnvRedditCustomAudiencesEnabled, constants.EnvRedditCustomAudiencesEnabled)
	}
	if msg := reddit.ValidateCustomAudienceIDs(cfg.CustomAudienceIDs, cfg.ExcludedCustomAudienceIDs); msg != "" {
		return redditCustomAudienceInvalid("%s", msg)
	}
	return nil
}

// ListRedditCustomAudiences lists the custom audiences on the ad account a Reddit create for the
// project uses, for the customAudienceIds picker (#549). Credentials resolve as for a create
// (d.creds.resolve: the project's own connection, or the LF system account when the deployment
// forces it), so every id listed is one a create for this project can send.
//
// called reports whether Reddit was contacted: false for the deployment gate and for a local
// refusal before the request (no connection, an unusable one).
func (d *RedditDispatcher) ListRedditCustomAudiences(ctx context.Context, projectID string, platform model.Provider) (_ *model.RedditCustomAudiences, called bool, _ error) {
	if !redditCustomAudiencesEnabled() {
		return nil, false, fmt.Errorf("reddit custom audiences are disabled (%s is not \"true\") until the shape is exercised against a live ad account: %w",
			constants.EnvRedditCustomAudiencesEnabled, domain.ErrTargetingLookupUnsupported)
	}
	client, _, err := d.resolveRedditClientWithCreds(ctx, projectID, platform, d.creds.resolve)
	if err != nil {
		return nil, false, err
	}
	rows, truncated, lerr := client.ListCustomAudiences(ctx)
	if lerr != nil {
		return nil, true, lerr
	}
	// make, not var: an account with no custom audience is a real, empty answer.
	out := make([]model.RedditCustomAudience, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.RedditCustomAudience{
			ID: r.ID, Name: r.Name, Type: r.Type, Status: r.Status, SizeLower: r.SizeLower, SizeUpper: r.SizeUpper,
		})
	}
	return &model.RedditCustomAudiences{AccountID: client.AccountID(), Audiences: out, Truncated: truncated}, true, nil
}
