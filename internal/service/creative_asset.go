// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
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
	"github.com/linuxfoundation/lfx-v2-campaign-service/pkg/constants"
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
// the allowed MIME strings, and the byte length is non-empty and within the design's
// MaxLength(41943040). That ceiling is stated in base64 CHARACTERS (the unit the wire schema
// measures), so as a bound on the decoded slice the generated validator applies it to, it admits
// up to ~40 MiB; the real 30 MiB stored-file ceiling is Stage 0 below, at maxCreativeStoredBytes.
// So a request reaching this handler has cleared those. What it cannot express, and what this handler adds,
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

	// Stage 0 — DECODE the base64 payload, then apply the STORED-FILE ceiling to the result.
	//
	// The wire attribute is a base64 STRING (design/brief.go), not a Goa Bytes attribute, so the
	// decode happens here rather than in the generated decoder. That change made the published
	// contract honest — Goa emits a Bytes attribute as `format: binary`, meaning raw octets,
	// which is not what an application/json body carries — and it moves exactly one
	// responsibility to this layer: turning the encoded string into the bytes everything below
	// already expected.
	//
	// MALFORMED BASE64 IS A 400, never a panic and never a 500. It is caller input, and the only
	// thing a caller can do about it is send it correctly.
	//
	// StdEncoding (padded, standard alphabet) is the same encoding the previous Goa Bytes
	// attribute used, so the accepted wire format is unchanged by the type switch — a client
	// that worked before works now.
	decoded, b64err := base64.StdEncoding.DecodeString(p.Bytes)
	if b64err != nil {
		// The error text is deliberately fixed: a base64 decoder's message can quote the
		// offending input, and this body is an image an operator uploaded.
		return nil, &briefs.BadRequestError{Code: "400", Message: "the uploaded bytes are not valid base64"}
	}

	// The STORED-FILE ceiling, applied to the DECODED length.
	//
	// The unit matters and it changed with the attribute type. The design's MaxLength(41943040)
	// bounds the ENCODED string in characters — that is what the generated validator now
	// enforces, and it is the right unit for a string. This is the DECODED bound, 30 MiB, and it
	// is a different quantity: 41,943,040 base64 characters decode to exactly 31,457,280 bytes,
	// so the two agree by construction rather than by coincidence.
	//
	// Checking len(p.Bytes) here instead would silently bound ENCODED characters and admit only
	// ~22.5 MiB of image, which is the same unit confusion that moved MaxLength off the decoded
	// slice in the first place.
	//
	// It is not the only enforcement, and must not be described as such: byte_size is
	// caller-supplied on the INSERT, so migration 000029 carries the same 30 MiB bound as a
	// table CHECK for writers that never pass through this handler. This check is what turns an
	// oversize UPLOAD into a clean 400 instead of a constraint violation.
	//
	// It is a len() on an already-decoded slice, so it costs nothing and it runs before any
	// header read: an oversized upload is refused before DecodeConfig touches it.
	if len(decoded) > maxCreativeStoredBytes {
		return nil, &briefs.BadRequestError{Code: "400", Message: "the uploaded image exceeds the maximum accepted size for a creative asset"}
	}

	// Validation runs in three stages, and the ORDER is the security property: the cheap header
	// read first, then the declared-size gate, and only then the allocating full decode. Each
	// stage bounds what the next one may spend.
	//
	// Stage 1 — image.DecodeConfig reads only the header. It costs nothing, and it yields both
	// the format and the DECLARED dimensions, which is what makes stage 2 possible before any
	// pixel buffer exists. Meta's creative POLICY (minimum dimensions, aspect ratio) is not
	// checked anywhere here: it is Meta-specific and belongs at dispatch, where Meta's API is the
	// authority — this endpoint is storage integrity, not platform policy.
	cfg, format, derr := image.DecodeConfig(bytes.NewReader(decoded))
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

	// Stage 2b — RESERVE the pixel buffer against the aggregate decode budget before stage 3
	// allocates it.
	//
	// Stage 2 bounds ONE image; it says nothing about how many decode at once, and the upstream
	// admission middleware cannot cover this because it prices permits from Content-Length.
	// Compression severs that relationship: a flat 4000x4000 PNG is ~68 KiB on the wire and
	// 61 MiB decoded, so wire-priced admission charges it the minimum and would let enough of
	// them through to exhaust the pod at the decode. The declared cost is knowable here — the
	// header has been read, nothing has been allocated — which is the earliest point an
	// aggregate bound can be applied at all.
	//
	// Shed rather than queue indefinitely: the caller gets the same retryable 503 the admission
	// middleware uses, for the same reason.
	release, ok := s.decodeReserver().reserve(ctx, decodedBytesFor(cfg.Width, cfg.Height, cfg.ColorModel))
	if !ok {
		return nil, &briefs.ConnServiceUnavailableError{Code: "503", Message: "the service is at upload capacity; retry shortly"}
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
	//
	// The reservation is released the moment this returns, on BOTH arms, and deliberately not
	// deferred to the method's return. What it reserves is the pixel buffer, which stops
	// existing here — the decoded image is discarded and only the verdict is kept. Holding it
	// across the checksum and the insert would keep decode capacity that nothing is occupying
	// for as long as the database takes, so a slow transaction would shed concurrent uploads
	// with 503 for memory already free. The failure arm releases too: an undecodable image
	// costs no pixel buffer either, and returning 400 while still holding its reservation
	// would leak the budget for the rest of the request.
	decodeErr := func() error {
		defer release()
		_, _, err := image.Decode(bytes.NewReader(decoded))
		return err
	}()
	if decodeErr != nil {
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
		ByteSize: int64(len(decoded)),
		// The dedupe/idempotency key. Content-derived, so a repeat upload of the same image
		// collides on (brief_id, checksum) and returns the existing asset.
		Checksum: sha256Hex(decoded),
		Bytes:    decoded,
		// Same nil handling as CreateBrief: a request with no decodable actor stores NULL rather
		// than refusing the write. On an idempotent re-upload the repo preserves the FIRST
		// uploader, so this attributes creation, not re-sending.
		CreatedBy: marshalActor(attributedActor(ctx, "upload creative asset")),
	}
	// BOUND the persistence span, because it is held under the admission permit.
	//
	// UploadAdmission takes its permit before next.ServeHTTP and releases it only after the whole
	// handler returns, so every span in here runs under it. CreateAsset is
	// BEGIN → SELECT ... FOR UPDATE on the parent brief → INSERT → COMMIT, and concurrent uploads
	// to the SAME brief serialize on that row lock. net/http gives a handler's r.Context() no
	// deadline — ReadTimeout and WriteTimeout install deadlines on the SOCKET and never cancel the
	// context — and there is no pool statement_timeout/lock_timeout and no http.TimeoutHandler on
	// the chain. Without a bound here a stalled or lock-blocked insert therefore pins its permit
	// until the client disconnects, and enough of those exhaust the upload budget with no memory
	// pressure at all: the control that exists to stop uploads denying service becomes the thing
	// denying it.
	//
	// UploadHandlerHeadroom is the budget rather than a new constant because it already NAMES this
	// span — the response time reserved inside WriteTimeout after the body is read, for exactly
	// "decode, persist, and write". Inventing a second number would leave two constants describing
	// one window, free to drift apart. A pool-level statement_timeout/lock_timeout would also bound
	// it, but it is set per-pool and would apply the upload's budget to every other query in the
	// service, so the bound is applied here where the span it protects is known.
	//
	// This is the same class as the decode wait: a bound that must not become a hang.
	insertCtx, cancelInsert := context.WithTimeout(ctx, constants.UploadHandlerHeadroom)
	defer cancelInsert()

	stored, created, err := repo.CreateAsset(insertCtx, asset)
	if err != nil {
		// Expiry is reported as an explicit, retryable 503 rather than falling through to
		// mapBriefErr's default 500. The distinction is real: the request did not fail because it
		// was malformed or because the server is broken, but because persistence did not complete
		// inside the budget — which a client SHOULD retry. It must never surface as a success or
		// an empty result: the transaction was cut off, so the asset may not be stored, and
		// telling the caller otherwise is the worse failure. errors.Is on the context sentinels
		// rather than on insertCtx.Err(), so a deadline propagated from the repo is caught too.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return nil, &briefs.ConnServiceUnavailableError{
				Code:    "503",
				Message: "storing the creative asset did not complete in time; retry shortly",
			}
		}
		return nil, mapBriefErr(err)
	}
	// created selects the response status in the generated encoder: 201 when this request stored
	// the asset, 200 when the idempotent path returned one that already existed. It is set ONLY
	// here, on the upload path — every other representation of a CreativeAsset leaves it nil, so
	// the field is omitted from those bodies.
	res := creativeAssetResult(stored)
	res.Created = createdTag(created)
	return res, nil
}

// createdTag renders the repository's "did this call insert" bool as the enum string the design
// declares. A string rather than a bool because Goa's response Tag matches on a string value.
func createdTag(created bool) *string {
	v := "false"
	if created {
		v = "true"
	}
	return &v
}

// Decode budget for an uploaded creative. The bound that matters is BYTES ALLOCATED during
// image.Decode, not pixels: pixels are only a proxy, and the conversion factor is a property of
// the decoded REPRESENTATION, which varies by bit depth.
//
// maxCreativeDecodedBytes is 80 MiB of decoded pixel buffer. That figure is deliberately the
// ceiling the previous pixel-only bound INTENDED — its comment promised ~76 MiB — rather than
// the ~152.6 MiB it actually permitted once 16-bit images are priced correctly. Setting the
// budget to that larger real number would have blessed the defect instead of fixing it: nothing
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
// code capped 20M pixels and its comment claimed ~76 MiB; the true worst case was 152.6 MiB
// (20,000,000 px x 8 B/px = 160,000,000 B, which is 160 MB decimal but 152.6 MiB binary — the
// same quantity in two units, which is why both are named explicitly here rather than left to
// the reader). The budget is now expressed in bytes and the per-pixel cost is read from the
// declared colour model, so a 16-bit image is charged what it actually costs.
//
// maxCreativeDimension additionally bounds each SIDE, because a byte budget alone admits a
// degenerate 1x20,000,000 strip that is no image any creative pipeline should accept.
const (
	maxCreativeDecodedBytes = 80 << 20 // 80 MiB of decoded pixel data
	maxCreativeDimension    = 10_000
	// maxCreativeStoredBytes is the ceiling on the STORED IMAGE FILE — the base64-DECODED body
	// bytes, which is what lands in the creative_assets.bytes column and what Meta caps at ~30 MB
	// for a single-image creative.
	//
	// It is enforced HERE rather than by the design's MaxLength because those constrain different
	// quantities. Goa publishes MaxLength as `maxLength` on the JSON STRING, and base64 expands by
	// 4/3, so declaring 30 MiB there rejected uploads at ~22.5 MiB decoded — inside what this
	// endpoint accepts. The design now declares the ENCODED ceiling (41,943,040 characters), which
	// is the unit that schema actually measures, and the decoded ceiling lives at the only layer
	// that sees decoded bytes: this one.
	//
	// Distinct from maxCreativeDecodedBytes above: that bounds the PIXEL BUFFER image.Decode
	// allocates (80 MiB), which compression makes unrelated to file size — a 68 KiB PNG can decode
	// to 61 MiB. This bounds the compressed file itself.
	maxCreativeStoredBytes = 31457280 // 30 MiB; at/above Meta's single-image max file size
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
	return decodedBytesFor(width, height, m) <= maxCreativeDecodedBytes
}

// decodedBytesFor is the pixel-buffer cost image.Decode will pay for a header-declared image.
//
// Extracted so the per-image gate above and the AGGREGATE decode reservation charge the same
// arithmetic. Two copies of this sum could drift, and the direction that matters is the
// reservation under-charging what the gate admits — the bound would then read as protection
// while permitting more than it accounts.
//
// Callers must have bounded width and height first (dimensionsWithinLimits does, via
// maxCreativeDimension). With both sides bounded the product cannot exceed 1e8 and the byte
// figure cannot exceed 8e8 — both well inside int64.
func decodedBytesFor(width, height int, m color.Model) int64 {
	if width <= 0 || height <= 0 {
		return 0
	}
	return int64(width) * int64(height) * bytesPerPixelFor(m)
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
