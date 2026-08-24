# 2026-08-24 — a number is stale only where it names the wrong quantity

**Docs** — creative-asset upload: the round that moved the wire schema from `MaxLength(31457280)`
to `MaxLength(41943040)` left a trail of comments describing the superseded declaration as
current. Review on PR #170 listed six sites. The tempting repair — replace `31457280` with
`41943040`, and `30 MiB` with `40 MiB`, everywhere — would have been wrong at more than half of
them, because two different real quantities share those numbers.

## The two quantities

- `maxCreativeStoredBytes` = `31457280` — the **decoded** stored-file ceiling, 30 MiB, enforced in
  the handler at Stage 0 (`internal/service/creative_asset.go`), sized at Meta's documented
  single-image maximum.
- `MaxLength(41943040)` — the **base64-encoded** ceiling on the wire schema, exactly
  `base64.StdEncoding.EncodedLen(maxCreativeStoredBytes)`. OpenAPI `maxLength` counts characters
  of the JSON string, so this is the only unit that schema can express the bound in.

The original defect was not that `31457280` was the wrong number. It was that a **decoded** number
was applied to an **encoded** string. So a comment saying "30 MiB" is correct wherever it describes
the decoded limit and wrong only where it attributes that figure to the wire schema. Classifying
each site by the quantity it names, rather than by the digits it contains, is the whole of the fix.

## What each site turned out to be

Wire-stale — attributed the 30 MiB decoded figure to the published schema, and were corrected to
name the encoded ceiling while re-attributing the 30 MiB bound to the handler:
`pkg/constants/http.go:25`, `internal/middleware/upload_admission.go:22`,
`internal/service/creative_asset.go:72`, `cmd/campaign-service/body_limit_wiring_test.go:55`,
`docs/knowledge/code/design.md:106`.

Decoded-correct — left alone: `internal/service/creative_asset.go:273`
(`maxCreativeStoredBytes = 31457280 // 30 MiB`) is the definition of the decoded ceiling itself.

Mixed — the VALUE is decoded-correct but its LABEL was wire-stale, so only the label changed:
`cmd/campaign-service/body_limit_wiring_test.go:156` (`const maxImage = 31457280`, which the test
then base64-encodes to build the body — the value must stay 30 MiB or the test stops sizing the
worst legal body) and `pkg/constants/http_test.go:159-160`, whose 30 MiB figure is one half of the
coexisting-peak sum and is measured in decoded bytes, not characters.

Two sites beyond the six the review listed were found by sweeping `31457280`, `30 MiB` and
`MaxLength` across the tree rather than visiting the named lines: `pkg/constants/http_test.go`
above, and `docs/knowledge/code/internal-middleware.md` (lines 17-18 and 39), which both described
the declared ceiling as the 30 MiB decoded one.

## A comment that named a symbol that did not exist

`design/brief.go:715` pointed a reader at `maxCreativeEncodedBytes` for where the decoded bound is
enforced. `git grep` finds that identifier at exactly one place in the tree: that comment. It is
defined nowhere. The two real symbols are `maxCreativeStoredBytes` and `maxCreativeDecodedBytes`,
and the citation now names both with what each bounds. This is the second dangling-symbol citation
found on this stack, and both were caught the same way — grepping the name instead of reading past
it. A symbol name in a comment is a claim, and it is one of the cheapest claims to check.

## The undeclared-length upload serializes, and that is now written down

`UploadAdmissionWeightFor(-1)` charges the full ceiling, which equals the whole budget, so a
chunked or undeclared upload runs one at a time. The pricing rationale was already documented; the
consequence was not, and the consequence is the part a reader meets when concurrency is 1 in
practice. goa's generated `RequestEncoder` never sets `ContentLength`, so the shipped generated
client lands on exactly this branch; the real BFF caller declares a length and is priced by size.
Charging the undeclared case less would make omitting `Content-Length` the cheapest way to buy a
permit, so the ceiling stays and the encoder-side fix is tracked as issue #183. Documented at the
function, where the surprising behaviour is.
