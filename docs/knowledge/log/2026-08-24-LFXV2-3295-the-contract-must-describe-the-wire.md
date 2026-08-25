# 2026-08-24 — the published contract must describe the wire it actually carries

**Fix** — the upload's `bytes` attribute is declared `String` instead of Goa `Bytes`, so the
OpenAPI document stops claiming `format: binary` for a field that is unavoidably a base64 string
in an `application/json` body.

`format: binary` in OAS3 means RAW OCTETS. A strict generator reading the old document built a
client that sent raw bytes this server cannot decode — on a NEW public endpoint, whose committed
contract should not knowingly describe a different wire representation than the one it accepts.

## Why the field type had to change

It is unconditional in the generator, not a missing option: `goa v3.25.3`
`http/codegen/openapi/v3/types.go:179-180` sets `s.Format = "binary"` for **every** `Bytes`
attribute. Two escapes were checked and neither exists:

- `Format(...)` is validated against a fixed whitelist (`expr.IsSupportedValidationFormat`,
  `attribute.go:999`) — date, uuid, email, ip, uri, … and **no byte/base64 member**.
- `openapi:extension:` meta can only add `x-` prefixed keys, never `format`.

So the type changes rather than the generator. The published schema is now
`type: string`, `minLength: 1`, `maxLength: 41943040`, with a description and a base64 example —
verified by **rendering `kodata/openapi3.json` and reading it**, not by assuming the change
propagated. No `format: byte` either: goa cannot emit it. A plain string is nonetheless HONEST
where `format: binary` was WRONG — an unformatted string tells a generator "a string, see the
description" and it passes the value through, whereas `format: binary` actively instructs it to
send octets.

The request media type is unchanged: still `application/json`, not multipart.

## The unit trap this had to avoid

`MaxLength` on a **string** counts CHARACTERS; on a `Bytes` attribute goa applied it to the
DECODED slice. Same literal, different quantity — and the number must not move:

| bound | unit | value |
| --- | --- | --- |
| design `MaxLength` | base64 characters | 41,943,040 |
| `maxCreativeStoredBytes` | decoded bytes | 31,457,280 |
| migration `000029` CHECK | decoded octets | 31,457,280 |

Base64 expands by exactly 4/3 with padding, so `EncodedLen(31457280) == 41943040` and the three
agree **by construction**. `TestUploadCreativeAsset_EncodedAndDecodedCeilingsAgree` asserts it
both directions rather than trusting the arithmetic. Getting it backwards would have narrowed the
accepted image to ~22.5 MiB or widened it to ~40 MiB.

Note what the type change actually FIXED here: as a `Bytes` attribute the generated validator
applied the *encoded* figure to the *decoded* slice — an effective ~40 MiB bound matching neither
the published character count nor the real ceiling. As a `String` it bounds the characters, which
is what the OpenAPI `maxLength` has always meant.

The decoded-size check moved with it: `len(p.Bytes)` on a string counts encoded characters, so
the 30 MiB ceiling is now applied to `len(decoded)`. Checking the string would have silently
bounded the wrong quantity — the same confusion in a new place.

## Malformed base64 is a 400, and the test that proves it

The decode now happens at the service boundary, so this method owns the answer for input that
cannot be decoded: a 400, never a panic and never a 500. Four cases — not base64 at all, an
illegal character, excess padding, and the URL-safe alphabet (the plausible client bug, since
base64url looks correct and decodes differently).

**The first version of that test was worthless and the mutation proved it.** Asserting only "some
400 came back" PASSED against a build with the base64 error check bypassed, because
`base64.DecodeString` returns partially decoded bytes ALONGSIDE its error: those bytes fell
through to the image decode, which answers 400 for its own reason. The test now asserts the
SPECIFIC message, and the same mutation turns it red. A status code shared by four stages
identifies none of them.

One fixture was wrong too: the excess-padding case was derived by trimming `=` off an encoded
value, which only yields an invalid string when the input length needs padding — it silently
depended on the fixture's length and decoded cleanly. Replaced with the literal `"QUJD="`.
