// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
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
// idempotent dedupe, parent-brief gate) is exercised against a fake/real repo in C7.
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
// The deferral note above points at C7, which is real (plan.md lists C1–C8) and still in progress.
// These three are pulled forward regardless: they are the endpoint's security boundary, and an
// unbound security guard is the one kind of gap that should not wait for a later phase.
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
