// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/microsoft"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// Microsoft sitelink and callout extensions at create time (#563, gap 3):
// microsoftConfig.sitelinks and microsoftConfig.callouts, created as SitelinkAdExtension and
// CalloutAdExtension entries and associated with the confirmed campaign. The rules live once, in
// microsoft.ValidateTextExtensions; this file adds the deployment gate and the JSON-type check,
// turns a refusal into the synchronous 400 shape ("invalid campaign config:
// microsoftConfig.<field>: ..."), and attaches the extensions after the campaign exists.

// microsoftSitelinkConfig is one microsoftConfig.sitelinks entry. Same keys as
// googleAdsSitelinkConfig, so a caller mirroring a Google Search campaign reuses its list.
type microsoftSitelinkConfig struct {
	Text         string `json:"text"`
	Description1 string `json:"description1"`
	Description2 string `json:"description2"`
	FinalURL     string `json:"finalUrl"`
}

// microsoftAdExtensionsEnabled reports whether this deployment opted in. Read per call, like
// redditTargetingExclusionsEnabled, so it flips without a rebuild.
func microsoftAdExtensionsEnabled() bool {
	return os.Getenv(constants.EnvMicrosoftAdExtensionsEnabled) == "true"
}

func microsoftSitelinks(in []microsoftSitelinkConfig) []microsoft.Sitelink {
	if len(in) == 0 {
		return nil
	}
	out := make([]microsoft.Sitelink, len(in))
	for i, s := range in {
		out[i] = microsoft.Sitelink{Text: s.Text, Description1: s.Description1, Description2: s.Description2, FinalURL: s.FinalURL}
	}
	return out
}

// checkMicrosoftTextExtensions refuses a config that sets either list while the deployment gate
// is off, or whose lists break microsoft.ValidateTextExtensions. Every refusal is a
// domain.ErrCreateConfigInvalid (a 400 from PreflightCreate) wrapped as a PreflightRefusal (so a
// job that reached Dispatch copies the sentence verbatim). Any non-empty list counts as set, so
// the gate never depends on the values' own validity.
func checkMicrosoftTextExtensions(cfg microsoftConfig) error {
	var set []string
	if len(cfg.Sitelinks) > 0 {
		set = append(set, microsoft.FieldSitelinks)
	}
	if len(cfg.Callouts) > 0 {
		set = append(set, microsoft.FieldCallouts)
	}
	if len(set) == 0 {
		return nil
	}
	if !microsoftAdExtensionsEnabled() {
		env := constants.EnvMicrosoftAdExtensionsEnabled
		fields := strings.Join(set, " and ")
		return domain.WrapPreflightRefusal(fmt.Errorf(
			"%w: microsoftConfig: %s disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, fields, env, fields, env))
	}
	if _, _, err := microsoft.ValidateTextExtensions(microsoftSitelinks(cfg.Sitelinks), cfg.Callouts); err != nil {
		var exErr *microsoft.TextExtensionError
		if errors.As(err, &exErr) {
			return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s: %s", domain.ErrCreateConfigInvalid, exErr.Field, exErr.Msg))
		}
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	return nil
}

// checkMicrosoftTextExtensionsShape refuses a sitelinks value that is not an array of objects
// with string fields, or a callouts value that is not an array of strings, so a wrong type is the
// synchronous 400 rather than a job failure (encoding/json would otherwise fail the whole
// microsoftConfig decode, which PreflightCreate leaves to Dispatch). null counts as absent, as
// encoding/json reads it. Keys are matched case-insensitively, as encoding/json matches them, so
// a case variant cannot slip a wrong type past this check; they are visited in sorted order so
// the refusal is deterministic.
func checkMicrosoftTextExtensionsShape(config json.RawMessage) error {
	if len(config) == 0 {
		return nil
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(config, &envelope) != nil {
		return nil
	}
	var platformCfg map[string]json.RawMessage
	if json.Unmarshal(envelope["microsoftConfig"], &platformCfg) != nil {
		return nil
	}
	keys := make([]string, 0, len(platformCfg))
	for k := range platformCfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		raw := platformCfg[k]
		if isJSONNull(raw) {
			continue
		}
		switch {
		case strings.EqualFold(k, microsoft.FieldSitelinks):
			var list []microsoftSitelinkConfig
			if json.Unmarshal(raw, &list) != nil {
				return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s: must be an array of objects with string text, description1, description2 and finalUrl", domain.ErrCreateConfigInvalid, microsoft.FieldSitelinks))
			}
		case strings.EqualFold(k, microsoft.FieldCallouts):
			var list []string
			if json.Unmarshal(raw, &list) != nil {
				return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s: must be an array of strings", domain.ErrCreateConfigInvalid, microsoft.FieldCallouts))
			}
		}
	}
	return nil
}

// attachMicrosoftTextExtensions attaches the requested sitelinks and callouts to the CONFIRMED
// campaign in result, recording the ids and Steps on result. It reports whether the campaign is
// DEGRADED (an extension was requested and is not, or may not be, attached). Nothing requested
// is a no-op. Like attachMicrosoftImage it never returns an error: the campaign exists, so a
// dispatch error would retain the claim for a reconcile that never re-runs this step.
func (d *MicrosoftDispatcher) attachMicrosoftTextExtensions(ctx context.Context, client *microsoft.Client, result *microsoft.CampaignResult, cfg microsoftConfig) bool {
	if len(cfg.Sitelinks) == 0 && len(cfg.Callouts) == 0 {
		return false
	}
	tr, err := client.AttachCampaignTextExtensions(ctx, result.CampaignID, microsoftSitelinks(cfg.Sitelinks), cfg.Callouts)
	if tr != nil {
		result.SitelinkAdExtensionIDs = tr.SitelinkAdExtensionIDs
		result.CalloutAdExtensionIDs = tr.CalloutAdExtensionIDs
		result.Steps = append(result.Steps, tr.Steps...)
	}
	if err != nil {
		// Refused before any request — unreachable after the preflight and Dispatch checks, but
		// a requested extension that was never sent is still a shortfall.
		result.Steps = append(result.Steps, "Sitelink and callout extensions NOT attached: "+err.Error())
		slog.WarnContext(ctx, "microsoft text extensions not attached", "campaign_id", result.CampaignID, "error", err)
		return true
	}
	if tr.Degraded() {
		slog.WarnContext(ctx, "microsoft text extensions degraded", "campaign_id", result.CampaignID,
			"failed", tr.Failed, "unconfirmed", tr.Unconfirmed)
		return true
	}
	return false
}
