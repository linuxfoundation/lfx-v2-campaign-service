// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package constants

import "time"

const (
	RequestIDHeader = "X-Request-ID"

	DefaultShutdownTimeout   = 25 * time.Second
	DefaultReadHeaderTimeout = 15 * time.Second
	// 120s, not 60s: this deadline is installed when the request HEADERS are read and keeps
	// expiring through the body read AND the handler, so it must cover DefaultReadTimeout
	// (90s) plus UploadHandlerHeadroom (30s). At 60s a legitimate slow upload could finish
	// reading with no budget left to answer.
	DefaultWriteTimeout = 120 * time.Second
	DefaultIdleTimeout  = 90 * time.Second
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
// Priced at the peak that PROVABLY coexists, not at a steady state. An earlier revision charged
// 64 MiB on the reasoning that the body buffer is released before the decode peaks. That was
// wrong in a way the code contradicts directly: UploadCreativeAsset calls
// image.Decode(bytes.NewReader(p.Bytes)), so the ~30 MiB decoded slice is live FOR THE WHOLE
// decode — it is the decode's own input and cannot be collected while it runs. The 80 MiB pixel
// buffer is allocated on top of it, so at least ~110 MiB necessarily coexists per upload.
//
// 110 MiB is therefore the floor, and the Goa decoder's ~42 MiB body buffer may still be
// unreclaimed on top of that (Go frees on GC, not on last use), so the honest figure is 128 MiB.
// Charging less would let two permits admit ~220 MiB against a 128 MiB budget — a bound that
// under-counts precisely when it matters, which is worse than no bound because it reads as
// protection.
//
// The consequence is deliberate and worth stating: at the current budget this admits ONE
// maximum-size upload at a time and sheds concurrent ones with 503 + Retry-After. That is the
// correct answer for a 512Mi pod — two worst-case uploads genuinely do not fit — and it is why
// the shed path returns a retryable status rather than an error. Smaller images still decode
// well within the same permit; the weight is priced for the worst legal upload, not the typical
// one.
const UploadAdmissionWeightBytes int64 = 128 << 20 // 128 MiB

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
// The deadlines share one clock, and the binding constraint is net/http's ordering: the WRITE
// deadline is installed when the request HEADERS are read, and it keeps expiring while the
// handler reads the body AND while the handler runs. So every second spent reading the body is a
// second subtracted from the budget available to decode, persist and write the response.
//
// That makes ReadTimeout == WriteTimeout wrong too, not just ReadTimeout > WriteTimeout. Equal
// budgets let a slow body consume essentially the whole write deadline and leave nothing for
// image.Decode, the insert and the response — the same dropped-connection failure as the
// too-long read deadline, reached a different way.
//
// The read budget is therefore held strictly BELOW the write budget, with the difference
// reserved as handler headroom:
//
//	DefaultReadTimeout + UploadHandlerHeadroom <= DefaultWriteTimeout
//
// Sizing: a 42 MiB body is ~34s at 10 Mbps and ~67s at 5 Mbps, so a read budget of 90s covers a
// genuinely slow but real uploader. WriteTimeout is raised to 120s to hold that plus headroom —
// squeezing the read budget instead would reject legitimate uploads from ordinary connections,
// which is a worse failure than a longer deadline on a route already bounded by admission
// control and MaxBodyBytes.
//
// TestServerTimeoutsReserveHandlerHeadroom asserts the inequality on the real server, so changing
// any one of the three without the others fails rather than silently starving the response.
const DefaultReadTimeout = 90 * time.Second

// UploadHandlerHeadroom is the response budget reserved inside WriteTimeout after the body has
// been read: decode, persist, and write. It is not itself a deadline — net/http has no such
// knob — but naming it makes the relationship between the read and write deadlines explicit and
// testable rather than an unwritten assumption about two constants that happen to differ.
//
// 30s is generous for a decode-and-insert that normally completes in well under a second; it is
// sized to survive a slow database rather than to be tight.
const UploadHandlerHeadroom = 30 * time.Second
