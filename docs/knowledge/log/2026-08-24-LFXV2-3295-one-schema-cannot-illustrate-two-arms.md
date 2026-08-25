# 2026-08-24 — one shared schema cannot illustrate two response arms

**Fix** — the published OpenAPI showed the upload's `created` discriminator **backwards**:
`201` with `created: "false"` and `200` with `created: "true"`, the exact inverse of the mapping
the endpoint applies.

The runtime was never wrong. `Response(StatusCreated, Tag("created", "true"))` plus a default
`Response(StatusOK)` drives selection correctly, and that is pinned at the service, encoder and
wire layers. Only the *document* lied.

## Why it happened, and why the obvious fix does not exist

Both success arms `$ref` the same `CreativeAsset` schema:

```json
"200": {"schema": {"$ref": "#/components/schemas/CreativeAsset"}}
"201": {"schema": {"$ref": "#/components/schemas/CreativeAsset"}}
```

So the document carries **one** example for both arms — there is no per-arm example to render.
With `Enum("true", "false")` and no `Example`, Goa picks an enum member per rendering, and the two
renderings disagreed. A single shared example cannot be correct for both arms, so the only choice
is *which* arm it illustrates.

Pinned to `Example("true")`: the `201` arm is the one the `Tag` selects on, and `200` is the
default. Now both arms render `"true"` consistently. That is still not a per-arm illustration —
it cannot be — but a consistent example that matches the tagged arm is strictly better than one
that inverts the mapping, which is worse than no example at all because it reads as authoritative.

## The other document defect on the same attribute — TRUE, and not fixable here

Goa emits the `bytes` attribute as `type: string, format: binary` while the transport carries a
**base64** string (`encoding/json` marshals a Go `[]byte` that way, and the generated example is
base64: `"amQ="`). In OAS3 `format: binary` means raw octets, so a strict generator can build a
client that sends something this server will not decode.

It is unconditional in the generator, not a missing DSL option —
`goa v3.25.3 http/codegen/openapi/v3/types.go:179-180`:

```go
s.Type = openapi.Type("string")
s.Format = "binary"
```

for every `Bytes` attribute, with no DSL or `Meta` override. Escaping it means declaring the
attribute `String` and hand-rolling the base64 decode, giving up the generated validator and the
typed payload — a worse trade than a wrong format annotation on one field. Recorded in
`design/brief.go` as a known document defect with the generator line cited, so the next reader
does not re-derive it or "fix" it by weakening the type.

A related claim in the same review — that the generated examples are *arrays*, which would be
invalid for a string schema — is **false** here: every `bytes` example in both generated
documents is a base64 string. Checked by scanning `gen/http/openapi3.json` and
`gen/http/openapi.json` for array-valued `bytes` examples; there are none.

## The rule

**A shared schema referenced from two arms has one example, and an example that contradicts the
discriminator is worse than none.** When two responses differ only by a tagged field, either pin
the example to the tagged arm or omit it — do not leave the generator to choose per rendering,
because the choice is not stable and half of the renderings will be wrong.
