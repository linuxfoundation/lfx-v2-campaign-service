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
// creative-asset upload declares MaxLength(41943040) on a base64 `String` attribute --
// the ENCODED ceiling, base64.EncodedLen of the 30 MiB stored-file limit -- but the
// generated validator counts CHARACTERS of that string, and it only ever sees the string
// after goahttp.RequestDecoder's json.Decoder has read the entire request body. Without an
// inbound cap an unauthenticated caller streams an arbitrarily large body and the server
// buffers all of it before any declared limit is consulted. (The base64 DECODE happens later
// still, in the service method after authentication.)
//
// The value is derived, not guessed. Base64 expands by exactly 4/3 with padding
// (RFC 4648 standard alphabet, the encoding the attribute documents), so the largest
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
// wrong in a way the code contradicts directly: the ~30 MiB decoded slice is the decode's own
// input and cannot be collected while it runs, and the 80 MiB pixel buffer is allocated on top
// of it.
//
// The tally, and what keeps it at three allocations rather than four. The wire attribute is a
// base64 STRING (design/brief.go), so the request now carries a ~40 MiB string that the older
// Goa Bytes attribute never materialised separately — goa decoded during JSON decoding. Left
// reachable it would coexist with the pixel buffer and push the peak to ~192 MiB, past this
// figure. UploadCreativeAsset therefore clears p.Bytes the moment the decode returns, which is
// the only point it stops being needed, so what provably coexists is:
//
//	~42 MiB  the Goa decoder's body buffer (Go frees on GC, not on last use)
//	~30 MiB  the decoded slice, live for the whole decode — it is the decode's own input
//	~80 MiB  the pixel buffer, allocated on top of the decoded slice
//	 = ~128 MiB
//
// That clearing is load-bearing, not tidiness: the payload struct keeps p reachable for the
// whole handler (it is read again for ProjectID/BriefID), so without it the string outlives the
// decode. TestUploadAdmissionWeightCoversTheCoexistingPeak pins this figure against the
// per-image ceilings, and the service test pins the release.
//
// Charging less than the real peak lets two permits admit more than the budget — a bound that
// under-counts precisely when it matters, which is worse than no bound because it reads as
// protection.
//
// It is the weight of the WORST LEGAL upload, and it is charged only to an upload that is
// actually that large. Charging it flat to every upload was a real defect rather than a
// conservative choice: budget/weight is then exactly 1, so the service admitted ONE upload at a
// time no matter how small, and a 200 KiB logo held the same permit as a 30 MiB image. See
// UploadAdmissionWeightFor, which prices a request from its declared size against this ceiling.
const UploadAdmissionWeightBytes int64 = 128 << 20 // 128 MiB

// UploadAdmissionAmplification is how many bytes of peak resident memory one byte of request
// body may cause this route to allocate.
//
// It is the same arithmetic that produced UploadAdmissionWeightBytes, expressed as a ratio so it
// can price a request smaller than the worst case. The worst legal upload is 42 MiB on the wire
// (MaxRequestBodyBytes) and provably peaks at ~128 MiB resident — the ~30 MiB decoded slice
// live for the whole decode, the pixel buffer allocated on top of it, and the body buffer not
// yet collected. 128/42 is just over 3, so 4 is the next whole ratio above the measured worst
// case: it keeps a small upload's charge proportional while never pricing ANY request below
// what the worst case proves it can cost.
//
// The ratio survives the base64-string wire attribute only because UploadCreativeAsset clears
// p.Bytes once decoded (see UploadAdmissionWeightBytes). Were that string left reachable across
// the pixel decode the peak would be ~192 MiB and this ratio would have to be 5.
//
// Deliberately a whole number, and deliberately an over-estimate. This multiplies a
// CALLER-SUPPLIED length, so every source of error in it must round against the caller.
const UploadAdmissionAmplification int64 = 4

// UploadAdmissionMinWeightBytes floors what any single upload is charged.
//
// Without a floor, size-proportional pricing degenerates: a request DECLARING Content-Length 0
// (or a few bytes) would be charged nothing and the budget would admit unboundedly many of them.
// Each still costs the process a goroutine, a connection and a decoder, so the floor is what
// keeps "many tiny uploads" bounded rather than free.
//
// It applies only to DECLARED lengths. A chunked request declaring nothing at all is NOT floored
// — UploadAdmissionWeightFor charges it the worst-case ceiling, because an undeclared body's
// cost is unknown until it has been read and the permit is taken before the read. See that
// function for why the floor was tried for the undeclared case and reverted.
//
// 8 MiB admits 16 concurrent small DECLARED uploads against the 128 MiB budget — enough that
// ordinary traffic never sheds, few enough that the aggregate stays a minority of the pod.
const UploadAdmissionMinWeightBytes int64 = 8 << 20 // 8 MiB

// UploadAdmissionWeightFor prices ONE upload against the budget from its DECLARED body size.
//
// Why declared size is safe to trust here, when Content-Length is caller-supplied and a caller
// may lie: the two directions of the lie are not symmetric, and neither one can under-charge.
//
//   - Over-declaring charges the caller MORE than it will spend. That is self-limiting — it
//     exhausts the liar's own admission first — and it is refused outright above
//     MaxRequestBodyBytes by MaxBodyBytes' Content-Length arm.
//   - UNDER-declaring is the dangerous direction, and it is closed downstream rather than here.
//     MaxBodyBytes wraps the body in http.MaxBytesReader, so a request that understates its
//     length cannot then read more than MaxRequestBodyBytes; it is refused with 413 at the read.
//     A liar therefore buys a cheap permit and still cannot spend more than the ceiling this
//     function would have charged it.
//
// Absent or chunked (ContentLength < 0) is charged the WORST-CASE CEILING. Because that ceiling
// equals UploadAdmissionBudgetBytes, one undeclared request takes the entire budget and
// undeclared uploads run strictly ONE AT A TIME.
//
// That cost is real and was accepted deliberately, so do not "fix" it by lowering the charge
// without re-reading this block. goa's generated RequestEncoder never sets ContentLength, so the
// shipped briefs client streams this upload chunked and lands here: serialization is the DEFAULT
// path for the only generated client that exists, not a rare branch.
//
// The floor was tried here and reverted. Charging the floor (8 MiB) admits 16 concurrent
// undeclared uploads, and the aggregate that produces is not survivable:
//
//   - internal/service's UploadCreativeAsset holds the base64-DECODED slice (the output of
//     decodeCreativeBytes, up to ~31.5 MiB) live in asset.Bytes through sha256Hex AND the whole
//     CreateAsset insert. So 16
//     admitted uploads coexist holding ~480 MiB of decoded slices alone, on a 512 MiB pod,
//     before any wire buffer.
//   - Neither other control bounds that aggregate. MaxBodyBytes caps ONE body, not the sum.
//     DecodeAdmissionBudgetBytes reserves only the PIXEL buffer and releases the instant
//     image.Decode returns — it never covers p.Bytes.
//   - What is reachable PRE-AUTH is the buffered body plus the ~40 MiB base64 STRING goa
//     materialises, because the generated handler decodes the request before the endpoint
//     calls authJWTFn. The base64 DECODE is NOT pre-auth: decodeCreativeBytes runs inside
//     UploadCreativeAsset, which the generated endpoint reaches only after authJWTFn returns
//     (gen/.../endpoints.go). The aggregate above is therefore what 16 AUTHENTICATED uploads
//     hold, and it is still the reason the floor was reverted — the decoded slices coexist on
//     one pod whether or not their callers authenticated, and the permit is taken before the
//     read, which is the only point any of it can be bounded.
//
// Both positions were defensible, which is why this comment records the trade rather than just
// the outcome: the floor bought concurrency for the real client and cost the memory bound; the
// ceiling keeps the memory bound and costs concurrency. The memory bound wins because it is the
// property this middleware exists to provide, and because failing safe (a retryable 503 under
// concurrent uploads) beats an OOM that takes the pod down for every route.
//
// The server cannot price what it has not read, so the honest fix is upstream: make the
// generated client DECLARE its length, and undeclared bodies become the rare case rather than
// the default. That is issue #183, and it is what restores concurrency for ordinary uploads.
// Uploads are low-frequency, so serialized undeclared uploads are a throughput cost in the
// meantime, not a correctness one.
func UploadAdmissionWeightFor(contentLength int64) int64 {
	if contentLength < 0 {
		// Unknown length is charged the WORST CASE, which equals the whole budget, so
		// undeclared uploads run strictly one at a time. See the doc comment: the server
		// cannot know what a chunked body will spend until it has read it, and the permit is
		// the only bound that runs BEFORE the read.
		return UploadAdmissionWeightBytes
	}
	// Overflow-safe: contentLength is bounded by MaxRequestBodyBytes downstream, but this
	// function must not depend on a bound applied by a different middleware, so the compare
	// happens before the multiply rather than after it.
	if contentLength > UploadAdmissionWeightBytes/UploadAdmissionAmplification {
		return UploadAdmissionWeightBytes
	}
	w := contentLength * UploadAdmissionAmplification
	if w < UploadAdmissionMinWeightBytes {
		return UploadAdmissionMinWeightBytes
	}
	return w
}

// DecodeAdmissionBudgetBytes is the total PIXEL-BUFFER memory concurrent creative-asset decodes
// may allocate at once.
//
// It is a second, independent bound, and it exists because UploadAdmissionBudgetBytes cannot
// cover this. That budget is spent against a weight priced from Content-Length, which bounds
// what arrives on the socket; image compression severs the link between wire bytes and decoded
// bytes. A flat 4000x4000 PNG is ~68 KiB on the wire and 61 MiB decoded — over 900x — and it is
// legal, admitted deliberately by the dimension gate. Wire-priced admission charges it the
// floor, so without this bound enough of them decode concurrently to exhaust the pod while the
// upload budget still reads as unspent.
//
// Sized like the upload budget and for the same reason: a quarter of the pod, because the
// runtime, the pool and every non-upload request share the limit. The two budgets are additive
// in the worst case — a request can hold an upload permit and a decode reservation at once — so
// together they cap uploads at half the pod, which is the same ceiling
// TestUploadAdmissionBudgetLeavesHeadroom asserts for uploads alone.
//
// At maxCreativeDecodedBytes (80 MiB) per worst-case image this admits one of them at a time and
// many ordinary ones, which is the same shape as the upload bound: priced for the worst legal
// input, shared by everything smaller.
const DecodeAdmissionBudgetBytes int64 = PodMemoryLimitBytes / 4 // 128 MiB

// DecodeAdmissionWait is how long a request will wait for DECODE budget before it is shed
// with 503.
//
// It exists because the wait cannot be left to the caller's context. The upload handler passes
// the HTTP request context, and net/http gives a handler's r.Context() no deadline at all:
// http.Server's ReadTimeout and WriteTimeout install deadlines on the SOCKET and never cancel
// that context. An acquisition governed only by it therefore blocks until the client hangs up —
// and because the request is still holding its outer UploadAdmission permit while it waits, an
// unbounded wait here converts a memory bound into permit and goroutine exhaustion. That is a
// hang, not a bound.
//
// Matched to UploadAdmissionWait, and for the same reason: long enough to absorb ordinary
// jitter between two uploads decoding at once, far short of the write deadline so a shed
// request is answered honestly rather than killed with no response. The two admission stages
// are the same control at different points in the request, so they shed on the same clock.
const DecodeAdmissionWait = 250 * time.Millisecond

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
