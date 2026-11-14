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

// Microsoft structured snippet extensions at create time (#563, gap 13):
// microsoftConfig.structuredSnippets, created as StructuredSnippetAdExtension entries and
// associated with the confirmed campaign, the callouts' path (microsoft_text_extensions.go). The
// rules live once, in microsoft.ValidateStructuredSnippets; this file adds the deployment gate and
// the JSON-type check, turns a refusal into the synchronous 400 shape ("invalid campaign config:
// microsoftConfig.structuredSnippets: ..."), and attaches the snippets after the campaign exists.
//
// The logo extension the issue lists with it is deferred: it needs its own image limits and an
// upload path distinct from the campaign image extension, and none of it is verified.

// microsoftStructuredSnippetConfig is one microsoftConfig.structuredSnippets entry. Same keys as
// googleAdsStructuredSnippetConfig, so a caller mirroring a Google Search campaign reuses its list.
type microsoftStructuredSnippetConfig struct {
	Header string   `json:"header"`
	Values []string `json:"values"`
}

// microsoftStructuredSnippetsEnabled reports whether this deployment opted in. Read per call, like
// microsoftAdExtensionsEnabled, so it flips without a rebuild.
func microsoftStructuredSnippetsEnabled() bool {
	return os.Getenv(constants.EnvMicrosoftStructuredSnippetsEnabled) == "true"
}

func microsoftStructuredSnippets(in []microsoftStructuredSnippetConfig) []microsoft.StructuredSnippet {
	if len(in) == 0 {
		return nil
	}
	out := make([]microsoft.StructuredSnippet, len(in))
	for i, s := range in {
		out[i] = microsoft.StructuredSnippet{Header: s.Header, Values: s.Values}
	}
	return out
}

// microsoftSnippetInvalid is the refusal for a bad snippet input: a 400 from PreflightCreate
// (domain.ErrCreateConfigInvalid) and, from Dispatch, a refusal the job copies verbatim
// (domain.PreflightRefusal). Service-authored text naming the field and positions only.
func microsoftSnippetInvalid(format string, args ...any) error {
	return domain.WrapPreflightRefusal(fmt.Errorf("%w: microsoftConfig.%s", domain.ErrCreateConfigInvalid, fmt.Sprintf(format, args...)))
}

// checkMicrosoftStructuredSnippets refuses a config that sets the list while the deployment gate
// is off, or whose list breaks microsoft.ValidateStructuredSnippets. Shared by PreflightCreate (a
// 400 before any job) and Dispatch (the same rules again, before any Microsoft call). Any
// non-empty list counts as set, so the gate never depends on the values' own validity.
func checkMicrosoftStructuredSnippets(cfg microsoftConfig) error {
	if len(cfg.StructuredSnippets) == 0 {
		return nil
	}
	field := microsoft.FieldStructuredSnippets
	if !microsoftStructuredSnippetsEnabled() {
		env := constants.EnvMicrosoftStructuredSnippetsEnabled
		return microsoftSnippetInvalid("%s: Microsoft structured snippets are disabled on this deployment (%s is not \"true\"); omit %s, or ask an operator to set %s to \"true\"",
			field, env, field, env)
	}
	if _, err := microsoft.ValidateStructuredSnippets(microsoftStructuredSnippets(cfg.StructuredSnippets)); err != nil {
		var exErr *microsoft.TextExtensionError
		if errors.As(err, &exErr) {
			return microsoftSnippetInvalid("%s: %s", exErr.Field, exErr.Msg)
		}
		return microsoftSnippetInvalid("%s: invalid", field)
	}
	return nil
}

// checkMicrosoftStructuredSnippetsShape refuses a structuredSnippets value that is not an array of
// {header: string, values: string[]} objects, so a wrong type is the synchronous 400 rather than a
// job failure. null counts as absent. Keys are matched case-insensitively, as encoding/json
// matches them, and visited in sorted order so the refusal is deterministic.
func checkMicrosoftStructuredSnippetsShape(config json.RawMessage) error {
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
		if !strings.EqualFold(k, microsoft.FieldStructuredSnippets) || isJSONNull(platformCfg[k]) {
			continue
		}
		var list []microsoftStructuredSnippetConfig
		if json.Unmarshal(platformCfg[k], &list) != nil {
			return microsoftSnippetInvalid("%s: must be an array of objects with a string header and an array of string values", microsoft.FieldStructuredSnippets)
		}
	}
	return nil
}

// attachMicrosoftStructuredSnippets attaches the requested snippets to the CONFIRMED campaign in
// result, recording the ids and Steps on result. It reports whether the campaign is DEGRADED (a
// snippet was requested and is not, or may not be, attached). Nothing requested is a no-op. Like
// attachMicrosoftTextExtensions it never returns an error: the campaign exists, so a dispatch
// error would retain the claim for a reconcile that never re-runs this step.
func attachMicrosoftStructuredSnippets(ctx context.Context, client *microsoft.Client, result *microsoft.CampaignResult, cfg microsoftConfig) bool {
	if len(cfg.StructuredSnippets) == 0 {
		return false
	}
	tr, err := client.AttachCampaignStructuredSnippets(ctx, result.CampaignID, microsoftStructuredSnippets(cfg.StructuredSnippets))
	if tr != nil {
		result.StructuredSnippetAdExtensionIDs = tr.StructuredSnippetAdExtensionIDs
		result.Steps = append(result.Steps, tr.Steps...)
	}
	if err != nil {
		// Refused before any request: unreachable after the preflight and Dispatch checks, but a
		// requested snippet that was never sent is still a shortfall.
		result.Steps = append(result.Steps, "Structured snippet extensions NOT attached: "+err.Error())
		slog.WarnContext(ctx, "microsoft structured snippets not attached", "campaign_id", result.CampaignID, "error", err)
		return true
	}
	if tr.Degraded() {
		slog.WarnContext(ctx, "microsoft structured snippets degraded", "campaign_id", result.CampaignID,
			"failed", tr.Failed, "unconfirmed", tr.Unconfirmed)
		return true
	}
	return false
}
