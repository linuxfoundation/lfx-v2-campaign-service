// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"

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
	// PNG is a few hundred KB on the wire and 3.3 GiB decoded. The budget is expressed in
	// DECODED BYTES and priced from the declared colour model, so a 16-bit PNG — which Go
	// decodes at 8 bytes per pixel, twice the usual rate — is charged what it really costs. The
	// check uses only header values, so a bomb is rejected having cost one header read.
	if !dimensionsWithinLimits(cfg.Width, cfg.Height, cfg.ColorModel) {
		return nil, &briefs.BadRequestError{Code: "400", Message: "image dimensions exceed the maximum accepted for a creative asset"}
	}

	// Stage 3 — decode in full. Stage 1 proved only that a HEADER parses: image.DecodeConfig
	// stops once it has the format and dimensions, so a PNG truncated immediately after its
	// IHDR chunk passes it while being unrecoverable image data. Storing that yields a corrupt
	// asset that fails much later at dispatch, far from the upload that caused it, so the bytes
	// are proven decodable HERE. This is the allocating step, which is exactly why stage 2 runs
	// first: the buffer it may allocate is now bounded by the DECODED-BYTE budget, not by what
	// the header claims. Byte budget rather than pixel count deliberately — the bound that
	// matters is bytes allocated, and a 16-bit image costs 8 bytes per pixel where an 8-bit one
	// costs 4, so a pixel-only cap admits twice the memory it appears to for 16-bit uploads.
	// The decoded image itself is discarded — only the verdict matters.
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

// Decode budget for an uploaded creative. The bound that matters is BYTES ALLOCATED during
// image.Decode, not pixels: pixels are only a proxy, and the conversion factor is a property of
// the decoded REPRESENTATION, which varies by bit depth.
//
// maxCreativeDecodedBytes is 80 MiB of decoded pixel buffer. That figure is deliberately the
// ceiling the previous pixel-only bound INTENDED — its comment promised ~76 MiB — rather than
// the ~153 MiB it actually permitted once 16-bit images are priced correctly. Setting the budget
// to that larger real number would have blessed the defect instead of fixing it: nothing
// previously admitted would newly be refused, and the gate would have been rewritten to no
// effect.
//
// It is generous against real creatives: 80 MiB admits ~21M pixels at 8-bit and ~10M at 16-bit,
// against a 4K UHD upload (3840x2160 = 8.29M pixels) which is the largest anyone plausibly sends
// — Meta's own recommended feed maximum is 1936x1936 = 3.75M. A 4K image is accepted at BOTH bit
// depths.
//
// bytesPerPixel is what makes the budget honest across bit depths. Go's image/png decodes a
// 16-bit colour-type-6 PNG to *image.NRGBA64 at EIGHT bytes per pixel, not four — so a
// pixel-only cap silently permits twice the memory it appears to. An earlier revision of this
// code capped 20M pixels and its comment claimed ~76 MiB; the true worst case was ~160 MiB. The
// budget is now expressed in bytes and the per-pixel cost is read from the declared colour
// model, so a 16-bit image is charged what it actually costs.
//
// maxCreativeDimension additionally bounds each SIDE, because a byte budget alone admits a
// degenerate 1x20,000,000 strip that is no image any creative pipeline should accept.
const (
	maxCreativeDecodedBytes = 80 << 20 // 80 MiB of decoded pixel data
	maxCreativeDimension    = 10_000
	// narrowBytesPerPixel is the cost of the widest 8-bit-per-channel representation Go
	// produces (RGBA/NRGBA). Gray and YCbCr are cheaper; charging all of them the RGBA rate
	// keeps the estimate conservative without needing a case per format.
	narrowBytesPerPixel = 4
	// wideBytesPerPixel is the cost of a 16-bit-per-channel representation (*image.NRGBA64,
	// *image.RGBA64). This is the case the pixel-only bound missed.
	wideBytesPerPixel = 8
)

// bytesPerPixelFor reports the per-pixel cost of the representation image.Decode will produce
// for this colour model. It reads DecodeConfig's ColorModel, which is available BEFORE any
// allocation — that is the whole point, since the estimate has to be made before the decode it
// is protecting against.
//
// It is deliberately fail-safe: any colour model not recognised as narrow is charged the WIDE
// rate. A new or unusual model is exactly the case where guessing cheap would be wrong, and the
// cost of over-charging a legitimate image is a refusal at a size no real creative reaches.
func bytesPerPixelFor(m color.Model) int64 {
	switch m {
	case color.RGBAModel, color.NRGBAModel, color.AlphaModel, color.GrayModel,
		color.YCbCrModel, color.CMYKModel, color.NYCbCrAModel:
		return narrowBytesPerPixel
	default:
		// color.RGBA64Model, color.NRGBA64Model, color.Gray16Model, color.Alpha16Model,
		// and anything added later.
		return wideBytesPerPixel
	}
}

// dimensionsWithinLimits reports whether a header-declared image is safe to decode: each side is
// bounded, and the pixel buffer its colour model implies fits the byte budget.
//
// Non-positive values are refused rather than trusted: a decoder reporting them has not given a
// usable size, and letting them through would make the arithmetic below meaningless.
func dimensionsWithinLimits(width, height int, m color.Model) bool {
	if width <= 0 || height <= 0 {
		return false
	}
	if width > maxCreativeDimension || height > maxCreativeDimension {
		return false
	}
	// Safe from overflow: both sides are bounded by maxCreativeDimension above, so the product
	// cannot exceed 1e8 and the byte figure cannot exceed 8e8 — both well inside int64.
	return int64(width)*int64(height)*bytesPerPixelFor(m) <= maxCreativeDecodedBytes
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
