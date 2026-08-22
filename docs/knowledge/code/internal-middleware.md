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
the entire body off the socket and base64-decoded it. Before this middleware there was no
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
30-MiB `MaxLength` on the upload's `bytes` attribute arrives as 41,943,040 characters — 40 MiB to
the byte. Only `content_type` and `bytes` travel in the body (`project_id` and `brief_id` are path
parameters), so the JSON envelope adds 39 bytes for the shorter enum value (`"image/png"`) and 40
for the longer (`"image/jpeg"`), putting the worst legal body at **41,943,080** — the JPEG case,
since `content_type` rides in the body and its length therefore counts. A 40-MiB cap would
reject every maximum-size image by those 40 bytes; 42 MiB clears it with ~2 MiB of headroom.
Raising the declared `MaxLength` requires raising this constant in step.

Both enum values are driven by `TestUploadRoute_AdmitsMaximumLegalUpload`, and the JPEG case is
what makes the last byte load-bearing: a cap of 41,943,079 passes a PNG-only fixture while
refusing every maximum-size JPEG the contract admits.

In the handler chain the cap sits inside the request-ID/debug/OTel wrappers (so a 413 still
carries a request id and is still traced) but outside the mux, since the Goa decoders behind the
mux are what would otherwise do the buffering.

See [internal/middleware](../../../internal/middleware).
