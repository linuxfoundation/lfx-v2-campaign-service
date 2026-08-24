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
// bytes for the shorter enum value ("image/png") and 40 for the longer
// ("image/jpeg"), putting the worst case at 41,943,080. A 40 MiB cap would therefore
// reject every maximum-size image by those 40 bytes. 42 MiB clears it with ~2 MiB of
// headroom, while still bounding the read at a fixed, modest multiple of the largest
// thing the contract admits.
//
// It applies to every route, not just the upload: no other endpoint takes a body
// remotely this large, so one ceiling costs them nothing and closes the same
// unbounded-read hole on all of them. Raising design/'s MaxLength requires raising
// this in step, or max-size uploads start failing with 413.
const MaxRequestBodyBytes int64 = 42 << 20 // 42 MiB

// PodMemoryLimitBytes mirrors resources.limits.memory in
// charts/lfx-v2-campaign-service/values.yaml (512Mi).
//
// It is duplicated here because the admission budget below has to be derived from a real
// number, and the authoritative one lives in a chart the Go build cannot read. Duplication
// across a repo boundary is exactly the kind of fact that goes stale silently, so it is not
// left to vigilance: TestPodMemoryLimitMatchesChart parses values.yaml and fails if the chart
// moves without this constant. Change one and the test names the other.
const PodMemoryLimitBytes int64 = 512 << 20 // 512 MiB

// UploadAdmissionBudgetBytes is the total in-flight upload allocation the process will admit at
// once, across all concurrent uploads.
//
// DERIVED, not chosen: it is one quarter of PodMemoryLimitBytes. The reasoning it encodes is
// that the pod's memory serves more than uploads — the Go runtime, the Postgres pool, the
// platform clients, the metrics registry and every non-upload request all live in the same
// limit — so uploads may claim only a minority share of it. A budget set near the pod limit
// would bound uploads against each other while still letting them starve everything else.
//
// The relationship is expressed as arithmetic on PodMemoryLimitBytes rather than written out as
// a literal, so raising the chart's limit raises this in step and cannot leave a stale figure
// behind. TestUploadAdmissionBudgetLeavesHeadroom asserts the headroom property directly.
const UploadAdmissionBudgetBytes int64 = PodMemoryLimitBytes / 4 // 128 MiB

// UploadAdmissionWeightBytes is the weight ONE upload takes from the budget.
//
// Priced at the transient peak a single maximum-size upload really reaches, not at any one of
// its parts: MaxRequestBodyBytes (42 MiB) buffered by the decoder, plus the ~30 MiB
// base64-decoded byte slice, plus the up-to-80 MiB pixel buffer image.Decode may allocate. That
// is ~152 MiB at the instant all three coexist, but the body buffer is released before the
// decode peaks, so 64 MiB is the honest steady-state weight — deliberately NOT the theoretical
// sum, which would admit only one upload at a time and make the endpoint serially slow for no
// memory benefit.
//
// With the budget above this admits two concurrent maximum-size uploads and sheds the third,
// which is the intended behaviour: two worst-case uploads sit inside the pod's share, three do
// not.
const UploadAdmissionWeightBytes int64 = 64 << 20 // 64 MiB

// UploadAdmissionWait is how long a request will wait for a permit before it is shed with 503.
//
// Short and bounded. Long enough to absorb ordinary arrival jitter so two near-simultaneous
// uploads do not shed one needlessly; far short of DefaultWriteTimeout, so a shed request is
// answered honestly rather than being killed by the write deadline with no response at all.
const UploadAdmissionWait = 250 * time.Millisecond

// DefaultReadTimeout bounds the time spent reading an ENTIRE request, headers and body.
//
// DefaultReadHeaderTimeout bounds only the headers, which leaves a slowloris able to dribble a
// body indefinitely — holding a connection, and on the upload route an admission permit, for as
// long as it likes. Without this, the admission budget above could be exhausted by clients that
// never send a complete request at all, which would turn the memory bound into a cheaper denial
// of service than the one it was added to prevent.
//
// It must exceed the time a legitimate maximum-size upload needs: 42 MiB over a slow but real
// connection. 120s is generous against that while still finite.
const DefaultReadTimeout = 120 * time.Second
