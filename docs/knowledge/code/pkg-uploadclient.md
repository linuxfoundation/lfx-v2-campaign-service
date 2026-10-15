---
type: "Go Package"
title: "pkg/uploadclient"
description: "Request encoder for the generated Goa clients that declares the encoded Content-Length, so creative-asset uploads are priced by size instead of serialized as chunked (#183)."
resource: "pkg/uploadclient"
---

# pkg/uploadclient

Request encoder for the generated Goa clients that declares the encoded Content-Length, so creative-asset uploads are priced by size instead of serialized as chunked (#183).

## Why it exists (issue #183)

goa's stock `goahttp.RequestEncoder` installs the JSON body but never sets
`req.ContentLength`. `BuildUploadCreativeAssetRequest` builds the request with a nil body, so the
length stays 0, and `net/http` sends a non-nil body with length 0 as **chunked**. The server then
sees `ContentLength == -1`, and `constants.UploadAdmissionWeightFor` charges an unknown length the
worst-case ceiling — the whole admission budget — so every upload from a stock-encoder client ran
one at a time, however small.

The server rule is deliberate and unchanged (see "Concurrent upload admission" in
[internal/middleware](internal-middleware.md)): the permit is taken before the body is read, so an
undeclared body must be priced as worst-case. The agreed fix (#170 review, recorded on #183) is on
the client: declare the length.

## Contract

`RequestEncoder(r)` is a drop-in for `goahttp.RequestEncoder`, passed as the `enc` argument of a
generated `NewClient`:

- same `Content-Type` default (`application/json`, a caller's value is kept);
- same JSON bytes (`encoding/json`, as goa uses — pinned byte-for-byte by
  `TestRequestEncoder_IsADropInThatDeclaresItsLength`);
- **plus** `ContentLength` set to the encoded length and a replayable `GetBody`;
- a marshal error leaves the request untouched.

It is safe on every endpoint; it is needed on the upload. Nothing in this repo constructs the
generated client outside tests, so adopting it is the Go caller's choice; a caller that keeps the
stock encoder (or a non-Go client that streams chunked) is still served, one upload at a time.

## End-to-end proof

`cmd/campaign-service/upload_client_length_test.go` drives the REAL generated briefs client over a
real TCP connection against the REAL `buildHandler` chain (production admission constants), with
only the mux replaced by a parking handler:

- `TestUploadClient_DeclaredLengthUploadsRunConcurrently` — with this encoder the server observes
  a positive `ContentLength` and two small uploads are inside the handler at the same time.
  Removing the `ContentLength` assignment makes it fail (the second upload is shed with `503`).
- `TestUploadClient_UndeclaredLengthIsStillChargedTheWholeBudget` — with goa's stock encoder the
  server observes `-1` and a second concurrent upload is shed, proving the server guard was not
  weakened.
