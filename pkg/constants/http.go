// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package constants

import "time"

const (
	RequestIDHeader = "X-Request-ID"

	DefaultShutdownTimeout   = 25 * time.Second
	DefaultReadHeaderTimeout = 60 * time.Second
	DefaultWriteTimeout      = 60 * time.Second
	DefaultIdleTimeout       = 90 * time.Second
)

// MaxRequestBodyBytes caps how many bytes the server will read from any request
// body before refusing it with 413.
//
// It exists because the size bounds declared in design/ do NOT bound the wire. The
// creative-asset upload declares MaxLength(31457280) on a Goa `Bytes` attribute, but
// the generated validator tests len() on the DECODED slice, and it only ever sees
// that slice after goahttp.RequestDecoder's json.Decoder has read the entire request
// body and base64-decoded it. Without an inbound cap an unauthenticated caller
// streams an arbitrarily large body and the server buffers and decodes all of it
// before any declared limit is consulted.
//
// The value is derived, not guessed. Base64 expands by exactly 4/3 with padding
// (encoding/json decodes a []byte field via base64.StdEncoding), so the largest
// LEGAL upload — 30 MiB of image — arrives as 41,943,040 base64 characters, which is
// 40 MiB to the byte. Only two fields ride in the body — content_type and bytes;
// project_id and brief_id are PATH parameters — so the JSON envelope adds just 39
// bytes, for a measured worst-case body of 41,943,079. A 40 MiB cap would therefore
// reject every maximum-size image by those 39 bytes. 42 MiB clears it with ~2 MiB of
// headroom, while still bounding the read at a fixed, modest multiple of the largest
// thing the contract admits.
//
// It applies to every route, not just the upload: no other endpoint takes a body
// remotely this large, so one ceiling costs them nothing and closes the same
// unbounded-read hole on all of them. Raising design/'s MaxLength requires raising
// this in step, or max-size uploads start failing with 413.
const MaxRequestBodyBytes int64 = 42 << 20 // 42 MiB
