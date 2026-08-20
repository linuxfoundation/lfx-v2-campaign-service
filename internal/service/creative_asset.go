// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"

	// Register the PNG and JPEG decoders so image.DecodeConfig can recognise them. Blank
	// imports: their init() functions register the formats; nothing calls them directly. The
	// set imported here is the sniff allow-list's UPPER bound — a format whose decoder is not
	// registered can never be recognised — but mimeForImageFormat below is the authoritative
	// allow-list, so an unrelated package importing another decoder (image/gif, say) cannot
	// widen what this endpoint accepts.
	_ "image/jpeg"
	_ "image/png"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// SetCreativeAssetRepo injects the creative-asset repository.
//
// Separate from NewBriefService for the same reason as SetIndexer / SetLLMClient: the ~40
// existing constructor call sites (nearly all tests) must keep compiling, and a BriefService
// without it still serves every other method. UploadCreativeAsset reports 503 rather than
// nil-panicking when it is missing. It is part of briefBackendSetter so the cold-start path
// binds it in the same step it binds the brief repos — leaving it out there would serve every
// upload a 503 forever on a pod that cold-started, while everything else worked (the exact
// silent-gap the audience service's SetBriefRepo/SetBuilder were pulled onto that interface to
// prevent).
func (s *BriefService) SetCreativeAssetRepo(r domain.CreativeAssetRepository) {
	if r == nil {
		return
	}
	s.mu.Lock()
	s.creativeAssets = r
	s.mu.Unlock()
}

// CreativeAssetRepoIsSet reports whether the creative-asset repository was injected. Exported
// ONLY so the container's wiring tests can assert injection directly, for the same reason
// AudienceService.BuilderIsSet is: UploadCreativeAsset returns the same typed 503 on a
// no-database container as it does on a live container whose wiring forgot this repo, so an
// error-based assertion cannot tell a wired service from an unwired one. Without it, deleting
// either SetCreativeAssetRepo call in the container compiles and passes every test while every
// upload 503s forever in production.
func (s *BriefService) CreativeAssetRepoIsSet() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.creativeAssets != nil
}

// creativeAssetRepo snapshots the repository under the read lock, mirroring deps().
func (s *BriefService) creativeAssetRepo() domain.CreativeAssetRepository {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.creativeAssets
}

// UploadCreativeAsset validates and stores an uploaded image for a brief so a Meta ad creative
// can later reference it by id. It touches no ad platform: the bytes are held until dispatch,
// where Meta's per-ad-account image_hash is resolved.
//
// The generated decoder already enforces what the CONTRACT can express — content_type is one of
// the allowed MIME strings, and the decoded byte length is within [1, 30 MiB] — so a request
// reaching this handler has cleared those. What it cannot express, and what this handler adds,
// is that the BYTES are actually a decodable image of the declared type: a client may send a
// JPEG under a declared image/png, or arbitrary bytes under either. The stored mime_type is the
// SNIFFED one, and a declared/sniffed mismatch is refused rather than silently corrected.
func (s *BriefService) UploadCreativeAsset(ctx context.Context, p *briefs.UploadCreativeAssetPayload) (*briefs.CreativeAsset, error) {
	repo := s.creativeAssetRepo()
	if repo == nil {
		// Availability-neutral wording, matching ready(): in the cold-start window the database
		// IS configured but the repo has not bound yet, so "not configured" would misdirect an
		// operator during a transient startup.
		return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "creative asset upload is unavailable"}
	}
	// No validateProjectSlug here, unlike CreateCampaigns/AdoptCampaign. Those stamp project_id
	// into the campaign-name attribution key and use it as the connection-lookup key at
	// dispatch, so a UUID would break the join. An asset's project_id is only a tenant-scoping
	// predicate — never an attribution or lookup key — so it stays UUID-or-slug like the other
	// nested brief routes (GetBrief, CreateAudience).

	// Validation runs in three stages, and the ORDER is the security property: the cheap header
	// read first, then the declared-size gate, and only then the allocating full decode. Each
	// stage bounds what the next one may spend.
	//
	// Stage 1 — image.DecodeConfig reads only the header. It costs nothing, and it yields both
	// the format and the DECLARED dimensions, which is what makes stage 2 possible before any
	// pixel buffer exists. Meta's creative POLICY (minimum dimensions, aspect ratio) is not
	// checked anywhere here: it is Meta-specific and belongs at dispatch, where Meta's API is the
	// authority — this endpoint is storage integrity, not platform policy.
	cfg, format, derr := image.DecodeConfig(bytes.NewReader(p.Bytes))
	if derr != nil {
		return nil, &briefs.BadRequestError{Code: "400", Message: "the uploaded bytes are not a decodable PNG or JPEG image"}
	}
	sniffed, ok := mimeForImageFormat(format)
	if !ok {
		// A format image.DecodeConfig recognised (some registered decoder) but this endpoint does
		// not accept. Kept distinct from the decode failure above: the bytes ARE an image, just
		// not one Meta single-image creatives support.
		return nil, &briefs.BadRequestError{Code: "400", Message: "unsupported image format; only PNG and JPEG are accepted"}
	}
	if p.ContentType != sniffed {
		return nil, &briefs.BadRequestError{Code: "400", Message: "declared content_type does not match the uploaded image bytes"}
	}

	// Stage 2 — refuse an image whose DECLARED size is beyond anything this endpoint serves,
	// BEFORE stage 3 allocates a pixel buffer for it. This is the decompression-bomb gate: both
	// PNG (zlib) and JPEG compress uniformly flat images enormously, so a body well inside the
	// 42 MiB request cap can declare dimensions whose decoded form is gigabytes. A 30000x30000
	// PNG is a few hundred KB on the wire and 3.3 GiB decoded. The check uses only the header
	// values, so a bomb is rejected having cost one header read.
	if !dimensionsWithinLimits(cfg.Width, cfg.Height) {
		return nil, &briefs.BadRequestError{Code: "400", Message: "image dimensions exceed the maximum accepted for a creative asset"}
	}

	// Stage 3 — decode in full. Stage 1 proved only that a HEADER parses: image.DecodeConfig
	// stops once it has the format and dimensions, so a PNG truncated immediately after its
	// IHDR chunk passes it while being unrecoverable image data. Storing that yields a corrupt
	// asset that fails much later at dispatch, far from the upload that caused it, so the bytes
	// are proven decodable HERE. This is the allocating step, which is exactly why stage 2 runs
	// first: the buffer it may allocate is now bounded by the pixel cap, not by what the header
	// claims. The decoded image itself is discarded — only the verdict matters.
	if _, _, err := image.Decode(bytes.NewReader(p.Bytes)); err != nil {
		return nil, &briefs.BadRequestError{Code: "400", Message: "the uploaded image data is incomplete or corrupt"}
	}

	asset := &model.CreativeAsset{
		ProjectID: p.ProjectID,
		BriefID:   p.BriefID,
		// The VERIFIED type, not the declared one: they are equal past the check above, but this
		// is the value that must be stored and it is the sniff, by definition.
		MimeType: sniffed,
		// Derived here, never trusted from the client (the payload carries no size): the stored
		// size is exactly what was received.
		ByteSize: int64(len(p.Bytes)),
		// The dedupe/idempotency key. Content-derived, so a repeat upload of the same image
		// collides on (brief_id, checksum) and returns the existing asset.
		Checksum: sha256Hex(p.Bytes),
		Bytes:    p.Bytes,
		// Same nil handling as CreateBrief: a request with no decodable actor stores NULL rather
		// than refusing the write. On an idempotent re-upload the repo preserves the FIRST
		// uploader, so this attributes creation, not re-sending.
		CreatedBy: marshalActor(attributedActor(ctx, "upload creative asset")),
	}
	stored, err := repo.CreateAsset(ctx, asset)
	if err != nil {
		return nil, mapBriefErr(err)
	}
	return creativeAssetResult(stored), nil
}

// Dimension limits for an uploaded creative. These bound the pixel buffer image.Decode may
// allocate, and they are deliberately generous against real ad creatives rather than tuned to
// them — this is an anti-bomb ceiling, not a Meta policy check (that lives at dispatch).
//
// The largest creative anyone plausibly uploads is 4K UHD, 3840x2160 = 8.29M pixels; Meta's own
// recommended feed maximum is 1936x1936 = 3.75M. maxCreativePixels sits at 20M, roughly 2.4x
// beyond 4K, which caps a decoded RGBA buffer near 76 MiB. maxCreativeDimension additionally
// bounds each SIDE, because the pixel product alone admits a degenerate 1x20,000,000 strip that
// is within the area budget yet is no image any creative pipeline should accept.
const (
	maxCreativePixels    = 20_000_000
	maxCreativeDimension = 10_000
)

// dimensionsWithinLimits reports whether header-declared dimensions are acceptable to decode.
// Non-positive values are refused rather than trusted: a decoder that reports them has not given
// a usable size, and letting them through would make the multiplication below meaningless.
func dimensionsWithinLimits(width, height int) bool {
	if width <= 0 || height <= 0 {
		return false
	}
	if width > maxCreativeDimension || height > maxCreativeDimension {
		return false
	}
	// Safe from overflow: both sides are already bounded by maxCreativeDimension above, so the
	// product cannot exceed 1e8 and fits an int on every platform this builds for.
	return width*height <= maxCreativePixels
}

// mimeForImageFormat maps an image.DecodeConfig format name to this endpoint's verified MIME
// type, reporting false for anything outside the PNG/JPEG allow-list. This — not the set of
// registered decoders — is what bounds what the upload accepts.
func mimeForImageFormat(format string) (string, bool) {
	switch format {
	case "png":
		return model.MimeTypePNG, true
	case "jpeg":
		return model.MimeTypeJPEG, true
	default:
		return "", false
	}
}

// sha256Hex returns the lowercase-hex SHA-256 of b, matching the format stored in
// creative_assets.checksum and used as the (brief_id, checksum) dedupe key.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// creativeAssetResult maps a stored asset to the Goa result type. The bytes are deliberately not
// echoed back: the response is metadata (the caller already has the bytes it sent), and a
// multi-megabyte base64 body on every upload would be pure overhead.
func creativeAssetResult(a *model.CreativeAsset) *briefs.CreativeAsset {
	return &briefs.CreativeAsset{
		ID:        a.ID,
		ProjectID: a.ProjectID,
		BriefID:   a.BriefID,
		MimeType:  a.MimeType,
		ByteSize:  a.ByteSize,
		Checksum:  a.Checksum,
	}
}
