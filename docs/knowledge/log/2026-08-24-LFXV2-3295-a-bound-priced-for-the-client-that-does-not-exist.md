# 2026-08-24 — LFXV2-3295: a bound priced for the client that does not exist

**Fix** — Upload admission charged an undeclared-length request the worst-case
ceiling, and the ceiling equals the whole budget, so chunked uploads ran strictly
one at a time. That was not a dormant branch: goa's generated `RequestEncoder`
never sets `ContentLength`, so the only generated client that exists streams this
upload chunked and landed on exactly that path. The pricing model's entire benefit
was defeated for it, as an artifact of encoder behaviour rather than a decision.

Unknown length is now charged `UploadAdmissionMinWeightBytes`, the same floor as
the smallest declared upload, taking undeclared-length concurrency from 1 to 16
while the declared-length case stays at 16 (128 MiB ÷ 8 MiB).

The objection that kept the ceiling — that omitting `Content-Length` would become
the cheapest way to buy a permit — does not survive checking where the bound
actually lives. The permit is not what caps spending, and the two controls that do
never read the declared length:

- the BODY is bounded by `MaxBodyBytes`, which wraps every request in
  `http.MaxBytesReader` at `MaxRequestBodyBytes` (42 MiB). Verified on this route,
  not assumed: `buildHandler` applies it to the mux immediately INSIDE
  `UploadAdmission`, so it is on the upload chain.
- the PIXEL BUFFER — the larger cost, which compression severs from wire size —
  is bounded by the separate aggregate `DecodeReserver`, charged from the DECODED
  size read out of the image header.

So under-declaring buys a cheaper permit and still cannot spend more than a
declaring caller. The undeclared case was never a new hole; it was the already
closed under-declaring one reached by omitting the header instead of understating
it. Making the generated client publish an encoded length so it is priced by true
size is the complementary client-side half, tracked as issue #183.

The second fix is the one span in this path that was still unbounded. The
`UploadAdmission` permit is taken before `next.ServeHTTP` and released only after
the handler returns, so it spans `CreateAsset` — `BEGIN → SELECT ... FOR UPDATE on
the parent brief → INSERT → COMMIT`. Concurrent uploads to the SAME brief serialize
on that row lock, and `net/http` gives a handler's `r.Context()` no deadline
(`ReadTimeout`/`WriteTimeout` are SOCKET deadlines and never cancel it), with no
pool `statement_timeout`/`lock_timeout` and no `http.TimeoutHandler` on the chain.
A stalled or lock-blocked insert therefore pinned its permit until the client
disconnected, and enough of those exhaust the upload budget with no memory pressure
at all — the control that exists to stop uploads denying service becoming the thing
denying it.

The insert now runs under `context.WithTimeout(ctx, UploadHandlerHeadroom)`.
`UploadHandlerHeadroom` rather than a new constant because it already NAMES this
span: the response budget reserved inside `WriteTimeout` for "decode, persist, and
write". A pool-level `statement_timeout` would also bound it but is set per-pool
and would impose the upload's budget on every other query, so the bound is applied
where the span it protects is known. Expiry surfaces as an explicit retryable
`503`, never a success and never an empty result — the transaction was cut off, so
the asset may not be stored, and reporting otherwise is the worse failure.

Both are the same class as the `Acquire` unbounded wait fixed earlier here: a bound
that becomes a hang. Each is pinned by a test that fails before and passes after,
and mutation-verified with compiling reverts — restoring the ceiling kills two
tests, replacing `WithTimeout` with `WithCancel` hangs the handler, and dropping
the 503 mapping surfaces the timeout as a generic 500.
