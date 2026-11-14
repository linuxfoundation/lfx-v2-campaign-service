// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"encoding/binary"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
)

// MP4 creative assets (#586).
//
// A video is checked STRUCTURALLY, never decoded: this service has no video decoder and needs
// none. What the image path's full decode proves for an image — that the bytes are a complete
// file of the declared type, not a truncated or mislabelled one — is proved here by walking the
// ISO base media file format's top-level boxes (ISO/IEC 14496-12 §4.2):
//
//   - the file opens with an ftyp box whose major brand is an MP4 brand. The brand check is what
//     keeps out the other ISOBMFF formats that also open with ftyp: QuickTime (.mov, brand "qt  "),
//     and the HEIF/AVIF still-image formats;
//   - the top-level boxes tile the file EXACTLY: every declared size is at least its own header,
//     none runs past the end, and the last one ends on the final byte. A truncated upload fails
//     here, because its last box declares more bytes than arrived;
//   - a moov box (the movie metadata) and an mdat box (the media data) are both present.
//
// Codec, duration, frame rate and dimensions are NOT checked here: they are platform policy, and
// the platform that consumes the video decides them (LinkedIn's Videos API reports
// PROCESSING_FAILED for a video it cannot use, which the dispatch reports before any post
// references it). This endpoint is storage integrity, as for images.

// mp4MajorBrands is the allow-list of ftyp major brands accepted as MP4. Each is an ISO/IEC
// 14496-12 / 14496-14 brand or the de-facto brand an encoder writes for an MP4 file.
var mp4MajorBrands = map[string]bool{
	"isom": true, "iso2": true, "iso3": true, "iso4": true, "iso5": true, "iso6": true,
	"mp41": true, "mp42": true, "avc1": true, "M4V ": true, "mmp4": true, "dash": true, "MSNV": true,
}

// validateMP4Upload reports whether b is a structurally complete MP4 file. Its error is a
// ready-to-return Goa 400 with a fixed message that never quotes the bytes.
func validateMP4Upload(b []byte) error {
	if !mp4Structurally(b) {
		return &briefs.BadRequestError{Code: "400", Message: "the uploaded bytes are not a complete MP4 video"}
	}
	return nil
}

// mp4Structurally walks b's top-level boxes; see the file comment for what it requires.
func mp4Structurally(b []byte) bool {
	var off uint64
	n := uint64(len(b))
	first, sawMoov, sawMdat := true, false, false
	for off < n {
		if n-off < 8 {
			return false
		}
		size := uint64(binary.BigEndian.Uint32(b[off : off+4]))
		typ := string(b[off+4 : off+8])
		header := uint64(8)
		switch size {
		case 0:
			// "box extends to the end of the file".
			size = n - off
		case 1:
			// A 64-bit largesize follows the type.
			if n-off < 16 {
				return false
			}
			size = binary.BigEndian.Uint64(b[off+8 : off+16])
			header = 16
		}
		if size < header || size > n-off {
			return false
		}
		if first {
			// ftyp is major_brand(4) + minor_version(4) + compatible brands.
			if typ != "ftyp" || size < header+8 || !mp4MajorBrands[string(b[off+header:off+header+4])] {
				return false
			}
			first = false
		}
		switch typ {
		case "moov":
			sawMoov = true
		case "mdat":
			sawMdat = true
		}
		off += size
	}
	return !first && sawMoov && sawMdat
}
