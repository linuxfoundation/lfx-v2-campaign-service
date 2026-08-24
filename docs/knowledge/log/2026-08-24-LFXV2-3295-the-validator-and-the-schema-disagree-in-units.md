# 2026-08-24 — the validator and the schema share a number but not a unit

**Docs** — `docs/knowledge/code/internal-service.md` said the generated validator enforces "the
base64 string is within `[1, 41,943,040]` CHARACTERS". Half right, and wrong in the exact way the
paragraph beneath it exists to warn about.

`41943040` appears in two places and means a different quantity in each:

| where | compares | unit |
| --- | --- | --- |
| published OpenAPI schema | `maxLength` on a JSON string | base64 **characters** |
| generated server validator | `len(body.Bytes)`, and `Bytes` is `[]byte` | **decoded** bytes |

`gen/http/lfx_v2_campaign_service_briefs/server/types.go`:

```go
if len(body.Bytes) > 41943040 {
```

`body.Bytes` is `[]byte` — populated by `encoding/json`, which base64-decodes a `[]byte` field.
The validator therefore runs *after* decoding and bounds the decoded slice at ~40 MiB, not the
string at 41,943,040 characters.

## Why this particular sentence mattered

The section it introduces explains that declaring the DECODED 30 MiB as `MaxLength` published a
schema rejecting uploads at ~22.5 MiB, and that "server and schema agreed only because the
generated validator applied the same number to the decoded slice, so both were wrong together and
nothing local disagreed." That is the correct account — and the summary line above it described the
validator as enforcing a CHARACTER bound, which is the very conflation the account is unpicking. A
reader who stopped at the summary took away the opposite of the lesson.

The consequence is not cosmetic. Because the validator counts decoded bytes against the *encoded*
figure, it does not enforce the real stored-file ceiling at all: it admits up to ~40 MiB decoded.
The 30 MiB bound is enforced only by the handler (`maxCreativeStoredBytes`, stage 0). Anyone who
believed the validator already applied the true ceiling could delete that handler check as
redundant and widen the accepted size by a third.

## The rule

A number shared between a published schema and generated code is **two claims, not one**, whenever
the two sides measure different representations. State the unit at every site, and when the same
literal appears on both sides of a decode boundary, say which side each one is on.

Swept the surface for the same conflation: `internal-middleware.md:18` already said the
`MaxLength` is "checked by the GENERATED VALIDATOR against the already-decoded slice" and needed
no change, so `internal-service.md` was the only stale site.
