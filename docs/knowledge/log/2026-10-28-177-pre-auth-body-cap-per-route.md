# 2026-10-28 — Non-upload routes get a 1 MiB pre-auth body cap (#177)

**Fix** — The inbound body cap was one global value, `constants.MaxRequestBodyBytes` (42 MiB),
sized for the largest legal creative-asset upload. Goa decodes a body before the endpoint
authenticates the caller, and only the upload route is gated by `UploadAdmission`, so every other
route let an unauthenticated caller make the decoder allocate up to 42 MiB per request (more once
`Any` attributes are boxed into interface values). `middleware.MaxBodyBytesPerRoute` now chooses
the cap per request, before the mux: the upload keeps 42 MiB, and every other route is held to
`constants.MaxNonUploadRequestBodyBytes` (1 MiB). The largest declared string on a non-upload
route is 8000 characters, so legitimate traffic is unaffected. The cap is per request; aggregate
pre-auth concurrency remains a gateway concern (the `briefs/**` route forwards bodies with no
size cap at Heimdall), which this service cannot own.

`TestNonUploadRoutes_RejectOversizeBodyBeforeDecoding` reads every body-bearing operation from the
served `openapi3.json` and sends each a well-formed body one byte over the cap, both with a
declared length and chunked. Against the old chain all 104 cases were decoded and answered 400 or
503, never 413. See [internal/middleware](../code/internal-middleware.md) and
[pkg/constants](../code/pkg-constants.md).
