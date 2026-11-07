// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/twitter"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// X custom audiences (#564 gap 3): twitterConfig.customAudienceIds / excludedCustomAudienceIds on
// create (CUSTOM_AUDIENCE criteria, EQ and NE, on the new line item), and the picker read behind
// list-twitter-ads-custom-audiences.
//
// Two gates, independent of each other, as for Reddit (reddit_custom_audiences.go).
// TWITTER_CUSTOM_AUDIENCES_ENABLED (here) keeps shapes no live X call has exercised off the
// shared ad account until an operator turns it on. The global_marketing_ops check
// (service.TwitterMarketingOpsInputs for the create inputs, the gateway's marketing-ops-api rule
// for the read) keeps a foundation's executive director, who is a campaign_manager, from using or
// listing audiences built on the LF ad account for another foundation.

// twitterCustomAudienceFields are the two create inputs, as the JSON keys a caller spells them.
var twitterCustomAudienceFields = []string{twitter.FieldCustomAudienceIDs, twitter.FieldExcludedCustomAudienceIDs}

// twitterCustomAudiencesEnabled reports whether this deployment opted in. Read per call, like
// twitterCreateTargetingEnabled, so it flips without a rebuild.
func twitterCustomAudiencesEnabled() bool {
	return os.Getenv(constants.EnvTwitterCustomAudiencesEnabled) == "true"
}

// twitterCustomAudienceInvalid is the refusal for a bad custom audience input: a 400 from
// PreflightCreate (domain.ErrCreateConfigInvalid) and, from Dispatch, a refusal the job copies
// verbatim (domain.PreflightRefusal). The text is service-authored; it names fields and 1-based
// positions, never a caller value.
func twitterCustomAudienceInvalid(format string, args ...any) error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig.%s", domain.ErrCreateConfigInvalid, fmt.Sprintf(format, args...)))
}

// checkTwitterCustomAudienceShape refuses a custom audience list of the wrong JSON type before
// any job exists. encoding/json matches keys case-insensitively, so every case variant of either
// key is checked, as the decoder would read it, in sorted order so the refusal is deterministic.
// An explicit null is an absent list, as it decodes.
func checkTwitterCustomAudienceShape(config json.RawMessage) error {
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
		for _, field := range twitterCustomAudienceFields {
			if !strings.EqualFold(k, field) || isJSONNull(platformCfg[k]) {
				continue
			}
			var ids []string
			if json.Unmarshal(platformCfg[k], &ids) != nil {
				return twitterCustomAudienceInvalid("%s: must be an array of strings", field)
			}
		}
	}
	return nil
}

// checkTwitterCustomAudiences applies the deployment gate and the id rules
// (twitter.ValidateCustomAudienceIDs) to a decoded config. Shared by PreflightCreate (a 400
// before any job) and Dispatch (the same rules again, before any X call, for a job enqueued
// before the flag was turned off). Any non-empty list counts as set, so the gate never depends
// on the values' own validity.
func checkTwitterCustomAudiences(cfg twitterConfig) error {
	if len(cfg.CustomAudienceIDs) == 0 && len(cfg.ExcludedCustomAudienceIDs) == 0 {
		return nil
	}
	if !twitterCustomAudiencesEnabled() {
		// Name the list the request actually set (the first, when both are), so a client mapping
		// twitterConfig.<field> onto a form control highlights the right input.
		field := twitter.FieldCustomAudienceIDs
		if len(cfg.CustomAudienceIDs) == 0 {
			field = twitter.FieldExcludedCustomAudienceIDs
		}
		env := constants.EnvTwitterCustomAudiencesEnabled
		return twitterCustomAudienceInvalid(
			"%s: X custom audiences are disabled on this deployment (%s is not \"true\"); "+
				"omit %s and %s, or ask an operator to set %s to \"true\"",
			field, env, twitter.FieldCustomAudienceIDs, twitter.FieldExcludedCustomAudienceIDs, env)
	}
	if err := twitter.ValidateCustomAudienceIDs(cfg.CustomAudienceIDs, cfg.ExcludedCustomAudienceIDs); err != nil {
		var tErr *twitter.TargetingError
		if errors.As(err, &tErr) {
			return twitterCustomAudienceInvalid("%s: %s", tErr.Field, tErr.Msg)
		}
		return twitterCustomAudienceInvalid("%s: %s", twitter.FieldCustomAudienceIDs, "invalid")
	}
	return nil
}

// checkTwitterCreateCriteriaCount refuses a create whose targeting lists name more criteria in
// total than one create can post inside its dispatch budget (twitter.CheckCreateCriteriaCount).
// geo and langs are the normalized lists checkTwitterCreateTargeting returned; the custom
// audience lists are counted as sent, since checkTwitterCustomAudiences refuses a repeat. Runs
// after both list checks, so a list's own refusal wins.
func checkTwitterCreateCriteriaCount(geo, langs []string, cfg twitterConfig) error {
	n := len(geo) + len(langs) + len(cfg.CustomAudienceIDs) + len(cfg.ExcludedCustomAudienceIDs)
	if err := twitter.CheckCreateCriteriaCount(n); err != nil {
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: twitterConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	return nil
}

// ListTwitterCustomAudiences lists the custom audiences on the ad account an X create for the
// project uses, for the customAudienceIds picker (#564 gap 3). Credentials resolve as for a
// create (d.creds.resolve: the project's own connection, or the LF system account when the
// deployment forces it), so every id listed is one a create for this project can send. The
// funding instrument a create also needs is not required for a read.
//
// called reports whether X was contacted: false for the deployment gate and for a local refusal
// before the request (no connection, an unusable one).
func (d *TwitterDispatcher) ListTwitterCustomAudiences(ctx context.Context, projectID string, platform model.Provider) (_ *model.TwitterCustomAudiences, called bool, _ error) {
	if !twitterCustomAudiencesEnabled() {
		return nil, false, fmt.Errorf("x custom audiences are disabled (%s is not \"true\") until the shape is exercised against a live ad account: %w",
			constants.EnvTwitterCustomAudiencesEnabled, domain.ErrTargetingLookupUnsupported)
	}
	res, err := d.creds.resolve(ctx, projectID, platform)
	if err != nil {
		return nil, false, err
	}
	creds, accountID, err := validateTwitterConnection(projectID, res)
	if err != nil {
		return nil, false, err
	}
	client := d.cachedTwitterClient(projectID, platform, res, creds, accountID,
		strings.TrimSpace(res.providerConfig["funding_instrument_id"]))
	rows, truncated, lerr := client.ListCustomAudiences(ctx)
	if lerr != nil {
		return nil, true, lerr
	}
	// make, not var: an account with no custom audience is a real, empty answer.
	out := make([]model.TwitterCustomAudience, 0, len(rows))
	for _, r := range rows {
		out = append(out, model.TwitterCustomAudience{
			ID: r.ID, Name: r.Name, AudienceType: r.AudienceType, Targetable: r.Targetable,
			AudienceSize: r.AudienceSize, ReasonsNotTargetable: r.ReasonsNotTargetable,
		})
	}
	return &model.TwitterCustomAudiences{AccountID: client.AccountID(), Audiences: out, Truncated: truncated}, true, nil
}
