// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"image"
	// Registers the GIF decoder for the TEST BINARY ONLY. It is deliberately absent from
	// creative_asset.go: this import reproduces the condition the handler comment says must stay
	// safe (another package registering a decoder) so the allow-list guard becomes reachable and
	// a widening mutation is caught. Adding it to the service binary is NOT required for the
	// guard to hold — that is exactly what the test proves.
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// TestBriefService_UploadCreativeAsset_UnavailableWithoutRepo pins that a BriefService with no
// creative-asset repository bound — the no-database and cold-start-pending modes — reports 503
// rather than nil-panicking, mirroring how GenerateEmailCopy / FetchEventURL check their own
// late-bound collaborators. The wording is availability-neutral (not "not configured") because a
// cold start has the database configured but the repo not yet bound.
//
// This runs BEFORE the image sniff: an unwired service must 503 regardless of what bytes arrive,
// so the payload here is a valid PNG header precisely to show the repo check short-circuits ahead
// of validation. The full validate-and-persist matrix (sniff, declared/sniffed match, checksum,
// idempotent dedupe, parent-brief gate) is enforced by the repository and exercised against a
// real database in internal/infrastructure/postgres (creative_asset_repo_live_test.go), not here.
func TestBriefService_UploadCreativeAsset_UnavailableWithoutRepo(t *testing.T) {
	s := NewBriefService(nil, nil, nil, nil)

	_, err := s.UploadCreativeAsset(context.Background(), &briefs.UploadCreativeAssetPayload{
		ProjectID:   "cncf",
		BriefID:     "b1",
		ContentType: "image/png",
		Bytes:       []byte{0x89, 0x50, 0x4e, 0x47}, // PNG magic, enough to look like an upload
	})

	var svcErr *briefs.ConnServiceUnavailableError
	if !errors.As(err, &svcErr) {
		t.Fatalf("UploadCreativeAsset: err = %T (%v), want *briefs.ConnServiceUnavailableError", err, err)
	}
	if svcErr.Code != "503" {
		t.Errorf("code = %q, want %q", svcErr.Code, "503")
	}
	if svcErr.Message != "creative asset upload is unavailable" {
		t.Errorf("message = %q, want %q", svcErr.Message, "creative asset upload is unavailable")
	}
}

// fakeCreativeAssetRepo records what reached storage. It stores rather than validates, so a guard
// deleted from the handler shows up here as bytes that should never have been persisted.
type fakeCreativeAssetRepo struct {
	stored *model.CreativeAsset
	err    error
}

func (f *fakeCreativeAssetRepo) CreateAsset(_ context.Context, a *model.CreativeAsset) (*model.CreativeAsset, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.stored = a
	out := *a
	out.ID = "asset-1"
	return &out, nil
}

func (f *fakeCreativeAssetRepo) GetAsset(_ context.Context, _, _, _ string) (*model.CreativeAsset, error) {
	return nil, domain.ErrNotFound
}

// pngBytes is the smallest real PNG that image.DecodeConfig accepts: signature + a complete IHDR.
// A hand-rolled header would be rejected for the wrong reason and the test would pass vacuously.
func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

// TestBriefService_UploadCreativeAsset_ValidationGuards binds the three checks that stand between
// arbitrary bytes and storage. Before this test, DELETING ANY OF THEM BROKE NOTHING — verified by
// removing the declared/sniffed mismatch guard and watching the whole suite stay green.
//
// The dedupe and parent-brief behaviours the note above defers belong to the repository and are
// covered by its live tests. These three do NOT belong there: they are the endpoint's own
// security boundary, and an unbound security guard is the one kind of gap that should not wait.
//
// Each case asserts BOTH the rejection AND that nothing was stored. Asserting only the error would
// pass against a handler that rejected the request after persisting the bytes.
func TestBriefService_UploadCreativeAsset_ValidationGuards(t *testing.T) {
	valid := pngBytes(t)

	cases := []struct {
		name        string
		contentType string
		payload     []byte
		wantMessage string
	}{
		{
			// Garbage that no decoder recognises. A declared content_type alone would wave it
			// through, which is the whole reason the sniff exists.
			"undecodable bytes are refused",
			"image/png",
			[]byte("this is not an image at all, it is prose"),
			"not a decodable",
		},
		{
			// Truncated: a real PNG signature with no complete IHDR. Distinct from garbage because
			// it is the shape a partial upload actually takes.
			"a truncated image is refused",
			"image/png",
			valid[:8],
			"not a decodable",
		},
		{
			// The bytes ARE an image and the declaration IS an accepted type — they simply do not
			// agree. Storing under the declared type would make mime_type a lie about the content.
			"a declared type that contradicts the bytes is refused",
			"image/jpeg",
			valid,
			"does not match",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeCreativeAssetRepo{}
			s := NewBriefService(nil, nil, nil, nil)
			s.SetCreativeAssetRepo(repo)

			_, err := s.UploadCreativeAsset(context.Background(), &briefs.UploadCreativeAssetPayload{
				ProjectID:   "cncf",
				BriefID:     "b1",
				ContentType: tc.contentType,
				Bytes:       tc.payload,
			})

			var badReq *briefs.BadRequestError
			if !errors.As(err, &badReq) {
				t.Fatalf("err = %T (%v), want *briefs.BadRequestError", err, err)
			}
			if !strings.Contains(badReq.Message, tc.wantMessage) {
				t.Errorf("message = %q, want it to contain %q", badReq.Message, tc.wantMessage)
			}
			// The half that makes this a security test rather than an error-shape test.
			if repo.stored != nil {
				t.Errorf("bytes reached storage despite rejection: %+v", repo.stored)
			}
		})
	}
}

// TestBriefService_UploadCreativeAsset_StoresSniffedTypeAndChecksum binds the two values the
// handler DERIVES rather than accepts. The checksum is the idempotency key for the whole feature —
// UNIQUE (brief_id, checksum) in migration 000026 — and nothing at this layer pinned it.
func TestBriefService_UploadCreativeAsset_StoresSniffedTypeAndChecksum(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bytes    []byte
		wantMIME string
	}{
		{"png", pngBytes(t), "image/png"},
		{"jpeg", jpegBytes(t), "image/jpeg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeCreativeAssetRepo{}
			s := NewBriefService(nil, nil, nil, nil)
			s.SetCreativeAssetRepo(repo)

			out, err := s.UploadCreativeAsset(context.Background(), &briefs.UploadCreativeAssetPayload{
				ProjectID:   "cncf",
				BriefID:     "b1",
				ContentType: tc.wantMIME,
				Bytes:       tc.bytes,
			})
			if err != nil {
				t.Fatalf("UploadCreativeAsset: %v", err)
			}
			if out == nil || out.ID == "" {
				t.Fatalf("result = %+v, want a stored asset id", out)
			}
			if repo.stored.MimeType != tc.wantMIME {
				t.Errorf("stored MimeType = %q, want the SNIFFED %q", repo.stored.MimeType, tc.wantMIME)
			}
			sum := sha256.Sum256(tc.bytes)
			if want := hex.EncodeToString(sum[:]); repo.stored.Checksum != want {
				t.Errorf("stored Checksum = %q, want sha256 of the bytes %q", repo.stored.Checksum, want)
			}
		})
	}
}

// TestBriefService_UploadCreativeAsset_RefusesDecodableFormatOutsideAllowList binds the
// authority mimeForImageFormat CLAIMS but, before this test, did not have.
//
// The handler's blank imports (image/png, image/jpeg) are described in its own comment as the
// allow-list's UPPER bound, with mimeForImageFormat named as the authoritative allow-list — the
// stated reason an unrelated package importing another decoder cannot widen this endpoint. That
// claim was untested. Every existing case feeds PNG or JPEG, so mutating mimeForImageFormat to
// `return "image/" + format, true` — accepting ANY format some decoder recognises — changed no
// test result. The guard was unreachable only by the accident that no GIF decoder happened to be
// registered in this binary, which is a property of the whole program's import graph, not of this
// package: one `_ "image/gif"` anywhere in any transitively-imported package silently widens the
// endpoint to store GIFs under a mime_type no CHECK constraint, no OpenAPI enum, and no Meta
// creative path expects.
//
// Importing image/gif HERE, in the _test package only, is what makes the guard reachable. It
// registers the decoder for this test binary — reproducing exactly the condition the comment
// says must stay safe — without adding it to the service binary's import graph. The GIF then
// decodes successfully (so this is NOT the DecodeConfig-failure path) and must still be refused,
// with the allow-list's own distinct message, and must not reach storage.
func TestBriefService_UploadCreativeAsset_RefusesDecodableFormatOutsideAllowList(t *testing.T) {
	// A real, decodable GIF — not hand-rolled bytes, which would be refused as undecodable and
	// pass this test for the wrong reason entirely.
	var buf bytes.Buffer
	if err := gif.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatalf("encode gif: %v", err)
	}
	raw := buf.Bytes()

	// Guard the guard: if this ever stops decoding, the test below would pass via the
	// undecodable-bytes arm and silently stop covering the allow-list.
	if _, format, err := image.DecodeConfig(bytes.NewReader(raw)); err != nil || format != "gif" {
		t.Fatalf("fixture must decode as gif, got format=%q err=%v", format, err)
	}

	for _, declared := range []string{"image/png", "image/jpeg"} {
		t.Run(declared, func(t *testing.T) {
			repo := &fakeCreativeAssetRepo{}
			s := NewBriefService(nil, nil, nil, nil)
			s.SetCreativeAssetRepo(repo)

			_, err := s.UploadCreativeAsset(context.Background(), &briefs.UploadCreativeAssetPayload{
				ProjectID:   "cncf",
				BriefID:     "b1",
				ContentType: declared,
				Bytes:       raw,
			})

			var badReq *briefs.BadRequestError
			if !errors.As(err, &badReq) {
				t.Fatalf("err = %T (%v), want *briefs.BadRequestError refusing a non-allow-listed format", err, err)
			}
			// The allow-list's OWN message, not the decode-failure one. Asserting merely "some
			// 400" would pass against a handler that refused this for the wrong reason, and the
			// two are deliberately kept distinct.
			if !strings.Contains(badReq.Message, "unsupported image format") {
				t.Errorf("message = %q, want the allow-list refusal (%q)", badReq.Message, "unsupported image format")
			}
			if repo.stored != nil {
				t.Errorf("a non-allow-listed format reached storage: mime=%q size=%d", repo.stored.MimeType, repo.stored.ByteSize)
			}
		})
	}
}

// TestMimeForImageFormat_AllowListIsExactlyPNGAndJPEG pins the allow-list directly, one layer
// below the handler. The handler test above proves the refusal happens; this proves the function
// named as authoritative maps exactly two formats and reports false for everything else,
// including formats whose decoders are registered in this binary (gif, via the import above) and
// ones that are not. Together they mean a widening mutation cannot survive at either layer.
func TestMimeForImageFormat_AllowListIsExactlyPNGAndJPEG(t *testing.T) {
	for _, tc := range []struct {
		format   string
		wantMIME string
		wantOK   bool
	}{
		{"png", model.MimeTypePNG, true},
		{"jpeg", model.MimeTypeJPEG, true},
		// Decodable in this test binary, still not accepted.
		{"gif", "", false},
		// Formats a future import could register.
		{"webp", "", false},
		{"bmp", "", false},
		{"tiff", "", false},
		// Not a format at all.
		{"", "", false},
	} {
		t.Run(tc.format, func(t *testing.T) {
			gotMIME, gotOK := mimeForImageFormat(tc.format)
			if gotOK != tc.wantOK {
				t.Errorf("mimeForImageFormat(%q) ok = %v, want %v", tc.format, gotOK, tc.wantOK)
			}
			if gotMIME != tc.wantMIME {
				t.Errorf("mimeForImageFormat(%q) mime = %q, want %q", tc.format, gotMIME, tc.wantMIME)
			}
		})
	}
}

// truncatedAfterIHDR returns a PNG cut off immediately after its IHDR chunk: the 8-byte
// signature plus the 25-byte IHDR (4 length + 4 type + 13 data + 4 CRC). image.DecodeConfig
// succeeds on it — that is the whole point — while a full decode fails with unexpected EOF.
func truncatedAfterIHDR(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 64, 64))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()[:33]
}

// TestBriefService_UploadCreativeAsset_RefusesTruncatedImageData binds the gap between "a header
// parses" and "these bytes are an image".
//
// image.DecodeConfig stops as soon as it has the format and dimensions, so a PNG truncated right
// after IHDR passes it while carrying no recoverable pixel data at all. Before this, such an
// upload was ACCEPTED AND PERSISTED, and only failed much later at dispatch — corrupt bytes
// stored as a valid asset, far from the upload that caused them.
//
// The fixture is the load-bearing part, and it is asserted rather than assumed. A test whose
// bytes were rejected by an EARLIER guard (undecodable header, wrong sniffed type, oversized
// dimensions) would pass while proving nothing about the full-decode stage — the same shape as
// the vacuous chunked-body fixture caught earlier on this PR. So the sub-assertions below pin
// that this input clears stage 1 and stage 2 and can only be refused by stage 3.
func TestBriefService_UploadCreativeAsset_RefusesTruncatedImageData(t *testing.T) {
	raw := truncatedAfterIHDR(t)

	// Premise 1: the header still parses as a 64x64 PNG, so stage 1 cannot be what rejects this.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || format != "png" {
		t.Fatalf("fixture must pass DecodeConfig as png, got format=%q err=%v", format, err)
	}
	// Premise 2: its dimensions are ordinary, so stage 2 cannot be what rejects it either.
	if !dimensionsWithinLimits(cfg.Width, cfg.Height) {
		t.Fatalf("fixture dimensions %dx%d must be within limits", cfg.Width, cfg.Height)
	}
	// Premise 3: a real decode genuinely fails — the defect being closed is real, not imagined.
	if _, derr := png.Decode(bytes.NewReader(raw)); derr == nil {
		t.Fatal("fixture must fail a full decode; if it decodes, it is not truncated")
	}

	repo := &fakeCreativeAssetRepo{}
	s := NewBriefService(nil, nil, nil, nil)
	s.SetCreativeAssetRepo(repo)

	_, uerr := s.UploadCreativeAsset(context.Background(), &briefs.UploadCreativeAssetPayload{
		ProjectID:   "cncf",
		BriefID:     "b1",
		ContentType: "image/png",
		Bytes:       raw,
	})

	var badReq *briefs.BadRequestError
	if !errors.As(uerr, &badReq) {
		t.Fatalf("err = %T (%v), want *briefs.BadRequestError", uerr, uerr)
	}
	// The full-decode stage's OWN message. Asserting merely "some 400" would pass if an earlier
	// guard had rejected it, which is exactly what the premises above rule out.
	if !strings.Contains(badReq.Message, "incomplete or corrupt") {
		t.Errorf("message = %q, want the full-decode refusal (%q)", badReq.Message, "incomplete or corrupt")
	}
	if repo.stored != nil {
		t.Errorf("truncated image data reached storage: mime=%q size=%d", repo.stored.MimeType, repo.stored.ByteSize)
	}
}

// TestBriefService_UploadCreativeAsset_RefusesOversizeDimensionsWithoutDecoding binds the
// decompression-bomb gate, and specifically that it fires BEFORE the allocating decode.
//
// Both PNG and JPEG compress a uniform image enormously, so a body far inside the 42 MiB request
// cap can declare dimensions whose decoded RGBA form is gigabytes. Checking dimensions only
// after decoding would be no protection at all — the allocation is the attack.
//
// Proving "without decoding" needs care: the handler returns the same error type either way. So
// the fixture is built so that a full decode CANNOT succeed on it — the header declares enormous
// dimensions the truncated body cannot satisfy — and the assertion is on the DIMENSION message.
// If the dimension gate were removed, control would reach the decode stage and report the
// corrupt-data message instead, so the two arms are distinguishable by message alone.
func TestBriefService_UploadCreativeAsset_RefusesOversizeDimensionsWithoutDecoding(t *testing.T) {
	raw := pngHeaderWithDimensions(t, 30000, 30000)

	// Premise: the header parses and reports the huge dimensions, so this reaches stage 2.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || format != "png" {
		t.Fatalf("fixture must pass DecodeConfig as png, got format=%q err=%v", format, err)
	}
	if cfg.Width != 30000 || cfg.Height != 30000 {
		t.Fatalf("fixture dims = %dx%d, want 30000x30000", cfg.Width, cfg.Height)
	}

	repo := &fakeCreativeAssetRepo{}
	s := NewBriefService(nil, nil, nil, nil)
	s.SetCreativeAssetRepo(repo)

	_, uerr := s.UploadCreativeAsset(context.Background(), &briefs.UploadCreativeAssetPayload{
		ProjectID:   "cncf",
		BriefID:     "b1",
		ContentType: "image/png",
		Bytes:       raw,
	})

	var badReq *briefs.BadRequestError
	if !errors.As(uerr, &badReq) {
		t.Fatalf("err = %T (%v), want *briefs.BadRequestError", uerr, uerr)
	}
	if !strings.Contains(badReq.Message, "dimensions exceed") {
		t.Errorf("message = %q, want the dimension refusal — anything else means the bomb reached the decoder", badReq.Message)
	}
	if repo.stored != nil {
		t.Errorf("an oversize-dimension image reached storage: %+v", repo.stored)
	}
}

// pngHeaderWithDimensions builds a PNG signature + IHDR declaring the given size, and nothing
// else. It is a decompression-bomb stand-in: the header claims an enormous image while the body
// is 33 bytes, so reaching a full decode would be the bug.
func pngHeaderWithDimensions(t *testing.T, width, height uint32) []byte {
	t.Helper()

	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 6 // colour type: RGBA
	// ihdr[10..12] = compression, filter, interlace — all 0.

	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(ihdr)))
	chunk := append([]byte("IHDR"), ihdr...)
	buf.Write(chunk)
	_ = binary.Write(&buf, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	return buf.Bytes()
}

// TestDimensionsWithinLimits pins the gate directly, including the degenerate shapes the pixel
// product alone would admit. A 1x20,000,000 strip is inside the area budget yet is not an image
// any creative pipeline should accept, which is why each SIDE is bounded too.
func TestDimensionsWithinLimits(t *testing.T) {
	for _, tc := range []struct {
		name          string
		width, height int
		want          bool
	}{
		{"Meta recommended feed", 1080, 1080, true},
		{"story 9:16", 1080, 1920, true},
		{"4K UHD, the largest plausible creative", 3840, 2160, true},
		{"exactly at the per-side limit", maxCreativeDimension, 1, true},
		{"one past the per-side limit", maxCreativeDimension + 1, 1, false},
		{"within both sides but past the pixel budget", 9000, 9000, false},
		{"degenerate strip inside the area budget", 1, 20_000_000, false},
		{"classic bomb", 30000, 30000, false},
		{"zero width is not a size", 0, 100, false},
		{"zero height is not a size", 100, 0, false},
		{"negative is not a size", -1, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dimensionsWithinLimits(tc.width, tc.height); got != tc.want {
				t.Errorf("dimensionsWithinLimits(%d, %d) = %v, want %v", tc.width, tc.height, got, tc.want)
			}
		})
	}
}
