// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/platform/linkedin"
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
)

// LinkedIn document ads (#665): linkedInConfig.format "document" with documentAssetId (a stored
// PDF of the brief), documentTitle and documentIntroText creates a document ad whose one creative
// is a dark post carrying the PDF, uploaded through LinkedIn's Documents API. Decision 2026-11-25
// (Misha): ENGAGEMENT by default; objective "lead_generation" with a Lead Gen Form makes it a gated
// document; "traffic" and "conversions" are refused. See linkedin/documents.go for the LinkedIn
// contract and its sources.

// linkedinDocumentEnabled reports whether this deployment opted in to linkedInConfig.format
// "document" (#665). Read per call so it flips without a rebuild. Off by default: the Documents
// API upload and the document post have not been exercised against a live account.
func linkedinDocumentEnabled() bool {
	return os.Getenv(constants.EnvLinkedInAdsDocumentEnabled) == "true"
}

// isDocumentKey reports whether a linkedInConfig key is one of the document's fields, matched
// case-insensitively as encoding/json does.
func isDocumentKey(k string) bool {
	return strings.EqualFold(k, "documentAssetId") || strings.EqualFold(k, "documentTitle") || strings.EqualFold(k, "documentIntroText")
}

// checkDocumentFieldShape refuses a document field that is not a JSON string; null is refused
// rather than read as omitted.
func checkDocumentFieldShape(raw json.RawMessage) error {
	var str string
	if isJSONNull(raw) || json.Unmarshal(raw, &str) != nil {
		return domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig.documentAssetId, documentTitle and documentIntroText must be strings", domain.ErrCreateConfigInvalid))
	}
	return nil
}

// checkDocumentShape runs checkDocumentFieldShape on the document's fields of a raw create config.
// Dispatch runs it before its strict decode, as it does checkLeadGenShape, so a wrong-typed field
// fails with this sentence rather than the decoder's.
func checkDocumentShape(config json.RawMessage) error {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(config, &envelope) != nil {
		return nil
	}
	var platformCfg map[string]json.RawMessage
	if json.Unmarshal(envelope["linkedInConfig"], &platformCfg) != nil {
		return nil
	}
	for k, raw := range platformCfg {
		if isDocumentKey(k) {
			if err := checkDocumentFieldShape(raw); err != nil {
				return err
			}
		}
	}
	return nil
}

// linkedinDocumentFormat is linkedinFormat's document arm: it returns linkedin.FormatDocument, or
// an ErrCreateConfigInvalid pre-flight refusal for
//
//   - format "document" while LINKEDIN_ADS_DOCUMENT_ENABLED is off (checked first);
//   - variants, textAds or carouselCards beside it: the document IS the one creative;
//   - objective "traffic" or "conversions" (LinkedIn offers document ads neither), or a manual
//     bid (both objectives a document takes are automatic);
//   - a documentAssetId that is not a uuid, and what linkedin.ValidateDocumentFields refuses (no
//     asset, a title over 70 characters, intro text over 600, either blank).
//
// Whether the asset exists for this brief and is a PDF LinkedIn accepts is decided at dispatch by
// resolveLinkedInDocument, still before any LinkedIn request. The Lead Gen Form of a gated
// document is checked by linkedinLeadGen like any lead generation create's.
func linkedinDocumentFormat(cfg linkedinConfig, plan bidPlan) (string, error) {
	if !linkedinDocumentEnabled() {
		return "", domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig.format %q is disabled on this deployment (%s is not \"true\"); omit it, or ask an operator to set %s to \"true\"",
			domain.ErrCreateConfigInvalid, linkedin.FormatDocument, constants.EnvLinkedInAdsDocumentEnabled, constants.EnvLinkedInAdsDocumentEnabled))
	}
	refuse := func(err error) (string, error) {
		return "", domain.WrapPreflightRefusal(fmt.Errorf("%w: linkedInConfig: %w", domain.ErrCreateConfigInvalid, err))
	}
	if len(cfg.Variants) > 0 || cfg.TextAds != nil || cfg.CarouselCards != nil || cfg.CarouselIntroText != "" {
		return refuse(fmt.Errorf("format %q takes its creative from documentAssetId; omit variants, textAds and carouselCards", linkedin.FormatDocument))
	}
	switch cfg.Objective {
	case "", linkedinObjectiveLeadGeneration:
	default:
		return refuse(fmt.Errorf("format %q is an engagement ad (objective omitted) or a gated lead generation ad (objective \"lead_generation\"); LinkedIn offers document ads no objective %q", linkedin.FormatDocument, cfg.Objective))
	}
	if plan.manual() {
		return refuse(fmt.Errorf("format %q is created with automatic bidding only; omit the manual bid", linkedin.FormatDocument))
	}
	if raw := strings.TrimSpace(cfg.DocumentAssetID); raw != "" {
		if _, err := uuid.Parse(raw); err != nil {
			return refuse(fmt.Errorf("documentAssetId %s is not a valid asset id", domain.CallerValue(raw)))
		}
	}
	if err := linkedin.ValidateDocumentFields(cfg.DocumentAssetID, cfg.DocumentTitle, cfg.DocumentIntroText); err != nil {
		return refuse(err)
	}
	return linkedin.FormatDocument, nil
}

// resolveLinkedInDocument loads and validates the stored PDF a document create names, returning
// nil when it names none. On success cfg.DocumentAssetID is the canonical uuid (so the snapshot
// never stores a caller spelling), and the returned release func owns the AssetReserver
// reservation for the bytes, held until the dispatch returns; on any failure the reservation is
// released here and the func is a no-op. The asset is looked up SCOPED to the brief, so another
// brief's or project's asset is "does not exist for this brief".
func (d *LinkedInDispatcher) resolveLinkedInDocument(ctx context.Context, brief *model.CampaignBrief, cfg *linkedinConfig) (*linkedin.DocumentUpload, func(), error) {
	noop := func() {}
	raw := strings.TrimSpace(cfg.DocumentAssetID)
	if raw == "" {
		return nil, noop, nil
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return nil, noop, domain.PreflightRefusalf("linkedInConfig.documentAssetId %s is not a valid asset id", domain.CallerValue(raw))
	}
	assetID := parsed.String()
	cfg.DocumentAssetID = assetID
	if d.creatives == nil {
		return nil, noop, domain.PreflightRefusalf("linkedInConfig.documentAssetId references creative asset %s but the creative-asset store is not configured", assetID)
	}
	// PRICE BEFORE LOADING, as resolveLinkedInImages does.
	size, err := d.creatives.GetAssetSize(ctx, brief.ProjectID, brief.ID, assetID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, noop, domain.PreflightRefusalf("linkedInConfig.documentAssetId references creative asset %s, which does not exist for this brief", assetID)
		}
		return nil, noop, fmt.Errorf("size creative asset %s: %w", assetID, err)
	}
	if size > linkedin.MaxDocumentBytes {
		return nil, noop, domain.PreflightRefusalf("creative asset %s is %d bytes; LinkedIn document ads accept at most %d bytes", assetID, size, linkedin.MaxDocumentBytes)
	}
	release, ok := d.assets.reserve(ctx, size)
	if !ok {
		return nil, noop, domain.PreflightRefusalf("creative asset %s (%d bytes) exceeds the memory concurrently available for dispatch; retry when other dispatches finish", assetID, size)
	}
	succeeded := false
	defer func() {
		if !succeeded {
			release()
		}
	}()
	asset, err := d.creatives.GetAsset(ctx, brief.ProjectID, brief.ID, assetID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, noop, domain.PreflightRefusalf("linkedInConfig.documentAssetId references creative asset %s, which does not exist for this brief", assetID)
		}
		return nil, noop, fmt.Errorf("load creative asset %s: %w", assetID, err)
	}
	if asset == nil || len(asset.Bytes) == 0 {
		return nil, noop, domain.PreflightRefusalf("creative asset %s has no stored bytes", assetID)
	}
	if asset.MimeType != model.MimeTypePDF {
		return nil, noop, domain.PreflightRefusalf("linkedInConfig.documentAssetId names creative asset %s, which is %s; a LinkedIn document ad takes a PDF (upload it with content_type application/pdf)", assetID, model.AssetKindPhrase(asset.MimeType))
	}
	doc := linkedin.DocumentUpload{Data: asset.Bytes, MimeType: asset.MimeType}
	if err := linkedin.ValidateDocument(doc); err != nil {
		return nil, noop, domain.WrapPreflightRefusal(fmt.Errorf("creative asset %s: %w", assetID, err))
	}
	succeeded = true
	return &doc, release, nil
}
