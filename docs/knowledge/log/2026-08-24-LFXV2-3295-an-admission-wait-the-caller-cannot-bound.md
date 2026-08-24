# 2026-08-24 — LFXV2-3295: an admission wait the caller cannot bound

**Fix** — `DecodeReserver.reserve` acquired its pixel budget against the caller's context and
nothing else. The upload handler passes the HTTP request context, and net/http gives a handler's
`r.Context()` NO deadline: `http.Server`'s `ReadTimeout` and `WriteTimeout` install deadlines on
the SOCKET and never cancel that context. Verified rather than reasoned about — a probe server
built with all three timeouts set reported `r.Context().Deadline()` as `ok == false`.

So a full decode budget did not shed, it HUNG: the acquisition blocked until the client
disconnected, and the waiter was still holding its outer `UploadAdmission` permit the whole time.
A bound intended to cap memory became a way to exhaust permits and goroutines — strictly worse
than the condition it was added for, because a shed request returns and a hung one does not. The
wait is now bounded by `constants.DecodeAdmissionWait` (250ms, matching `UploadAdmissionWait`),
derived from the caller's context so a cancelled request still aborts early. Refusal returns
`false` and answers the retryable 503; it is never a success and never an empty result.

Two further findings from the same round, both real and both about the SHAPE of a bound rather
than its size:

- The reservation was released by `defer` at method return, so it spanned the checksum and the
  entire insert. The decoded image is discarded the moment `image.Decode` returns, so every byte
  was already free while the transaction ran — a slow database shed concurrent uploads for memory
  nobody held. The release is now scoped to the decode, on BOTH the success and the 400 arm; the
  failure arm has its own test, because a fix that guards only the happy path leaks on the other.
- `UploadAdmission` ran outermost, so a request whose DECLARED `Content-Length` already exceeded
  `MaxRequestBodyBytes` still had to buy a permit first — and `UploadAdmissionWeightFor` prices
  anything past the amplification threshold at the full worst-case weight, which equals the whole
  budget. Whenever any other upload held a permit, a plainly-oversized request waited the shed
  timeout and got a retryable 503 telling it to retry something that could never succeed. It now
  answers 413 from the headers alone, before seeking a permit. This does not weaken the pre-auth
  placement: the check reads a header already parsed and consumes no body, and undeclared/chunked
  bodies are deliberately still routed through admission to `MaxBodyBytes`' reader arm, which is
  the only place their size becomes knowable.

The two budgets remain SEPARATE and neither subsumes the other: `UploadAdmission` prices wire
bytes from `Content-Length`, `DecodeReserver` prices decoded pixels from the header. A flat
4000x4000 PNG is ~68 KiB on the wire and 61 MiB decoded — 916x — so collapsing them would restore
the hole either one was added to close. Three doc sites that had drifted into claiming the wire
permit covers pixels were corrected for the same reason: a bound described as redundant gets
deleted. Process-wide composition of the two per-request bounds is tracked separately in #179 and
deliberately out of scope here.

Two of the round's findings were already fixed and were verified STALE by mutation rather than by
reading: transposing `ProjectID`/`BriefID` in `creativeAssetResult`, and dropping the repo-error
check so a nil `stored` mapped to a result — both are now caught.
