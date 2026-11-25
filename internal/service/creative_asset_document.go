// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"bytes"

	briefs "github.com/linuxfoundation/lfx-v2-campaign-service/gen/lfx_v2_campaign_service_briefs"
)

// PDF document uploads (#665).
//
// A stored PDF is the source of a LinkedIn document ad (linkedInConfig.documentAssetId), uploaded
// through LinkedIn's Documents API at dispatch. Like an MP4 it is checked STRUCTURALLY, not
// rendered: the bytes must open with the PDF header ("%PDF-" and a 1.x or 2.x version, ISO 32000)
// and carry the end-of-file marker "%%EOF" in their last pdfTrailerWindow bytes, where a complete
// file ends (trailing whitespace and a final incremental-update section are tolerated by the
// window). That refuses a truncated upload and anything else declared as application/pdf; whether
// LinkedIn can render the document (pages, fonts, the 300-page limit) is LinkedIn's to judge, and
// a document it cannot use ends PROCESSING_FAILED before any post references it. A PDF is stored
// with no width or height.

// pdfTrailerWindow is how far from the end "%%EOF" may sit. PDF readers search the last 1024
// bytes for it, so a file whose marker sits further back is not one a reader opens either.
const pdfTrailerWindow = 1024

// minPDFBytes is the smallest upload worth a structural look: a header, one object, a
// cross-reference section and the trailer cannot fit in less.
const minPDFBytes = 64

// validatePDFUpload reports whether b is a structurally complete PDF file. Its error is a
// ready-to-return Goa 400 with a fixed message that never quotes the bytes.
func validatePDFUpload(b []byte) error {
	if !pdfStructurally(b) {
		return &briefs.BadRequestError{Code: "400", Message: "the uploaded bytes are not a complete PDF document"}
	}
	return nil
}

// pdfStructurally checks the header and the end-of-file marker; see the file comment.
func pdfStructurally(b []byte) bool {
	if len(b) < minPDFBytes || !bytes.HasPrefix(b, []byte("%PDF-")) {
		return false
	}
	// "%PDF-1.7" or "%PDF-2.0": a major version of 1 or 2, a dot, a digit.
	if (b[5] != '1' && b[5] != '2') || b[6] != '.' || b[7] < '0' || b[7] > '9' {
		return false
	}
	tail := b
	if len(tail) > pdfTrailerWindow {
		tail = tail[len(tail)-pdfTrailerWindow:]
	}
	return bytes.Contains(tail, []byte("%%EOF"))
}
