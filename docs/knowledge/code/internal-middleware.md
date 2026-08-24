---
type: "Go Package"
title: "internal/middleware"
description: "Package middleware provides HTTP middleware for the service."
resource: "internal/middleware"
---

# internal/middleware

Package middleware provides HTTP middleware for the service.

## Inbound request body cap (LFXV2-3295)

`MaxBodyBytes(limit)` bounds how much of a request body the server will read, and
`cmd/campaign-service` applies it on every route at `constants.MaxRequestBodyBytes` (42 MiB).

It exists because the size bounds declared in `design/` do not bound the wire. A Goa `Bytes`
attribute's `MaxLength` is checked by the GENERATED VALIDATOR against the already-decoded slice,
and the validator only sees that slice after `goahttp.RequestDecoder`'s `json.Decoder` has read
the entire body off the socket and base64-decoded it. (The upload declares that ceiling in base64
CHARACTERS — `MaxLength(41943040)` — because that is the unit the published schema measures; the
30 MiB DECODED ceiling is enforced separately in the handler at `maxCreativeStoredBytes`.) Before this middleware there was no
`http.MaxBytesReader` anywhere in the service — every `LimitReader` in the tree caps an OUTBOUND
response — so an unauthenticated caller could stream an arbitrarily large body to the
creative-asset upload and the server would buffer and decode all of it before any declared limit
was consulted.

Two arms, because a body can exceed the cap in two ways. A `Content-Length` past the cap is
refused up front without reading a byte. A body whose length is absent, understated, or chunked
can only be discovered by reading, so `http.MaxBytesReader` wraps it and the read fails one byte
past the cap. The second arm is what makes this more than a `Content-Length` check: that header is
caller-supplied and a chunked request carries none.

Both arms answer `413` in the service's `{code, message}` JSON shape rather than letting the
overflow surface as a decode failure — an unbounded body reaching the Goa decoder produces a
`400` about malformed JSON, which tells an operator the client sent bad input when the server in
fact refused to read it. Nothing about the body is logged or echoed: the message is fixed, and on
the `Content-Length` arm the bytes are never read at all.

The cap is sized from the largest LEGAL upload, not guessed. Base64 expands by exactly 4/3, so the
30-MiB stored-file ceiling arrives as 41,943,040 characters — 40 MiB to the byte, which is exactly
what the upload's `bytes` attribute declares as `MaxLength`. Only `content_type` and `bytes` travel in the body (`project_id` and `brief_id` are path
parameters), so the JSON envelope adds 39 bytes for the shorter enum value (`"image/png"`) and 40
for the longer (`"image/jpeg"`), putting the worst legal body at **41,943,080** — the JPEG case,
since `content_type` rides in the body and its length therefore counts. A 40-MiB cap would
reject every maximum-size image by those 40 bytes; 42 MiB clears it with ~2 MiB of headroom.
Raising the declared `MaxLength` (or the `maxCreativeStoredBytes` ceiling it encodes) requires
raising this constant in step.

Both enum values are driven by `TestUploadRoute_AdmitsMaximumLegalUpload`, and the JPEG case is
what makes the last byte load-bearing: a cap of 41,943,079 passes a PNG-only fixture while
refusing every maximum-size JPEG the contract admits.

In the handler chain the cap sits inside the request-ID/debug/OTel wrappers (so a 413 still
carries a request id and is still traced) but outside the mux, since the Goa decoders behind the
mux are what would otherwise do the buffering.

## Upload admission bound (LFXV2-3295)

`UploadAdmission(budgetBytes, maxBodyBytes, weightFor, wait)` bounds the TOTAL bytes concurrent
uploads may cause the process to allocate. The body cap above bounds ONE request; this bounds how
many run at once.

The distinction is the point. Against a fixed pod memory limit, N concurrent and entirely LEGAL
uploads multiply a per-request allocation until the pod is OOM-killed — a cheap denial of service
that no tightening of a per-request number can fix, because the quantity being bounded is
different in kind.

Placement is the security property, and it is why this is NOT a semaphore inside
`UploadCreativeAsset`. Goa's generated handler calls `decodeRequest(r)` and only then
`endpoint(...)`, and the generated endpoint calls `authJWTFn` as its first statement — so the
body read and the base64 decode both complete BEFORE any JWT is examined. A guard in the service
method would acquire after ~72 MiB had already been allocated for an unauthenticated caller,
bounding only the decode tail while reading as though the problem were solved. Admission
therefore sits immediately outside `MaxBodyBytes` and outside the mux, taking its permit before
the decoder touches the body.

Scope, stated honestly: this bounds the pod's memory. It does not authenticate, and it does not
prevent an unauthenticated body from being read — auth-before-body-read belongs at the gateway.
It ensures such a read cannot happen without first taking a permit from a budget tied to the
pod's real limit.

What the weight accounts, precisely: the WIRE side of the inbound request — the buffered body
and the base64-decoded slice for one upload. It does NOT account the decoded PIXEL buffer, and
the two must not be treated as one bound. The weight is priced from `Content-Length`, and image
compression severs the link between wire bytes and decoded bytes: a flat 4000x4000 PNG is ~68 KiB
on the wire and 61 MiB decoded, so it takes the minimum wire permit while allocating a pixel
buffer that permit never paid for. Decoded pixels are admitted separately, against their own
budget, by `service.DecodeReserver` (`DecodeAdmissionBudgetBytes`) — see
`docs/knowledge/code/internal-service.md`. Reading this middleware as covering pixels would make
that second reservation look redundant and invite its removal.

It also does NOT account memory an unrelated path later allocates from bytes stored
earlier. Outbound dispatch reads assets back out of Postgres, but that runs in a dispatch
worker long after the HTTP request returned and released its permit — a different lifetime and code path, not an undercount of this one. Dispatch-side
memory is bounded by its own control on that path: the Meta dispatcher resolves each distinct
asset once per dispatch and caps the total distinct bytes one dispatch may hold, so repeating a
single asset across variants does not multiply what is resident.

The budget is DERIVED, not chosen: `constants.UploadAdmissionBudgetBytes` is
`PodMemoryLimitBytes / 4`, and `PodMemoryLimitBytes` mirrors the chart's
`resources.limits.memory`. Because that number lives in a file the Go build cannot read,
`TestPodMemoryLimitMatchesChart` parses `values.yaml` and fails when the two drift — the
relationship is enforced rather than trusted.

Two refusals, and their ORDER is deliberate. A body whose DECLARED `Content-Length` already
exceeds `maxBodyBytes` is answered `413` immediately, before any permit is sought: that verdict is
permanent and knowable from the headers, so shedding it with a retryable `503` because the budget
happened to be busy would tell the caller to retry something that can never succeed at any size of
budget. The check reads an already-parsed header and consumes no body, so it does not weaken the
pre-auth placement above. An UNDECLARED or chunked body is deliberately not covered here — its
size is unknowable without reading it, so it proceeds through admission and meets `MaxBodyBytes`'
`MaxBytesReader` arm, which is where that case belongs.

The weight is PRICED per request, not charged flat, by `constants.UploadAdmissionWeightFor`. A
flat charge equal to the budget made effective concurrency exactly 1 — a 200 KiB logo held the
same permit as a 30 MiB image. Declared sizes are charged proportionally
(`UploadAdmissionAmplification`) between a floor (`UploadAdmissionMinWeightBytes`, 8 MiB) and the
worst-case ceiling, so ordinary uploads run ~16 at a time against the 128 MiB budget.

Unknown length (chunked or absent, `ContentLength < 0`) is charged the FLOOR, not the ceiling.
The ceiling equals the whole budget, so pricing the unknown case there re-created concurrency 1 —
and not hypothetically: goa's generated `RequestEncoder` never sets `ContentLength`, so the
shipped briefs client streams this upload chunked and hit exactly that path. Charging the floor
does not make an omitted header the cheapest permit, because the permit is not what bounds
spending: the body is bounded by `MaxBodyBytes`' `MaxBytesReader` and the pixel buffer by the
separate `DecodeReserver` budget, and neither consults the declared length. Making the generated
client declare an encoded length so it is priced by its true size is the complementary
client-side fix, tracked as issue #183.

Under saturation a request waits a bounded `UploadAdmissionWait` (250 ms, absorbing ordinary
jitter) and is then shed with an explicit `503` plus `Retry-After` in the service's
`{code, message}` shape. Never a `200`, never an empty body: a refusal that surfaced as success
would tell a caller its asset was stored when the body was never read. Non-upload routes are
never gated, so a memory control cannot become an availability regression.

`DefaultReadTimeout` accompanies it: `ReadHeaderTimeout` alone leaves a slowloris able to dribble
a body indefinitely while holding a permit, which would exhaust the budget with requests that
never complete.

## Response-writer capabilities

`deferredBufferWriter` wraps every request with a non-nil body, so any interface it fails to
forward is silently lost to every in-mux handler — a type assertion simply returns `ok == false`
with no error anywhere. It implements `Unwrap`, `Flush` and `Hijack` for that reason.

`Flush` is deliberately inert while the response is being held: flushing a buffered response
would put a body describing a truncated request on the wire and make it unrewritable, defeating
the deferred buffering. `Hijack` marks the response committed, since a hijacked connection can no
longer be rewritten to a 413.

See [internal/middleware](../../../internal/middleware).
