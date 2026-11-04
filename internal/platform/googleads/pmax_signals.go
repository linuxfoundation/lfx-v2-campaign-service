// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package googleads

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Performance Max asset group signals (#461)
// ---------------------------------------------------------------------------
//
// A signal tells Performance Max where to START looking: a SEARCH THEME is free text
// naming searches the campaign is for, and an AUDIENCE signal names an existing Audience
// whose people Google treats as likely converters. Neither is targeting — Google may serve
// beyond both.
//
// Each signal is an AssetGroupSignal hanging off an ASSET GROUP, so this client creates
// them only after createPerformanceMaxAssetGroup has confirmed one, in a single
// googleAds:mutate. The shapes this file sends were verified by validateOnly probes on
// 2026-11-03 (v23): `{assetGroup, searchTheme: {text}}` is accepted and a theme over 80
// characters is refused (stringLengthError TOO_LONG); several themes in one
// googleAds:mutate are accepted; `{assetGroup, audience: {audience:
// customers/<id>/audiences/<id>}}` is accepted, but only ONE audience signal per asset
// group (ONE_AUDIENCE_ALLOWED_PER_ASSET_GROUP); and themes plus one audience in the same
// request are accepted.

const (
	// maxSearchThemeRunes is the theme length the probe found Google refusing past
	// (stringLengthError TOO_LONG over 80). Counted in runes, as the extension text in
	// assets.go is.
	maxSearchThemeRunes = 80

	// maxSearchThemes is the per-asset-group search-theme ceiling: 50, verified by a
	// validateOnly probe on 2026-11-03 (a 51st theme on the group was refused with
	// resourceCountLimitExceededError RESOURCE_LIMIT). Google's help page still says 25,
	// which is stale. Enforced here because the signals mutate runs after the campaign
	// and its asset group exist, and a refusal there is a partial result where the same
	// refusal here costs one corrected request.
	maxSearchThemes = 50
)

// An audience signal takes two spellings, as validateConversionActions takes two: the full
// resource name a GAQL read returns, matched by demandGenAudienceRE (the Demand Gen
// Audience's own pattern, demandgen_targeting.go), and the bare id a human reads off the
// Google Ads UI. Either way the audience id must be a positive canonical base-10 number
// (canonicalCampaignID), the rule #459 set for DemandGenAudience: "0", "007" or an id past
// int64 names no Audience, and Google would refuse it only after the asset group exists.

// performanceMaxSignalPlan is the validated signal set: trimmed themes in input order and
// the audience as a full resource name in THIS account, or empty.
type performanceMaxSignalPlan struct {
	searchThemes []string
	audience     string
}

func (p performanceMaxSignalPlan) empty() bool {
	return len(p.searchThemes) == 0 && p.audience == ""
}

func (p performanceMaxSignalPlan) count() int {
	n := len(p.searchThemes)
	if p.audience != "" {
		n++
	}
	return n
}

// validatePerformanceMaxSignals resolves the signal inputs WITHOUT sending anything,
// inside preflightCampaignKind, so the adoption path refuses exactly what the create path
// refuses. `assetGroup` is whether the validated creative will create an asset group.
//
// Duplicate themes are REFUSED, compared case-insensitively, rather than de-duplicated:
// the same rule callouts and sitelinks take, and for their reason — an operator who
// pasted twenty themes and finds nineteen in the Google Ads UI deserves to have been
// told which one collapsed, and the refusal names it before anything is paid for.
//
// The audience check is ACCOUNT-scoped, not foundation-scoped: on the shared LF Google Ads
// customer, a project can name another foundation's Audience as its signal. That is an
// accepted risk for now — on Performance Max an audience is a signal, not a restriction —
// and lfx-self-serve offers Google audience inputs only to root principals; a follow-up
// issue tracks enforcement in this service.
func validatePerformanceMaxSignals(kind, customerID string, in CampaignInput, assetGroup bool) (performanceMaxSignalPlan, error) {
	if len(in.PerformanceMaxSearchThemes) == 0 && in.PerformanceMaxAudienceSignal == "" {
		return performanceMaxSignalPlan{}, nil
	}
	if kind != campaignKindPerformanceMax {
		return performanceMaxSignalPlan{}, fmt.Errorf("google-ads: search themes and audience signals are not supported on %s (they are Performance Max asset group signals, and only Performance Max has asset groups); omit PerformanceMaxSearchThemes and PerformanceMaxAudienceSignal, or create a Performance Max campaign", kind)
	}
	if !assetGroup {
		return performanceMaxSignalPlan{}, fmt.Errorf("google-ads: Performance Max search themes and audience signals attach to an asset group, and this request creates none (no PerformanceMaxCreative); supply PerformanceMaxCreative, or omit PerformanceMaxSearchThemes and PerformanceMaxAudienceSignal")
	}
	if n := len(in.PerformanceMaxSearchThemes); n > maxSearchThemes {
		return performanceMaxSignalPlan{}, fmt.Errorf("google-ads: a Performance Max asset group accepts at most %d search themes, got %d", maxSearchThemes, n)
	}

	var plan performanceMaxSignalPlan
	seen := make(map[string]struct{}, len(in.PerformanceMaxSearchThemes))
	for i, raw := range in.PerformanceMaxSearchThemes {
		theme := strings.TrimSpace(raw)
		if theme == "" {
			return performanceMaxSignalPlan{}, fmt.Errorf("google-ads: Performance Max search theme %d is empty", i)
		}
		if n := utf8.RuneCountInString(theme); n > maxSearchThemeRunes {
			return performanceMaxSignalPlan{}, fmt.Errorf("google-ads: Performance Max search theme %d is %d characters, exceeding the %d limit", i, n, maxSearchThemeRunes)
		}
		key := strings.ToLower(theme)
		if _, dup := seen[key]; dup {
			return performanceMaxSignalPlan{}, fmt.Errorf("google-ads: Performance Max search theme %q is listed more than once", capForError(theme))
		}
		seen[key] = struct{}{}
		plan.searchThemes = append(plan.searchThemes, theme)
	}

	if in.PerformanceMaxAudienceSignal != "" {
		audience, err := resolvePerformanceMaxAudience(customerID, in.PerformanceMaxAudienceSignal)
		if err != nil {
			return performanceMaxSignalPlan{}, err
		}
		plan.audience = audience
	}
	return plan, nil
}

// resolvePerformanceMaxAudience turns a non-empty audience signal into a full Audience
// resource name in THIS account. A value that is only whitespace is refused rather than
// read as absent, as validateDemandGenAudienceField refuses a blank DemandGenAudience: the
// caller set the field, and dropping it would create a campaign without the signal they
// named.
func resolvePerformanceMaxAudience(customerID, raw string) (string, error) {
	audience := strings.TrimSpace(raw)
	if audience == "" {
		return "", fmt.Errorf("google-ads: PerformanceMaxAudienceSignal must not be blank; " +
			"omit it, or name an existing Audience by its numeric id or its " +
			"customers/<id>/audiences/<id> resource name")
	}
	if canonicalCampaignID(audience) != "" {
		return "customers/" + customerID + "/audiences/" + audience, nil
	}
	m := demandGenAudienceRE.FindStringSubmatch(audience)
	if m == nil || canonicalCampaignID(m[2]) == "" {
		return "", fmt.Errorf("google-ads: audience signal %q is neither a positive canonical "+
			"audience id nor a customers/<id>/audiences/<id> resource name with one",
			capForError(audience))
	}
	// Another account's audience is refused rather than re-pointed: an audience belongs to
	// the account that built it, and the same id here names a different audience or none,
	// which Google would refuse after the asset group exists.
	if m[1] != customerID {
		return "", fmt.Errorf("google-ads: audience signal %q belongs to customer %s, not the "+
			"campaign's account %s; an audience cannot be shared across accounts",
			capForError(audience), m[1], customerID)
	}
	return audience, nil
}

// searchThemeInfo is SearchThemeInfo: only `text` exists on the input side.
type searchThemeInfo struct {
	Text string `json:"text"`
}

// assetGroupSignalCreate is AssetGroupSignal. Its audience arm reuses audienceInfo
// (demandgen_targeting.go), the same AudienceInfo payload the Demand Gen ad group sends.
// Its signal is a oneof, so exactly one of the two pointers is set — by construction in
// createPerformanceMaxSignals, which builds each operation from one theme or the one
// audience.
type assetGroupSignalCreate struct {
	AssetGroup  string           `json:"assetGroup"`
	SearchTheme *searchThemeInfo `json:"searchTheme,omitempty"`
	Audience    *audienceInfo    `json:"audience,omitempty"`
}

// createPerformanceMaxSignals creates every signal on the confirmed asset group in ONE
// googleAds:mutate, themes first in input order and then the audience, and returns the
// criterion id of each.
//
// partialFailure is left off, so the request is ATOMIC: a definite refusal means no
// signal exists, and an ambiguous outcome means all of them may. Either way the asset
// group and its links are already in place, which is why the errors say so — the
// campaign can serve without its signals, and the operator only needs to add them.
//
// A 2xx is confirmed result by result: each must be an assetGroupSignals resource in THIS
// account whose composite `<groupId>~<criterionId>` names the asset group these
// operations named. Anything else is UNCONFIRMED, and no id is recorded from it.
func (c *Client) createPerformanceMaxSignals(ctx context.Context, groupResource, groupID string, plan performanceMaxSignalPlan) ([]string, error) {
	if plan.empty() {
		return nil, nil
	}
	// Unreachable today (pmax.go calls this only with a confirmed group); refuses rather
	// than sends a signal naming no group.
	if groupResource == "" || groupID == "" {
		return nil, fmt.Errorf("google-ads Performance Max asset group signals not sent: no confirmed asset group to attach %d signal(s) to", plan.count())
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("google-ads Performance Max asset group signals aborted before their request (context done; asset group %s has no signals yet): %w", groupID, ctxErr)
	}

	ops := make([]googleAdsMutateOperation, 0, plan.count())
	for _, theme := range plan.searchThemes {
		ops = append(ops, googleAdsMutateOperation{AssetGroupSignalOperation: &mutateOperation{Create: assetGroupSignalCreate{
			AssetGroup:  groupResource,
			SearchTheme: &searchThemeInfo{Text: theme},
		}}})
	}
	if plan.audience != "" {
		ops = append(ops, googleAdsMutateOperation{AssetGroupSignalOperation: &mutateOperation{Create: assetGroupSignalCreate{
			AssetGroup: groupResource,
			Audience:   &audienceInfo{Audience: plan.audience},
		}}})
	}

	resp, err := c.doRequest(ctx, http.MethodPost, c.customerPath("googleAds:mutate"), googleAdsMutateRequest{MutateOperations: ops}, false)
	if err != nil {
		if createOutcomeAmbiguous(err) {
			return nil, fmt.Errorf("google-ads Performance Max asset group signals UNCONFIRMED (asset group %s created with its assets; its %d signal(s) may exist — verify in Google Ads before retrying): %w", groupID, len(ops), err)
		}
		return nil, fmt.Errorf("google-ads Performance Max asset group signals failed (asset group %s created with its assets; the %d signal(s) are created in one atomic request, so none of them exists): %w", groupID, len(ops), err)
	}

	var results googleAdsMutateResponse
	if uErr := json.Unmarshal(resp, &results); uErr != nil || len(results.MutateOperationResponses) != len(ops) {
		return nil, fmt.Errorf("google-ads Performance Max asset group signals UNCONFIRMED (asset group %s created; 2xx with a malformed or mis-sized response for %d signal(s) — they may exist — verify in Google Ads before retrying)", groupID, len(ops))
	}
	ids := make([]string, 0, len(ops))
	for i, r := range results.MutateOperationResponses {
		var name string
		if r.AssetGroupSignalResult != nil {
			name = r.AssetGroupSignalResult.ResourceName
		}
		id := c.assetGroupSignalCriterionID(name, groupID)
		if id == "" {
			return nil, fmt.Errorf("google-ads Performance Max asset group signals UNCONFIRMED (asset group %s created; result %d names %q, not a signal on that asset group in this account — verify in Google Ads before retrying)", groupID, i, capForError(name))
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// assetGroupSignalCriterionID returns the criterion id of an AssetGroupSignal resource
// name — `customers/<cid>/assetGroupSignals/<groupId>~<criterionId>` — when it is in THIS
// account and on groupID, and "" for anything else.
func (c *Client) assetGroupSignalCriterionID(name, groupID string) string {
	if groupID == "" || c.validateResourceKind("assetGroupSignals", name, false) != nil {
		return ""
	}
	parts := strings.Split(resourceID(name), "~")
	if len(parts) != 2 || parts[0] != groupID || !numericID(parts[1]) {
		return ""
	}
	return parts[1]
}

// performanceMaxSignalStep summarises the signals for the operator-facing step list,
// naming only what was asked for.
func performanceMaxSignalStep(plan performanceMaxSignalPlan) string {
	parts := make([]string, 0, 2)
	if n := len(plan.searchThemes); n > 0 {
		parts = append(parts, fmt.Sprintf("%d search theme(s)", n))
	}
	if plan.audience != "" {
		parts = append(parts, "1 audience signal")
	}
	return strings.Join(parts, ", ")
}
