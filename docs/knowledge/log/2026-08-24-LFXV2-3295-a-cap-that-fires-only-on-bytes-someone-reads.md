# 2026-08-24 — LFXV2-3295 a cap that fires only on bytes someone reads

**Verification** — a review thread reported that `MaxBodyBytes` can be bypassed by a valid JSON
envelope followed by unbounded trailing bytes. The mechanism is real and the conclusion is not:
measured, the untouched bytes are never consumed, and the byte bound holds.

**The claim, stated fairly.** `MaxBodyBytes`' second arm fires at the READ, via
`http.MaxBytesReader`. `json.Decoder.Decode` stops at the end of the first complete JSON value.
So a caller can send a small valid envelope followed by megabytes of trailing whitespace: the
decoder never reads far enough to trip the reader, `tracked.exceeded()` stays false, and the
request is answered 200. An over-cap transmission did not fire the cap. That much reproduces
exactly as described.

**Measured, rather than argued.** Against a 4 KiB cap, over a real `net/http` server with a
byte-counting listener:

| trailing sent | bytes read off socket | status | `exceeded()` |
|---|---|---|---|
| 1 MiB | 295,170 | 200 | false |
| 8 MiB | 528,698 | 200 | false |
| 64 MiB | 409,888 | 200 | false |
| 512 MiB | 528,696 | 200 | false |

Consumption is FLAT while the payload grows 512x. Peak heap during a 200 MiB trailing send was
383 KiB against a 349 KiB baseline — a 34 KiB delta. On a raw socket the client's writes fail
with `broken pipe` after ~1.8 MiB: the server answers 200 and closes rather than draining.

**Verdict (b): harmless, and the reason is the difference between SENT and CONSUMED.** The bound
this middleware provides is on bytes read into the process. Bytes the decoder never requests stay
in the kernel receive buffer and are discarded with the connection; nothing in this process
allocates them. What flat consumption under a 512x payload growth shows is that the numbers above
are socket-buffer artifacts, not a leak that scales.

**The shape that would NOT have been harmless was checked too**, because "the cap is conditional
on the read" is only reassuring if nothing dangerous is reachable through it. Oversized bytes
INSIDE the JSON value are the case that could actually persist an over-cap asset — the decoder
must read every one of them to produce the value. There the cap fires exactly as designed: a
base64 payload 128x the cap returned 413 with `http: request body too large`, zero bytes decoded,
nothing available to persist. The distinction is not "small reads escape" but "bytes nobody
wanted are never read".

**The floor-pricing rationale survives.** `constants.UploadAdmissionWeightFor` charges an
undeclared/chunked body the floor, and rests on the premise that under-declaring buys nothing
"because `MaxBodyBytes` bounds the real body regardless". That premise was the reason to check
this at all — if trailing bytes could enter the process unbounded, the premise would weaken. They
cannot: an undeclared request cannot get more bytes into the process by appending junk, because
the junk is not read, and the only shape that does get read is refused at the cap. No design
question for the ceiling-vs-floor decision arises from this thread.

**Both cases are now pinned by tests**, because the distinction is the kind a later reader is
likely to re-report as a bypass:
`TestMaxBodyBytes_TrailingBytesAfterAValidValueAreNeverConsumed` asserts that consumption does
not scale with the payload (the load-bearing property — a genuinely bypassed middleware would
read the trailing bytes), and `TestMaxBodyBytes_OversizeInsideTheValueStillTrips413` asserts the
413 on the shape that matters for storage. The latter is mutation-verified: removing
`http.MaxBytesReader` from `MaxBodyBytes` compiles and turns it red at `status = 200, want 413`.

**A coverage claim corrected in the same pass.** `cmd/campaign-service`'s
`TestUploadAdmission_IsWiredIntoTheRealChain` derives its saturation count from
`constants.UploadAdmissionWeightFor(-1)`, so reverting the unknown-length floor to the worst-case
ceiling merely moves it from parking 16 requests to parking 1 — and it stays GREEN (confirmed by
mutation). That is correct for a wiring proof, which must not break when pricing constants are
retuned, but it means no failure there can be read as evidence about what an upload is CHARGED.
The pricing rule is bound non-derivably by `TestUploadAdmissionWeightFor_PricesWithoutUnderCharging`
and `TestUploadAdmission_UndeclaredLengthUploadsRunConcurrently` in `internal/middleware`, which
compare against literal weights and DO fail under that mutation. The wiring test now says so at
the derivation, where a reader meets it.
