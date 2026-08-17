# 2026-08-17 — LFXV2-2665 creative-upload guard coverage

**Fix** — The three validation guards on `UploadCreativeAsset` had no binding test. Deleting any
one of them — the `image.DecodeConfig` sniff, the format allow-list, or the declared-vs-sniffed
mismatch check — left the entire suite green. Verified by removing the mismatch guard and running
`go test ./internal/...`: zero failures.

That matters more than an ordinary coverage gap because these three ARE the endpoint's security
boundary. Between them they stop arbitrary bytes being stored as a declared image, stop a format
some other package's blank import happens to register from being accepted, and stop `mime_type`
becoming a lie about the content it labels.

The tests assert BOTH halves: the request is refused AND nothing reached storage. Asserting only
the error would pass against a handler that rejected the request after persisting the bytes, which
is the failure mode worth pinning — the rejection is visible, the stored row is not.

Two further cases cover what the handler DERIVES rather than accepts: that the stored `mime_type`
is the sniffed value (not the declared one) and that `checksum` is the SHA-256 of the bytes. The
checksum is the idempotency key the whole feature rests on — `UNIQUE (brief_id, checksum)` in
migration 000026 — and nothing at this layer pinned it.

**Note on the deferral.** The existing test comment defers this matrix to phase C7, and C7 is real:
`specs/004-meta-single-image-creative/plan.md:184` lists it, and the plan runs C1–C8. These three
are pulled forward anyway. A guard whose deletion breaks no test is indistinguishable from a guard
that was never written, and for a security boundary that gap should not wait on a later phase.

**Docs** — `design/brief.go`'s endpoint Description advertised validation of "size **and dimension**
limits". No dimension check exists, and the handler comment says so explicitly: Meta's creative
policy (minimum dimensions, aspect ratio) is deliberately left to dispatch, where Meta's API is the
authority. The Description ships into `openapi.yaml` as the published contract, so an integrator
would read it as a promise that an undersized image is rejected at upload. The size limit IS
enforced (`MaxLength` in the design); the dimension limit was never there. Regenerated via
`make apigen`, which propagated to the four kodata copies.
