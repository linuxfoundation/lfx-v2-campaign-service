// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// LinkedIn Lead Gen Form campaigns (#664, #552 gap 6): linkedInConfig.objective "lead_generation"
// with linkedInConfig.leadGenForm (a form built in Campaign Manager) and the optional
// linkedInConfig.leadGenCallToAction, behind LINKEDIN_ADS_LEAD_GEN_ENABLED. The rules live once,
// in linkedin.ValidateLeadGen; this file adds the deployment gate and the JSON-type check, and
// turns a refusal into the synchronous 400 shape ("invalid campaign config: linkedInConfig...").

// linkedinObjectiveLeadGeneration is the linkedInConfig.objective spelling of LEAD_GENERATION,
// metaConfig's word for the same goal.
const linkedinObjectiveLeadGeneration = "lead_generation"

// linkedinLeadGenEnabled reports whether this deployment opted in. Read per call, like the other
// LinkedIn gates, so it flips without a rebuild.
func linkedinLeadGenEnabled() bool {
	return os.Getenv(constants.EnvLinkedInAdsLeadGenEnabled) == "true"
}

// linkedinLeadGenDisabled is the gate's refusal, naming what the create set.
func linkedinLeadGenDisabled(fields string) error {
	env := constants.EnvLinkedInAdsLeadGenEnabled
	return domain.WrapPreflightRefusal(fmt.Errorf(
		"%w: %s disabled on this deployment (%s is not \"true\"); omit it, or ask an operator to set %s to \"true\"",
		domain.ErrCreateConfigInvalid, fields, env, env))
}

// linkedinLeadGen resolves a create's lead gen inputs into the client's LeadGen (nil for a create
// that is not lead gen). Every refusal is a domain.ErrCreateConfigInvalid pre-flight refusal: a
// 400 from PreflightCreate, and the same sentence failing a job that reached Dispatch.
//
//   - leadGenForm or leadGenCallToAction without objective "lead_generation": refused.
//   - any of the three while LINKEDIN_ADS_LEAD_GEN_ENABLED is off: refused (linkedinObjective
//     refuses the objective itself first).
//   - otherwise linkedin.ValidateLeadGen: the form id's shape, the label, and the format, Audience
//     Network and variant call-to-action rules. The format is normalised as the client will; an
//     unknown one is left to linkedinFormat's refusal.
func linkedinLeadGen(cfg linkedinConfig, delivery linkedin.DeliveryOptions) (*linkedin.LeadGen, error) {
	leadGen := cfg.Objective == linkedinObjectiveLeadGeneration
	form, cta := strings.TrimSpace(cfg.LeadGenForm) != "", cfg.LeadGenCallToAction != ""
	if !leadGen && !form && !cta {
		return nil, nil
	}
	if !linkedinLeadGenEnabled() {
		var set []string
		if form {
			set = append(set, "linkedInConfig."+linkedin.FieldLeadGenForm)
		}
		if cta {
			set = append(set, "linkedInConfig."+linkedin.FieldLeadGenCallToAction)
		}
		if leadGen {
			set = append(set, "linkedInConfig.objective \"lead_generation\"")
		}
		return nil, linkedinLeadGenDisabled(strings.Join(set, " and "))
	}
	if !leadGen {
		return nil, domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig.%s and %s are used only with linkedInConfig.objective \"lead_generation\"",
			domain.ErrCreateConfigInvalid, linkedin.FieldLeadGenForm, linkedin.FieldLeadGenCallToAction))
	}
	var in *linkedin.LeadGen
	if form {
		in = &linkedin.LeadGen{FormID: cfg.LeadGenForm, CallToAction: cfg.LeadGenCallToAction}
	}
	format, err := linkedin.NormalizeFormat(cfg.Format)
	if err != nil {
		format = cfg.Format
	}
	lg, err := linkedin.ValidateLeadGen(in, linkedin.ObjectiveLeadGeneration, format, delivery, cfg.Variants)
	if err != nil {
		return nil, domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	return lg, nil
}

// checkLeadGenShape refuses a leadGenForm or leadGenCallToAction that is not a JSON string, so a
// wrong type (a form id sent as a number) is the synchronous 400 rather than a value the
// best-effort pre-flight decode skips. null counts as absent. Keys are matched
// case-insensitively, as encoding/json matches them, and visited in sorted order so the refusal
// is deterministic.
func checkLeadGenShape(config json.RawMessage) error {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(config, &envelope) != nil {
		return nil
	}
	var platformCfg map[string]json.RawMessage
	if json.Unmarshal(envelope["linkedInConfig"], &platformCfg) != nil {
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
		for _, field := range []string{linkedin.FieldLeadGenForm, linkedin.FieldLeadGenCallToAction} {
			var s string
			if strings.EqualFold(k, field) && json.Unmarshal(raw, &s) != nil {
				return domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig.%s must be a string", domain.ErrCreateConfigInvalid, field))
			}
		}
	}
	return nil
}
