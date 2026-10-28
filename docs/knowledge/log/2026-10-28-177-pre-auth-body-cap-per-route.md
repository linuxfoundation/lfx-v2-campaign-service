# 2026-10-28 — Non-upload routes get a 2 MiB pre-auth body cap (#177)

**Fix** — The inbound body cap was one global value, `constants.MaxRequestBodyBytes` (42 MiB),
sized for the largest legal creative-asset upload. Goa decodes a body before the endpoint
authenticates the caller, and only the upload route is gated by `UploadAdmission`, so every other
route let an unauthenticated caller make the decoder allocate up to 42 MiB per request (more once
`Any` attributes are boxed into interface values). `middleware.MaxBodyBytesPerRoute` now chooses
the cap per request, before the mux: the upload keeps 42 MiB, and every other route is held to
`constants.MaxNonUploadRequestBodyBytes` (2 MiB). It is sized from the largest legitimate body,
the email wizard's `update-sections` (120 blocks, a 200,000-rune assembled body, ~1.2 MB
JSON-escaped at worst), which a 1 MiB cap would have refused; a test pins that body under the cap. The cap is per request; aggregate
pre-auth concurrency remains a gateway concern (the `briefs/**` route forwards bodies with no
size cap at Heimdall), which this service cannot own.

`TestNonUploadRoutes_RejectOversizeBodyBeforeDecoding` reads every body-bearing operation from the
served `openapi3.json` and sends each a well-formed body one byte over the cap, both with a
declared length and chunked. Against the old chain all 104 cases were decoded and answered 400 or
503, never 413. See [internal/middleware](../code/internal-middleware.md) and
[pkg/constants](../code/pkg-constants.md).
