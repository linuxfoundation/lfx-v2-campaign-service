// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
	"github.com/linuxfoundation/lfx-v2-campaign-service/internal/domain/model"
)

// mp4Box encodes one ISOBMFF box with a 32-bit size.
func mp4Box(typ string, payload []byte) []byte {
	b := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(8+len(payload)))
	copy(b[4:], typ)
	return append(b, payload...)
}

// testMP4File is ftyp(brand) + moov + mdat.
func testMP4File(brand string) []byte {
	out := mp4Box("ftyp", []byte(brand+"\x00\x00\x02\x00isommp41"))
	out = append(out, mp4Box("moov", make([]byte, 24))...)
	return append(out, mp4Box("mdat", make([]byte, 64))...)
}

func TestMP4Structurally(t *testing.T) {
	good := testMP4File("isom")
	largesize := func() []byte {
		// The same mdat with a 64-bit largesize header (size field 1).
		out := mp4Box("ftyp", []byte("mp42\x00\x00\x00\x00"))
		out = append(out, mp4Box("moov", make([]byte, 8))...)
		hdr := make([]byte, 16)
		binary.BigEndian.PutUint32(hdr, 1)
		copy(hdr[4:], "mdat")
		binary.BigEndian.PutUint64(hdr[8:], 16+32)
		return append(append(out, hdr...), make([]byte, 32)...)
	}()
	toEnd := func() []byte {
		// The last box declares size 0: "extends to the end of the file".
		out := mp4Box("ftyp", []byte("iso5\x00\x00\x00\x00"))
		out = append(out, mp4Box("moov", make([]byte, 8))...)
		return append(append(out, 0, 0, 0, 0, 'm', 'd', 'a', 't'), make([]byte, 20)...)
	}()
	cases := []struct {
		name string
		b    []byte
		ok   bool
	}{
		{"isom", good, true},
		{"64-bit largesize", largesize, true},
		{"size 0 to end", toEnd, true},
		{"empty", nil, false},
		{"truncated", good[:len(good)-1], false},
		{"trailing bytes", append(append([]byte{}, good...), 1, 2, 3), false},
		{"quicktime brand", testMP4File("qt  "), false},
		{"heic brand", testMP4File("heic"), false},
		{"not ftyp first", append(mp4Box("moov", nil), good...), false},
		{"no moov", append(mp4Box("ftyp", []byte("isom\x00\x00\x00\x00")), mp4Box("mdat", nil)...), false},
		{"no mdat", append(mp4Box("ftyp", []byte("isom\x00\x00\x00\x00")), mp4Box("moov", nil)...), false},
		{"box smaller than its header", append(append([]byte{}, good[:len(good)-72]...), 0, 0, 0, 4, 'm', 'd', 'a', 't'), false},
		{"png bytes", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mp4Structurally(tc.b); got != tc.ok {
				t.Errorf("mp4Structurally = %v, want %v", got, tc.ok)
			}
		})
	}
}

// TestBriefService_UploadCreativeAsset_StoresMP4 pins the video arm (#586): a valid MP4 declared
// video/mp4 is stored as video/mp4 with no dimensions, and an MP4 declared as an image, or an
// image declared as video/mp4, is a 400 that stores nothing.
func TestBriefService_UploadCreativeAsset_StoresMP4(t *testing.T) {
	video := testMP4File("isom")
	repo := &fakeCreativeAssetRepo{}
	s := NewBriefService(nil, nil, nil, nil)
	s.SetCreativeAssetRepo(repo)
	out, err := s.UploadCreativeAsset(context.Background(), &briefs.UploadCreativeAssetPayload{
		ProjectID: "cncf", BriefID: "b1", ContentType: model.MimeTypeMP4, Bytes: b64(video),
	})
	if err != nil {
		t.Fatalf("UploadCreativeAsset: %v", err)
	}
	if repo.stored == nil || repo.stored.MimeType != model.MimeTypeMP4 || repo.stored.ByteSize != int64(len(video)) {
		t.Fatalf("stored = %+v, want video/mp4 of %d bytes", repo.stored, len(video))
	}
	if repo.stored.Width != 0 || repo.stored.Height != 0 || out.Width != nil || out.Height != nil {
		t.Errorf("a video must carry no dimensions: stored %dx%d, result %v x %v", repo.stored.Width, repo.stored.Height, out.Width, out.Height)
	}
	if out.MimeType != model.MimeTypeMP4 {
		t.Errorf("result MimeType = %q, want video/mp4", out.MimeType)
	}

	for name, p := range map[string]*briefs.UploadCreativeAssetPayload{
		"mp4 declared as png": {ProjectID: "cncf", BriefID: "b1", ContentType: model.MimeTypePNG, Bytes: b64(video)},
		"png declared as mp4": {ProjectID: "cncf", BriefID: "b1", ContentType: model.MimeTypeMP4, Bytes: b64(pngBytes(t))},
		"truncated mp4":       {ProjectID: "cncf", BriefID: "b1", ContentType: model.MimeTypeMP4, Bytes: b64(video[:len(video)-5])},
		"quicktime as mp4":    {ProjectID: "cncf", BriefID: "b1", ContentType: model.MimeTypeMP4, Bytes: b64(testMP4File("qt  "))},
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
