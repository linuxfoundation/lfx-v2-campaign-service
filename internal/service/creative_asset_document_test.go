// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"
	"context"
	"errors"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// testPDFFile is a minimal structurally complete PDF: header, one object, xref, trailer, %%EOF.
func testPDFFile() []byte {
	return []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\nxref\n0 1\n0000000000 65535 f \ntrailer\n<< /Root 1 0 R >>\nstartxref\n9\n%%EOF\n")
}

func TestPDFStructurally(t *testing.T) {
	good := testPDFFile()
	// A file whose %%EOF sits more than 1024 bytes from the end: readers do not find it there.
	farEOF := append(append([]byte{}, good...), bytes.Repeat([]byte(" "), pdfTrailerWindow+1)...)
	for _, tc := range []struct {
		name string
		b    []byte
		ok   bool
	}{
		{"pdf 1.7", good, true},
		{"pdf 2.0", append([]byte("%PDF-2.0"), good[8:]...), true},
		{"trailing whitespace", append(append([]byte{}, good...), "\r\n"...), true},
		{"trailing tab, space and NUL", append(append([]byte{}, good...), "\t \x00"...), true},
		// An incremental update appended after the first revision's %%EOF and cut off before its
		// own: the earlier marker is inside the window, but it is not the end of the file.
		{"truncated incremental update after an earlier %%EOF", append(append([]byte{}, good...), "2 0 obj\n<< /Type /Pages /Kids [] /Count 0 >>\nendobj\nxref\n0 3\n"...), false},
		{"bytes after the final %%EOF", append(append([]byte{}, good...), "junk"...), false},
		{"complete incremental update", append(append([]byte{}, good...), "2 0 obj\n<< >>\nendobj\ntrailer\n<< /Prev 9 >>\n%%EOF\n"...), true},
		{"empty", nil, false},
		{"too small", []byte("%PDF-1.7\n%%EOF"), false},
		{"truncated (no %%EOF)", good[:len(good)-7], false},
		{"%%EOF beyond the trailer window", farEOF, false},
		{"version 3", append([]byte("%PDF-3.0"), good[8:]...), false},
		{"no dot", append([]byte("%PDF-1x7"), good[8:]...), false},
		{"png bytes", append([]byte("\x89PNG\r\n\x1a\n"), good...), false},
		{"header not at offset 0", append([]byte(" "), good...), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pdfStructurally(tc.b); got != tc.ok {
				t.Errorf("pdfStructurally = %v, want %v", got, tc.ok)
			}
		})
	}
}

// TestBriefService_UploadCreativeAsset_StoresPDF pins the document arm (#665): a valid PDF declared
// application/pdf is stored as application/pdf with no dimensions, and a PDF declared as an image
// or a video, or other bytes declared as a PDF, is a 400 that stores nothing.
func TestBriefService_UploadCreativeAsset_StoresPDF(t *testing.T) {
	doc := testPDFFile()
	repo := &fakeCreativeAssetRepo{}
	s := NewBriefService(nil, nil, nil, nil)
	s.SetCreativeAssetRepo(repo)
	out, err := s.UploadCreativeAsset(context.Background(), &briefs.UploadCreativeAssetPayload{
		ProjectID: "cncf", BriefID: "b1", ContentType: model.MimeTypePDF, Bytes: b64(doc),
	})
	if err != nil {
		t.Fatalf("UploadCreativeAsset: %v", err)
	}
	if repo.stored == nil || repo.stored.MimeType != model.MimeTypePDF || repo.stored.ByteSize != int64(len(doc)) {
		t.Fatalf("stored = %+v, want application/pdf of %d bytes", repo.stored, len(doc))
	}
	if repo.stored.Width != 0 || repo.stored.Height != 0 || out.Width != nil || out.Height != nil {
		t.Errorf("a PDF must carry no dimensions: stored %dx%d", repo.stored.Width, repo.stored.Height)
	}
	if out.MimeType != model.MimeTypePDF {
		t.Errorf("result MimeType = %q, want application/pdf", out.MimeType)
	}

	for name, p := range map[string]*briefs.UploadCreativeAssetPayload{
		"pdf declared as png": {ProjectID: "cncf", BriefID: "b1", ContentType: model.MimeTypePNG, Bytes: b64(doc)},
		"pdf declared as mp4": {ProjectID: "cncf", BriefID: "b1", ContentType: model.MimeTypeMP4, Bytes: b64(doc)},
		"png declared as pdf": {ProjectID: "cncf", BriefID: "b1", ContentType: model.MimeTypePDF, Bytes: b64(pngBytes(t))},
		"mp4 declared as pdf": {ProjectID: "cncf", BriefID: "b1", ContentType: model.MimeTypePDF, Bytes: b64(testMP4File("isom"))},
		"truncated pdf":       {ProjectID: "cncf", BriefID: "b1", ContentType: model.MimeTypePDF, Bytes: b64(doc[:len(doc)-7])},
	} {
		t.Run(name, func(t *testing.T) {
			repo := &fakeCreativeAssetRepo{}
			s := NewBriefService(nil, nil, nil, nil)
			s.SetCreativeAssetRepo(repo)
			_, err := s.UploadCreativeAsset(context.Background(), p)
			var bad *briefs.BadRequestError
			if !errors.As(err, &bad) {
				t.Fatalf("err = %v, want a 400", err)
			}
			if repo.stored != nil {
				t.Error("a refused upload stored an asset")
			}
		})
	}
}
