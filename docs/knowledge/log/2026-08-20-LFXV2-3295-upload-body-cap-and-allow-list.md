# 2026-08-20 — LFXV2-3295 the declared size limit never bounded the wire

**Fix** — the creative-asset upload endpoint declares `MaxLength(31457280)` on its Goa
`Bytes` attribute, which reads like a 30-MiB upload limit and is not one. The generated
validator tests `len()` on the DECODED slice, and it only receives that slice after
`goahttp.RequestDecoder`'s `json.Decoder` has read the entire request body off the socket
and base64-decoded it. The declared limit therefore reports on a buffer the server has
already paid for. Sweeping `cmd/` and `internal/` found no `http.MaxBytesReader` anywhere —
every `LimitReader` in the tree caps an OUTBOUND response — so an unauthenticated caller
could stream a body of any size into that decoder.

The bound is now inbound: `middleware.MaxBodyBytes`, applied on every route at
`constants.MaxRequestBodyBytes`. Two arms, because a body exceeds a cap in two ways. A
`Content-Length` past the cap is refused without reading a byte. An absent, understated or
chunked length can only be discovered by reading, so `http.MaxBytesReader` wraps the body
and the read fails one byte past the cap. The second arm is the one that matters against an
attacker: `Content-Length` is caller-supplied, and omitting it is free.

The size was DERIVED, and the obvious answer is wrong. Base64 expands by exactly 4/3, so a
maximum-legal 30-MiB image is 41,943,040 characters of base64 — 40 MiB to the byte — before
the JSON envelope is added. Only `content_type` and `bytes` are in the body — `project_id` and
`brief_id` are path parameters — so the envelope adds 39 bytes and the measured worst-case body
is 41,943,079. A 40-MiB cap, which is what "base64 is 4/3" suggests, would reject every
maximum-size upload by those 39 bytes.
The cap is 42 MiB. `TestUploadRoute_AdmitsMaximumLegalUpload` builds that full body and
fails with the arithmetic printed if the constant ever drops below it, so the derivation is
checked by the suite rather than trusted to this note.

Both arms answer `413` in the service's `{code, message}` shape rather than letting the
overflow surface as a decode error. Unbounded input reaching the Goa decoder produces a
`400` about malformed JSON, which tells an operator the client sent bad input when the
server in fact refused to read it.

**The allow-list was a comment, not a contract.** `mimeForImageFormat` is named in its own
source as the authoritative PNG/JPEG allow-list, with the blank decoder imports described as
merely the UPPER bound — the stated reason another package importing `image/gif` cannot
widen the endpoint. Mutating it to `return "image/" + format, true` SURVIVED the entire
suite: every existing case fed PNG or JPEG, so nothing ever reached the `default` arm. The
guard was unreachable only because no GIF decoder happened to be registered in the binary,
which is a property of the whole program's import graph and not of this package. One
`_ "image/gif"` in any transitively-imported package would have silently widened the
endpoint to store GIFs under a `mime_type` that no CHECK constraint, no OpenAPI enum and no
Meta creative path expects. Importing `image/gif` in the TEST package only reproduces that
condition exactly — the decoder is registered for the test binary, the guard becomes
reachable, and a real GIF must still be refused with the allow-list's own distinct message.

The lesson generalises past this endpoint: a comment asserting that some OTHER layer is the
real boundary is a claim about unreachable code, and unreachable code is untested code. The
mutation that proves it is not a nit — it is the only thing standing between the claim and
an unrelated import falsifying it.

**Verification** — every mutation compiled, and each was reverted:

- Deleting `MaxBodyBytes` from the server chain fails
  `TestUploadRoute_RejectsOversizeBodyBeforeDecoding` with `status = 400, want 413` — the
  exact misleading decode error the fix removes.
- Narrowing the cap to 40 MiB fails `TestUploadRoute_AdmitsMaximumLegalUpload`, printing
  `MaxRequestBodyBytes (41943040) is below the largest LEGAL upload (41943079 bytes)`.
- Dropping the `MaxBytesReader` arm (keeping only `Content-Length`) fails
  `TestMaxBodyBytes_RejectsUndeclaredOversizeMidRead`; the route-level test stays GREEN,
  which is why the middleware-level test exists.
- Dropping the `Content-Length` arm, answering `400` instead of `413`, and an off-by-one
  that refuses an at-the-limit body each fail their own case.
- Widening `mimeForImageFormat` fails both the handler test and the direct allow-list test.
- The three pre-existing guards (declared-vs-sniffed mismatch, `DecodeConfig` failure, the
  unwired-repo `503`) were re-verified after these changes and still bind.

**A test that agreed with itself.** The route-level chunked-body test — the only coverage
of the arm that matters against an attacker — was VACUOUS as first written, and two
independent reviewers caught it. Its fixture streamed a run of `'a'`, which is invalid JSON
at byte 1, so `json.Decoder` aborted after ~512 bytes and answered 400 whether or not the
cap was installed. Measured both ways: 512 bytes read, status 400, identical output. The
fixture's own error absorbed the mutation, and the test's only surviving assertion ("not
2xx") was satisfied by the decoder unaided.

Making it bind exposed a REAL defect the vacuous version had hidden. `MaxBytesReader`
reports the overflow to whoever performs the read — the Goa decoder, behind the mux — and
that decoder cannot tell a truncated body from a malformed one, so it rendered the cap's
refusal as a generic 400 about invalid JSON. The chunked arm was therefore never producing
the clean 413 the design claimed. The middleware now tracks whether a read tripped the limit
(`*http.MaxBytesError` via `errors.As`) and buffers the handler's response so it can replace
that misleading 400 with the correct 413. The lesson is the general one: a fixture that
fails for its OWN reason proves nothing, and the bug it hides can be in the very control the
test was written to defend.

**Review follow-ups — three gaps the pre-merge review found, all confirmed.**

`image.DecodeConfig` does not prove an image is decodable. It stops once it has the format and
dimensions, so a PNG truncated immediately after its IHDR chunk passes it — measured: 33 bytes,
`DecodeConfig` returns `png` and `64x64` with a nil error, while `png.Decode` on the same bytes
returns `unexpected EOF`. Such an upload was ACCEPTED AND PERSISTED and would fail far later at
dispatch. The naive remedy — decode everything — is a decompression-bomb vector, since PNG and
JPEG both compress a flat image enormously and a body well inside the 42-MiB request cap can
declare dimensions decoding to gigabytes (30000x30000 is a few hundred KB on the wire and 3.3
GiB decoded). So validation is now explicitly staged: header read, then a dimension gate using
the values the header already gave for free, and only then the allocating full decode. The
limits (20M pixels, 10,000 per side) are ~2.4x a 4K UHD creative — the largest anyone plausibly
uploads, and itself far beyond Meta's 1936x1936 recommendation — and cap a decoded RGBA buffer
near 76 MiB. The per-side bound is not redundant with the area bound: a 1x20,000,000 strip fits
the pixel budget and is not an image any creative pipeline should accept.

The 413 was undeclared in the contract. The status is emitted by middleware outside the mux, so
it never passes through a Goa encoder — which is exactly why it was easy to ship undeclared, and
why every other test on the change passed without it. The generated client had no decode case
and would have reported a routine oversized upload as `ErrInvalidResponse`, and the OpenAPI
documents omitted the behaviour entirely. `PayloadTooLarge` is now declared on the brief and
connection error helpers, so all 17 body-bearing methods carry it; it is on the bodyless methods
too, because the cap is global and any route can answer it.

**Verification of the follow-ups** — every mutation compiled and was reverted:

- Removing the full decode: the truncated-PNG test fails with `err = <nil>`, i.e. the corrupt
  asset was accepted, which is precisely the defect.
- Removing the dimension gate, AND separately moving it to run after the decode: both fail with
  `message = "the uploaded image data is incomplete or corrupt", want the dimension refusal` —
  the bomb reached the decoder. The two arms are distinguishable by message, which is what makes
  "rejected without decoding" an assertable property rather than a hopeful comment.
- Dropping the per-side bound fails the degenerate-strip case; dropping the non-positive guard
  fails the zero/negative cases.
- Dropping the `Response("PayloadTooLarge", …)` mapping fails the contract test, printing the
  documented response set without 413.

The truncated-PNG fixture asserts its own premises — that it clears stage 1 and stage 2 — so it
cannot pass by being rejected for an unrelated reason. That check exists because of the vacuous
fixture recorded above; the same trap was the first thing to look for.

**A survivor worth recording.** Deleting either `SetCreativeAssetRepo` call from the
container COMPILED and left the whole suite green, while in production every upload would
answer `503` forever on that pod with the rest of the brief routes live. `briefBackendSetter`
forces the METHOD to exist, never the call to be made, and the handler's `503` is
deliberately indistinguishable from the no-database mode's — so no black-box assertion can
tell a mis-wired live container from a correctly wired one. Both startup paths now bind
through one `bindBriefLiveBackends` helper, making the coupling structural rather than
remembered, and a test pins that the helper leaves the repo bound. A caller that BYPASSES
the helper altogether still survives; that is a real residual limit, not a closed hole.
