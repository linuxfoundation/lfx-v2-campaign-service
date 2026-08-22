# 2026-08-22 — the worst legal input is the only one that prices a cap

**Fix** — three defects in the upload body-cap work, all of the same shape: a claim that was
true of the case chosen rather than of the case that matters.

## The fixture could not fail on the worst case

`TestUploadRoute_AdmitsMaximumLegalUpload` built the largest legal upload with
`content_type: "image/png"`. But `content_type` rides IN the body, so its length counts toward
the size being tested, and `design/brief.go`'s `Enum` admits `"image/jpeg"` too — one character
longer. The worst legal body is therefore 41,943,080 bytes, not the 41,943,079 the PNG fixture
produced.

The consequence was a cap that could be wrong by exactly one byte with the suite still green:
setting `MaxRequestBodyBytes` to 41,943,079 passes a PNG-only test while refusing every
maximum-size JPEG the contract accepts. This was confirmed rather than reasoned about — with the
constant mutated to 41,943,079 the PNG subtest PASSES and the JPEG subtest FAILS. The test now
drives both enum values and pins each exact wire size, so the envelope changing is visible here
rather than in production.

The general shape is worth naming: a test that admits the maximum is only as good as its notion
of maximum. When any part of the measured thing is itself variable, the fixture has to take the
variable at its worst, not at its first.

## The catalog promised a property only one arm delivers

`docs/api-catalog.md` said an oversize body is "refused with 413 before being read". That is true
of the `Content-Length` arm, which refuses without touching the body. It is false of the other:
an undeclared/chunked body cannot be measured up front, so it is wrapped in
`http.MaxBytesReader`, the overflow is discovered only by reading up to the cap, the decoder
fails with a generic 400 about malformed JSON, and the middleware then REWRITES that into the
413. Both arms answer 413 and neither buffers past the cap — but only one avoids reading. The
row now states the property per arm instead of generalising the stronger one.

## One number, three places, two values

`pkg/constants/http.go` had the worst case right (41,943,080, derived from the JPEG envelope);
`docs/knowledge/code/internal-middleware.md` said 41,943,079 and attributed it to a 39-byte
envelope. The concept doc is corrected and now names the test that holds the number down.

The same class was swept — not just the cited line — through `internal/service/creative_asset_test.go`,
whose explanatory arithmetic still described a superseded 160-MiB budget. The real one is
`maxCreativeDecodedBytes = 80 << 20`. Three stale claims: a duplicated comment block describing
an `8000x5000` case the table does not contain and calling 160 MiB "admitted" (at an 80-MiB
budget it is refused), and two references to a "256 MiB" buffer and a "160 MiB budget" for a
4000x4000 16-bit image that actually costs 122 MiB against 80. Every VERDICT in the table was
correct; only the explanations were wrong, which is the dangerous version — the tests pass, so
nothing forces the arithmetic to be re-checked, and the next person sizing a budget reads the
comment rather than recomputing it.

`internal/service/creative_asset.go:182`'s "~160 MiB" was checked and LEFT: it describes what
the OLD 20M-pixel bound really permitted (20M x 8 = 152.6 MiB), and is a correct historical
statement rather than a stale one.

## The rule

An explanatory number in a comment is a claim the test suite does not check. When a budget or a
cap moves, the verdicts get updated because they fail; the arithmetic beside them does not, and
survives as confident, wrong documentation. Sweep the FILE for the superseded figure rather than
fixing the line a review happened to cite.
